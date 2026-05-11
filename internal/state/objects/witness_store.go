package objects

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
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
	// db is the backing Database; consulted only for db.UndoEnabled in
	// Mutate / Put to decide whether to capture an undo snapshot.
	db *state.Database

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
	// dirty is the set of owners whose secondary entries need
	// reconciliation. Implemented as a map for O(1) dedup but with
	// a backing slice (dirtyOrder) for cache-friendly iteration in
	// flushSecondaries — the map lookups in the per-Mutate hot path
	// are 5%+ of CPU, while iter-via-slice is essentially free.
	// Both are kept in sync: an owner enters via dirtyMark, exits
	// only via clearDirty (called by flushSecondaries).
	// dirty / dirtyOrder are keyed by *Witness (the stable slot pointer
	// preserved across in-place Mutate). Pointer-keyed maps hash a
	// single uint64 — kills the string-hash cost markDirty paid on
	// every witness-vote propagation (1.07 GB of map ops at 4M / 12% of
	// mapaccess2_faststr CPU).
	// Dirty bookkeeping: dirtyOrder is the in-flush queue; dedup is
	// done via a per-Witness epoch counter (Witness.dirtyEpoch ==
	// s.dirtyEpoch ⇒ already in dirtyOrder for this round). On
	// flushSecondaries, s.dirtyEpoch bumps so prior epoch values
	// become "stale" without any mapclear pass.
	//
	// Replaces the prior pointer-keyed map (mapaccess2_fast64 +
	// mapclear cost ~1.5s on the 4M run). Restore-undo path may
	// re-introduce a stale dirtyEpoch (snap captures pre-mutation
	// state including the field) — accepted, the duplicate
	// dirtyOrder entry is idempotent for flushSecondaries.
	dirtyOrder []*Witness
	dirtyEpoch uint32

	// freeWitness is a freelist of *Witness slots reclaimed at
	// CommitPoint / RestoreToPoint. Put pulls from here before
	// allocating a fresh struct, eliminating the per-Put heap
	// allocation that dominated the WitnessStore alloc profile
	// (1GB / 1M blocks). Single-threaded replay only — no lock.
	freeWitness []*Witness

	// flushHints memoize the btree path hint per secondary index.
	// flushSecondaries does ~30 paired Delete+Set ops per producer-
	// reward block; reusing a per-tree hint cuts each op's tree walk
	// to a hint-confirmed leaf insert/delete (12% saving in the mid-
	// chain profile where flushSecondaries is 14.5% of CPU).
	hintByVote btree.PathHint
	hintByVST  btree.PathHint
	hintByPow  btree.PathHint
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

// witnessSecKey is the secondary-index sort key. `sort` is a fixed
// 20-byte buffer that fits all three index encodings (8 bytes for vote,
// 12 for pow, 20 for VST+created). Using a fixed array instead of a
// string eliminates the `string(buf[:])` heap allocation that fired on
// every flushSecondaries Delete/Set call — ~500 MB cum across the 4M-
// block run. Shorter encodings zero-pad the trailing bytes; that's
// consistent across the inserts and the lookup probes so ordering
// stays correct.
type witnessSecKey struct {
	sort  [20]byte
	owner string
}

type witnessUndo struct {
	owner string
	// hadPrev=false means the row was a fresh insert; Restore deletes it.
	// hadPrev=true with snap captured means the row was overwritten or
	// mutated in place; Restore copies snap back over *primary[owner].
	hadPrev bool
	// slot is the *Witness in primary at mutation time. For insert-undo
	// (hadPrev=false) Restore frees this slot back to freeWitness.
	slot *Witness
	snap Witness // pre-mutation value when hadPrev=true; zero when !hadPrev
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
	ws.db = db
	db.RegisterSubsystem(witnessStoreSubsystemKey, ws, ws)
	return ws
}

func newWitnessStore() *WitnessStore {
	less := func(a, b witnessSecKey) bool {
		cmp := bytes.Compare(a.sort[:], b.sort[:])
		if cmp != 0 {
			return cmp < 0
		}
		return a.owner < b.owner
	}
	// Degree=64: the witness secondary indices peak around ~3K entries
	// post-PoW era. At depth = log_64(3K) ≈ 1.85, every Set/Delete
	// traverses 2 levels max — vs depth ~3 with the previous Degree=16.
	// The flushSecondaries hot path does ~30 paired Delete+Set ops per
	// round; cutting tree levels by 33% maps directly to CPU since
	// each level is a cache miss + bounds-check + binary search.
	const witnessIndexDegree = 64
	return &WitnessStore{
		primary:    make(map[string]*Witness, 1024),
		byVote:     btree.NewBTreeGOptions[witnessSecKey](less, btree.Options{Degree: witnessIndexDegree, NoLocks: true}),
		byVST:      btree.NewBTreeGOptions[witnessSecKey](less, btree.Options{Degree: witnessIndexDegree, NoLocks: true}),
		byPow:      btree.NewBTreeGOptions[witnessSecKey](less, btree.Options{Degree: witnessIndexDegree, NoLocks: true}),
		undoLog:    make([]witnessUndo, 0, 64),
		flushed:    make(map[string]witnessSecondaryKey, 64),
		dirtyOrder: make([]*Witness, 0, 32),
		dirtyEpoch: 1, // 0 reserved for "fresh slot, never marked"
	}
}

// markDirty adds w to the dirty set, deduping via Witness.dirtyEpoch
// (matches current store epoch ⇒ already in dirtyOrder).
func (s *WitnessStore) markDirty(w *Witness) {
	if w.dirtyEpoch == s.dirtyEpoch {
		return
	}
	w.dirtyEpoch = s.dirtyEpoch
	s.dirtyOrder = append(s.dirtyOrder, w)
}

// SnapshotPoint returns the current undo-log length — passed to
// RestoreToPoint to roll back any Puts made since.
func (s *WitnessStore) SnapshotPoint() int { return len(s.undoLog) }

// RestoreToPoint replays the undo log in reverse, restoring the store
// to the state it was in at the moment SnapshotPoint() returned `snap`.
//
// Mutation/overwrite undo (hadPrev=true): copy `snap` back over
// *primary[owner] — preserves pointer identity so any held *Witness
// references remain valid.
// Insert undo (hadPrev=false): delete the row from primary and return
// the slot to the freelist.
func (s *WitnessStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := &s.undoLog[i]
		if e.hadPrev {
			if cur, ok := s.primary[e.owner]; ok {
				*cur = e.snap
				s.markDirty(cur)
			}
		} else {
			if cur, ok := s.primary[e.owner]; ok {
				s.freeWitness = append(s.freeWitness, cur)
				delete(s.primary, e.owner)
				// Slot was inserted by the rolled-back tx and is
				// going away; dirtyOrder may still reference the
				// pointer — flushSecondaries tolerates a missing
				// primary entry (drops secondaries from flushed if
				// any, no-ops otherwise). Clearing the witness's
				// dirtyEpoch prevents a future re-mark from happening
				// "for free" via the epoch match on the same slot
				// when it's pulled from the freelist later.
				cur.dirtyEpoch = 0
			}
		}
		e.slot = nil
	}
	s.undoLog = s.undoLog[:snap]
}

// CommitPoint releases accumulated undo entries — called from
// Session.Commit. Mutate/overwrite entries' snap is value-type, so
// dropping the entry is enough. Insert entries are now permanent (the
// slot stays in primary).
func (s *WitnessStore) CommitPoint() {
	for i := range s.undoLog {
		s.undoLog[i].slot = nil
	}
	s.undoLog = s.undoLog[:0]
	// Cap the freelist to keep idle memory bounded.
	if cap(s.freeWitness) > 1024 && len(s.freeWitness) > 1024 {
		s.freeWitness = s.freeWitness[:1024]
	}
}

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

// Mutate fetches the live *Witness for owner and calls fn to apply
// in-place mutations. Captures a value snapshot for undo; rolls back
// by copying the snapshot back over the in-place pointer.
//
// Compared to Get+Put-style: this path performs ONE 700-byte struct
// copy (into the undoLog snapshot) instead of TWO (Put copies the
// caller's value into a fresh slot AND copies the pointer back).
// AdjustWitnessVotesForVoter fires ~30 mutations per producer-reward
// block, ~120M mutations / 4M-block run → halving the struct-copy
// traffic saves ~40 GB of memmove.
//
// Pointer identity is preserved: callers may keep *Witness aliases
// across Mutate calls. Returns false if owner has no row.
func (s *WitnessStore) Mutate(owner auth.AccountName, fn func(*Witness)) bool {
	name := string(owner)
	cur, ok := s.primary[name]
	if !ok {
		return false
	}
	// Skip the 700-byte snap copy when no savepoint is outstanding.
	// Per-block hooks (processProducerReward, schedule rebuild, cashout)
	// run at depth=0 and never roll back; capturing a snapshot per
	// witness mutation in that window was ~84 GB of memcpy across a
	// 4M-block run.
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, witnessUndo{
			owner: name, hadPrev: true, slot: cur, snap: *cur,
		})
	}
	fn(cur)
	s.markDirty(cur)
	return true
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
//
// Allocation: pulls a *Witness slot from the freelist if available
// (refilled by CommitPoint / RestoreToPoint when prev pointers
// become unreachable); otherwise allocates a fresh struct. This
// avoids the per-Put heap allocation in the hot AdjustWitnessVotes /
// schedule-update loops.
func (s *WitnessStore) Put(w *Witness) {
	name := string(w.Owner)
	if cur, ok := s.primary[name]; ok {
		// Overwrite path: capture snap, mutate slot in place. Preserves
		// pointer identity. Skip the snap when no savepoint is outstanding.
		if s.db.UndoEnabled {
			s.undoLog = append(s.undoLog, witnessUndo{
				owner: name, hadPrev: true, slot: cur, snap: *cur,
			})
		}
		*cur = *w
		s.markDirty(cur)
		return
	}
	// Insert path: pull from freelist or allocate fresh.
	var cp *Witness
	if n := len(s.freeWitness); n > 0 {
		cp = s.freeWitness[n-1]
		s.freeWitness = s.freeWitness[:n-1]
	} else {
		cp = new(Witness)
	}
	*cp = *w
	s.primary[name] = cp
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, witnessUndo{
			owner: name, hadPrev: false, slot: cp,
		})
	}
	s.markDirty(cp)
}

// flushSecondaries reconciles the byVote/byVST/byPow btrees against
// the current primary state for every dirty owner. Called by each
// RangeBy* method as a one-shot before iteration. Idempotent and
// cheap when nothing is dirty.
func (s *WitnessStore) flushSecondaries() {
	if len(s.dirtyOrder) == 0 {
		return
	}
	hVote, hVST, hPow := &s.hintByVote, &s.hintByVST, &s.hintByPow
	for _, w := range s.dirtyOrder {
		// Skip stale entries dropped by an Insert-undo
		// (dirtyEpoch was cleared in RestoreToPoint to indicate
		// "this pointer is no longer in primary; flush is a no-op").
		// Note: dirtyEpoch may also legitimately differ from the
		// current epoch if a snap-restore happened mid-round and
		// left the field at a prior epoch — flush is still correct
		// because we read the live state via s.primary[owner].
		if w.dirtyEpoch != s.dirtyEpoch {
			continue
		}
		owner := string(w.Owner)
		cur := s.primary[owner]
		old, hadOld := s.flushed[owner]
		if cur == nil {
			// Witness was created and then deleted/undone; drop btree
			// entries keyed on the old snapshot if any.
			if hadOld {
				s.byVote.DeleteHint(witnessByVoteSecKey(old.votes, owner), hVote)
				if !virtualTimeIsZeroFlat(old.vst) {
					s.byVST.DeleteHint(witnessByVSTSecKey(old.vst, old.created, owner), hVST)
				}
				if old.pow != 0 {
					s.byPow.DeleteHint(witnessByPowSecKey(old.pow, old.created, owner), hPow)
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
				s.byVote.DeleteHint(witnessByVoteSecKey(old.votes, owner), hVote)
				s.byVote.SetHint(witnessByVoteSecKey(newK.votes, owner), hVote)
			}
			if old.vst != newK.vst {
				if !virtualTimeIsZeroFlat(old.vst) {
					s.byVST.DeleteHint(witnessByVSTSecKey(old.vst, old.created, owner), hVST)
				}
				if !virtualTimeIsZeroFlat(newK.vst) {
					s.byVST.SetHint(witnessByVSTSecKey(newK.vst, newK.created, owner), hVST)
				}
			}
			if old.pow != newK.pow {
				if old.pow != 0 {
					s.byPow.DeleteHint(witnessByPowSecKey(old.pow, old.created, owner), hPow)
				}
				if newK.pow != 0 {
					s.byPow.SetHint(witnessByPowSecKey(newK.pow, newK.created, owner), hPow)
				}
			}
		} else {
			s.byVote.SetHint(witnessByVoteSecKey(newK.votes, owner), hVote)
			if !virtualTimeIsZeroFlat(newK.vst) {
				s.byVST.SetHint(witnessByVSTSecKey(newK.vst, newK.created, owner), hVST)
			}
			if newK.pow != 0 {
				s.byPow.SetHint(witnessByPowSecKey(newK.pow, newK.created, owner), hPow)
			}
		}
		s.flushed[owner] = newK
	}
	// Bump epoch so all current dirtyEpoch tags become stale; truncate
	// dirtyOrder. No mapclear pass needed. Reserve 0 as the "never
	// marked" sentinel so freshly inserted Witnesses default to
	// not-dirty even when allocated via new(Witness).
	s.dirtyEpoch++
	if s.dirtyEpoch == 0 {
		s.dirtyEpoch = 1
	}
	s.dirtyOrder = s.dirtyOrder[:0]
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
// puts higher-vote witnesses first under ascending byte order. Remaining
// bytes of the 20-byte sort buffer are zero (consistent across all
// vote-index entries so byte ordering is unaffected).
func witnessByVoteSecKey(votes int64, owner string) witnessSecKey {
	v := uint64(votes)
	if votes < 0 {
		v = 0
	}
	var k witnessSecKey
	binary.BigEndian.PutUint64(k.sort[0:8], ^v)
	k.owner = owner
	return k
}

// witnessByVSTSecKey encodes (vst_be16, created_be4) — VST is already
// big-endian, and creation block_time follows so witnesses with equal
// VST sort by creation order (mirrors hived's composite_key<vst, witness_id>
// tie-break). Owner falls through as the secondary tiebreak only for
// the (rare) two-witnesses-created-in-the-same-block case.
func witnessByVSTSecKey(vst [16]byte, created uint32, owner string) witnessSecKey {
	var k witnessSecKey
	copy(k.sort[:16], vst[:])
	binary.BigEndian.PutUint32(k.sort[16:20], created)
	k.owner = owner
	return k
}

// witnessByPowSecKey encodes (pow_be8, created_be4) — pow_worker first
// then creation block_time so ties break by witness_id (creation order),
// mirroring hived's by_pow composite_key<pow_worker, witness_id>.
// Trailing bytes of the 20-byte sort buffer are zero.
func witnessByPowSecKey(pow uint64, created uint32, owner string) witnessSecKey {
	var k witnessSecKey
	binary.BigEndian.PutUint64(k.sort[0:8], pow)
	binary.BigEndian.PutUint32(k.sort[8:12], created)
	k.owner = owner
	return k
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

// witnessStoreSnapshotMagic identifies a witness_store snapshot stream.
var witnessStoreSnapshotMagic = [4]byte{'W', 'S', 'T', 'R'}

const witnessStoreSnapshotVersion uint32 = 1

// MarshalSnapshot serializes the primary witness map to w. Format:
//
//	magic[4] | version_u32_le | count_u32_le |
//	(varuint(name_len) | name | varuint(rec_len) | AppendWitness(rec))*
//
// Names are emitted in ascending order so snapshots are byte-stable
// across runs. Secondary indexes are NOT serialized — they're rebuilt
// from the primary on UnmarshalSnapshot via flushSecondaries.
func (s *WitnessStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(witnessStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], witnessStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	owners := make([]string, 0, len(s.primary))
	for o := range s.primary {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	var (
		varBuf [binary.MaxVarintLen64]byte
		recBuf []byte
	)
	for _, o := range owners {
		wp := s.primary[o]
		// Owner name as length-prefixed string (witness key).
		n := binary.PutUvarint(varBuf[:], uint64(len(o)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, o); err != nil {
			return err
		}
		// Witness record. Reuse buffer.
		recBuf = AppendWitness(recBuf[:0], wp)
		n = binary.PutUvarint(varBuf[:], uint64(len(recBuf)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		if _, err := w.Write(recBuf); err != nil {
			return err
		}
	}
	return nil
}

// UnmarshalSnapshot replaces the store's primary map (and clears
// secondaries / undo log) from r. After loading, secondary indexes are
// marked dirty so the next RangeBy* rebuilds them lazily.
func (s *WitnessStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("witness_store: read magic: %w", err)
	}
	if magic != witnessStoreSnapshotMagic {
		return fmt.Errorf("witness_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("witness_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != witnessStoreSnapshotVersion {
		return fmt.Errorf("witness_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	// Reset state before loading.
	s.primary = make(map[string]*Witness, count)
	s.byVote.Clear()
	s.byVST.Clear()
	s.byPow.Clear()
	s.flushed = make(map[string]witnessSecondaryKey, count)
	s.dirtyOrder = make([]*Witness, 0, count)
	s.dirtyEpoch = 1
	s.undoLog = s.undoLog[:0]
	for i := uint32(0); i < count; i++ {
		nameLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("witness_store: read name len[%d]: %w", i, err)
		}
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(br, nameBuf); err != nil {
			return fmt.Errorf("witness_store: read name[%d]: %w", i, err)
		}
		recLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("witness_store: read rec len[%d]: %w", i, err)
		}
		rec := make([]byte, recLen)
		if _, err := io.ReadFull(br, rec); err != nil {
			return fmt.Errorf("witness_store: read rec[%d]: %w", i, err)
		}
		wit, err := ReadWitness(rec)
		if err != nil {
			return fmt.Errorf("witness_store: decode rec[%d]: %w", i, err)
		}
		cp := wit
		owner := string(nameBuf)
		s.primary[owner] = &cp
		s.markDirty(&cp)
	}
	return nil
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
