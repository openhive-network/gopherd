package objects

import (
	"encoding/binary"
	"sort"

	"github.com/tidwall/btree"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// WitnessStore is the in-memory typed container for the Witness table.
// It replaces the generic K/V state path for witnesses: mutations
// happen in-place against the primary map and three secondary btrees,
// with no serialize → overlay copy → btree commit chain. The
// chainbase-equivalent design eliminates ~50% of the prior per-block
// CPU on the AdjustWitnessVotesForVoter hot path.
//
// Invariants
//
//   - primary[name] points to the canonical *Witness for `name`. Get
//     returns a value-copy so callers never mutate the stored record
//     except via Put.
//   - The three secondary btrees mirror the primary map: every primary
//     entry contributes one byVote key, optionally one byVST key
//     (skipped when VST is all-zero), and optionally one byPow key
//     (skipped when PowWorker is 0). Put keeps them in sync.
//   - undoLog is the chainbase-style "before image" stack. Each Put
//     appends one entry capturing the prior pointer for that owner;
//     RestoreToPoint walks in reverse and reinstates them.
type WitnessStore struct {
	primary map[string]*Witness
	byVote  *btree.BTreeG[witnessSecKey]
	byVST   *btree.BTreeG[witnessSecKey]
	byPow   *btree.BTreeG[witnessSecKey]
	undoLog []witnessUndo

	// Lazy secondary-index machinery — secondary btrees only need to
	// be consistent when a RangeBy* is about to read them. The only
	// production reader is updateWitnessSchedule's rebuild path which
	// fires every 21 blocks; in between, AdjustWitnessVotesForVoter
	// hits ~30 witnesses per produced block, so eager secondary updates
	// would do 21 × 30 × ~3 btree ops per round that the next rebuild
	// would re-do anyway. Deferring them collapses the work into one
	// delete+insert per witness per round.
	//
	// Invariant: for owner ∈ flushed, the byVote/byVST/byPow btrees
	// hold entries keyed on flushed[owner]'s (votes, vst, pow). Owners
	// in `dirty` may have primary[owner] values that differ from
	// flushed[owner]; flushSecondaries reconciles them.
	flushed map[string]witnessSecondaryKey
	dirty   map[string]struct{}
}

// witnessSecondaryKey snapshots the (votes, vst, pow, created) values
// that the secondary btrees are currently keyed on for one owner.
//
// `created` mirrors hived's by_schedule_time tie-break by witness_id
// (row insertion order). Two witnesses with equal VST in hived's
// composite index resolve by creation order; with Go's tie-break being
// owner-name ascending, witnesses created LATER (e.g., sminer1 in PoW
// phase) but lex-smaller than genesis ones (e.g., steemit16) reordered
// in front of them, advancing different witnesses' VSTs at every
// schedule rebuild. Block 3,207,456 is the first block where the
// resulting per-witness VST drift triggers a chain-state divergence.
type witnessSecondaryKey struct {
	votes   int64
	vst     [16]byte
	pow     uint64
	created uint32
}

// witnessSecKey is the secondary-index sort key. `sort` holds the
// pre-encoded sort prefix (8 bytes for votes/pow, 16 for VST) so the
// btree's Less is a pair of cheap string compares.
type witnessSecKey struct {
	sort  string
	owner string
}

type witnessUndo struct {
	owner string
	prev  *Witness // nil = the row didn't exist before this Put
}

const witnessStoreSubsystemKey = "witness_store"

// EnsureWitnessStore returns the database's WitnessStore, lazily
// creating + registering it on first call. Domain code calls this
// instead of building stores by hand so the snapshot/commit hooks are
// always wired.
func EnsureWitnessStore(s *state.Session) *WitnessStore {
	db := s.Database()
	if existing := db.GetSubsystem(witnessStoreSubsystemKey); existing != nil {
		if ws, ok := existing.(*WitnessStore); ok {
			return ws
		}
	}
	ws := newWitnessStore()
	db.RegisterSubsystem(witnessStoreSubsystemKey, ws, ws)
	return ws
}

func newWitnessStore() *WitnessStore {
	less := func(a, b witnessSecKey) bool {
		if a.sort != b.sort {
			return a.sort < b.sort
		}
		return a.owner < b.owner
	}
	return &WitnessStore{
		primary: make(map[string]*Witness, 1024),
		byVote:  btree.NewBTreeGOptions[witnessSecKey](less, btree.Options{Degree: 16, NoLocks: true}),
		byVST:   btree.NewBTreeGOptions[witnessSecKey](less, btree.Options{Degree: 16, NoLocks: true}),
		byPow:   btree.NewBTreeGOptions[witnessSecKey](less, btree.Options{Degree: 16, NoLocks: true}),
		undoLog: make([]witnessUndo, 0, 64),
		flushed: make(map[string]witnessSecondaryKey, 64),
		dirty:   make(map[string]struct{}, 32),
	}
}

// SnapshotPoint returns the current undo-log length — passed to
// RestoreToPoint to roll back any Puts made since.
func (s *WitnessStore) SnapshotPoint() int { return len(s.undoLog) }

// RestoreToPoint replays the undo log in reverse from the back until
// the log length equals `snap`, returning the store to the state it
// was in at the moment SnapshotPoint() returned `snap`. Reverted
// owners are re-marked dirty so the next RangeBy* re-reconciles
// secondary indexes against the restored primary state.
func (s *WitnessStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		if e.prev != nil {
			s.primary[e.owner] = e.prev
		} else {
			delete(s.primary, e.owner)
		}
		s.dirty[e.owner] = struct{}{}
	}
	s.undoLog = s.undoLog[:snap]
}

// CommitPoint releases accumulated undo entries — called from
// Session.Commit. After this point the prior images can no longer be
// restored.
func (s *WitnessStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Has reports whether owner has a witness row.
func (s *WitnessStore) Has(owner auth.AccountName) bool {
	_, ok := s.primary[string(owner)]
	return ok
}

// Get returns a value-copy of the witness for owner. The boolean is
// false if the row doesn't exist; the returned Witness in that case
// is the zero value.
func (s *WitnessStore) Get(owner auth.AccountName) (Witness, bool) {
	w, ok := s.primary[string(owner)]
	if !ok {
		return Witness{}, false
	}
	return *w, true
}

// Put inserts or replaces the witness keyed by w.Owner. The caller's
// `w` is copied — subsequent caller mutations don't affect the store.
// The previous pointer (if any) is captured in the undo log so a
// later RestoreToPoint can reinstate it; that means the Witness
// pointed to by the prior slot must NOT be mutated externally — it
// becomes the canonical "before image". Use Put again to update.
//
// Secondary-index updates are deferred — the owner is added to the
// dirty set, and the actual btree delete/insert happens in
// flushSecondaries (called by every RangeBy* method). This collapses
// the typical 21 puts/round per active witness into one delete+insert
// per round.
func (s *WitnessStore) Put(w *Witness) {
	name := string(w.Owner)
	prev := s.primary[name]
	s.undoLog = append(s.undoLog, witnessUndo{owner: name, prev: prev})

	cp := *w
	s.primary[name] = &cp
	s.dirty[name] = struct{}{}
}

// flushSecondaries reconciles the byVote/byVST/byPow btrees against
// the current primary state for every dirty owner. Called by each
// RangeBy* method as a one-shot before iteration. Idempotent and
// cheap when nothing is dirty.
func (s *WitnessStore) flushSecondaries() {
	if len(s.dirty) == 0 {
		return
	}
	for owner := range s.dirty {
		cur := s.primary[owner]
		old, hadOld := s.flushed[owner]
		if cur == nil {
			// Witness was created and then deleted/undone; drop btree
			// entries keyed on the old snapshot if any.
			if hadOld {
				s.byVote.Delete(witnessByVoteSecKey(old.votes, owner))
				if !virtualTimeIsZeroFlat(old.vst) {
					s.byVST.Delete(witnessByVSTSecKey(old.vst, old.created, owner))
				}
				if old.pow != 0 {
					s.byPow.Delete(witnessByPowSecKey(old.pow, old.created, owner))
				}
				delete(s.flushed, owner)
			}
			continue
		}
		newK := witnessSecondaryKey{
			votes:   cur.TotalVotesVests,
			vst:     cur.VirtualScheduledTime,
			pow:     cur.PowWorker,
			created: cur.Created,
		}
		if hadOld {
			if old.votes != newK.votes {
				s.byVote.Delete(witnessByVoteSecKey(old.votes, owner))
				s.byVote.Set(witnessByVoteSecKey(newK.votes, owner))
			}
			if old.vst != newK.vst {
				if !virtualTimeIsZeroFlat(old.vst) {
					s.byVST.Delete(witnessByVSTSecKey(old.vst, old.created, owner))
				}
				if !virtualTimeIsZeroFlat(newK.vst) {
					s.byVST.Set(witnessByVSTSecKey(newK.vst, newK.created, owner))
				}
			}
			if old.pow != newK.pow {
				if old.pow != 0 {
					s.byPow.Delete(witnessByPowSecKey(old.pow, old.created, owner))
				}
				if newK.pow != 0 {
					s.byPow.Set(witnessByPowSecKey(newK.pow, newK.created, owner))
				}
			}
		} else {
			s.byVote.Set(witnessByVoteSecKey(newK.votes, owner))
			if !virtualTimeIsZeroFlat(newK.vst) {
				s.byVST.Set(witnessByVSTSecKey(newK.vst, newK.created, owner))
			}
			if newK.pow != 0 {
				s.byPow.Set(witnessByPowSecKey(newK.pow, newK.created, owner))
			}
		}
		s.flushed[owner] = newK
	}
	clear(s.dirty)
}

func virtualTimeIsZeroFlat(v [16]byte) bool {
	for _, b := range v {
		if b != 0 {
			return false
		}
	}
	return true
}

// witnessByVoteSecKey encodes (votes_negated_be8, owner). votes_negated
// puts higher-vote witnesses first under ascending byte order.
func witnessByVoteSecKey(votes int64, owner string) witnessSecKey {
	v := uint64(votes)
	if votes < 0 {
		v = 0
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], ^v)
	return witnessSecKey{sort: string(buf[:]), owner: owner}
}

// witnessByVSTSecKey encodes (vst_be16, created_be4) — VST is already
// big-endian, and creation block_time follows so witnesses with equal
// VST sort by creation order (mirrors hived's composite_key<vst, witness_id>
// tie-break). Owner falls through as the secondary tiebreak only for
// the (rare) two-witnesses-created-in-the-same-block case.
func witnessByVSTSecKey(vst [16]byte, created uint32, owner string) witnessSecKey {
	var buf [20]byte
	copy(buf[:16], vst[:])
	binary.BigEndian.PutUint32(buf[16:], created)
	return witnessSecKey{sort: string(buf[:]), owner: owner}
}

// witnessByPowSecKey encodes (pow_be8, created_be4) — pow_worker first
// then creation block_time so ties break by witness_id (creation order),
// mirroring hived's by_pow composite_key<pow_worker, witness_id>.
func witnessByPowSecKey(pow uint64, created uint32, owner string) witnessSecKey {
	var buf [12]byte
	binary.BigEndian.PutUint64(buf[:8], pow)
	binary.BigEndian.PutUint32(buf[8:], created)
	return witnessSecKey{sort: string(buf[:]), owner: owner}
}

// RangePrimary calls fn for every witness in ascending owner order.
// Used by HF code that walks the whole table.
func (s *WitnessStore) RangePrimary(fn func(w *Witness) (bool, error)) error {
	owners := make([]string, 0, len(s.primary))
	for o := range s.primary {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	for _, o := range owners {
		ok, err := fn(s.primary[o])
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// RangeByVote iterates witnesses by total_votes_vests descending,
// owner ascending. Returning false from fn stops iteration.
func (s *WitnessStore) RangeByVote(fn func(w *Witness) (bool, error)) error {
	s.flushSecondaries()
	var stopErr error
	s.byVote.Scan(func(k witnessSecKey) bool {
		w := s.primary[k.owner]
		if w == nil {
			return true
		}
		ok, err := fn(w)
		if err != nil {
			stopErr = err
			return false
		}
		return ok
	})
	return stopErr
}

// RangeByScheduleTime iterates witnesses by VirtualScheduledTime
// ascending. Witnesses with VST==zero are excluded (they have no
// secondary entry). Returning false from fn stops iteration.
func (s *WitnessStore) RangeByScheduleTime(fn func(w *Witness) (bool, error)) error {
	s.flushSecondaries()
	var stopErr error
	s.byVST.Scan(func(k witnessSecKey) bool {
		w := s.primary[k.owner]
		if w == nil {
			return true
		}
		ok, err := fn(w)
		if err != nil {
			stopErr = err
			return false
		}
		return ok
	})
	return stopErr
}

// RangeByPow iterates witnesses by PowWorker ascending. Witnesses
// with PowWorker==0 are excluded. Returning false from fn stops
// iteration.
func (s *WitnessStore) RangeByPow(fn func(w *Witness) (bool, error)) error {
	s.flushSecondaries()
	var stopErr error
	s.byPow.Scan(func(k witnessSecKey) bool {
		w := s.primary[k.owner]
		if w == nil {
			return true
		}
		ok, err := fn(w)
		if err != nil {
			stopErr = err
			return false
		}
		return ok
	})
	return stopErr
}
