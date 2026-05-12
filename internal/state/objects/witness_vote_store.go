package objects

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// WitnessVoteStore is the typed in-memory container for the witness
// vote graph (account → set of witnesses they voted for). Replaces the
// generic K/V WitnessVoteTable so the per-block AdjustWitnessVotesForVoter
// path can iterate a producer's votes via direct map lookup instead of
// an overlay-merging Range over a 30-row prefix scan.
//
// The undo log captures per-edge mutations (Put or Delete with a
// before-image bool) so RestoreToPoint can roll back op-failure tx
// snapshots in lock-step with the K/V overlay.
type WitnessVoteStore struct {
	db *state.Database
	// votes[account] maps each witness `account` votes for to the
	// cached *Witness pointer. Nil pointer = pointer not yet resolved
	// (lazy fallback). Caching avoids a WitnessStore.primary map
	// lookup per witness in the hot AdjustWitnessVotesForVoter loop
	// (30 witnesses × every CreateVesting × every block past HF7).
	//
	// Pointer stability: witnesses are immortal in mainnet — Put never
	// reuses a slot for a different account name, and there is no
	// Delete API on WitnessStore. The only stale-pointer risk is an
	// Insert-undo that frees the slot back to the freelist; the
	// rolled-back tx's voted-for-newly-created-witness combination
	// rolls back the vote-edge too, so the stale pointer never
	// surfaces in steady state.
	votes map[string]map[string]*Witness

	undoLog []witnessVoteUndo
}

type witnessVoteUndo struct {
	account string
	witness string
	// before reports whether the (account, witness) edge existed
	// BEFORE this op. Restore reinstates it accordingly.
	before bool
}

const witnessVoteStoreSubsystemKey = "witness_vote_store"

// EnsureWitnessVoteStore returns the database's WitnessVoteStore,
// lazily creating + registering it on first call.
func EnsureWitnessVoteStore(s *state.Session) *WitnessVoteStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotWitnessVote); x != nil {
		return x.(*WitnessVoteStore)
	}
	if existing := db.GetSubsystem(witnessVoteStoreSubsystemKey); existing != nil {
		if vs, ok := existing.(*WitnessVoteStore); ok {
			db.SetHotSubsystem(hotSlotWitnessVote, vs)
			return vs
		}
	}
	vs := &WitnessVoteStore{
		db:      db,
		votes:   make(map[string]map[string]*Witness, 256),
		undoLog: make([]witnessVoteUndo, 0, 32),
	}
	db.RegisterSubsystem(witnessVoteStoreSubsystemKey, vs, vs)
	db.SetHotSubsystem(hotSlotWitnessVote, vs)
	return vs
}

func (s *WitnessVoteStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *WitnessVoteStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		set, ok := s.votes[e.account]
		if e.before {
			if !ok {
				set = make(map[string]*Witness, 4)
				s.votes[e.account] = set
			}
			set[e.witness] = nil // pointer rebound lazily on next read
		} else if ok {
			delete(set, e.witness)
			if len(set) == 0 {
				delete(s.votes, e.account)
			}
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *WitnessVoteStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Put records account → witness. The current edge state goes into the
// undo log so RestoreToPoint can revert it.
func (s *WitnessVoteStore) Put(account, witness auth.AccountName) {
	a, w := string(account), string(witness)
	set, ok := s.votes[a]
	if !ok {
		set = make(map[string]*Witness, 4)
		s.votes[a] = set
	}
	if s.db.UndoEnabled {
		_, exists := set[w]
		s.undoLog = append(s.undoLog, witnessVoteUndo{account: a, witness: w, before: exists})
	}
	if _, exists := set[w]; !exists {
		set[w] = nil // pointer resolved lazily on first read
	}
}

// Has reports whether account currently votes for witness.
func (s *WitnessVoteStore) Has(account, witness auth.AccountName) bool {
	set, ok := s.votes[string(account)]
	if !ok {
		return false
	}
	_, ok = set[string(witness)]
	return ok
}

// Delete removes the edge.
func (s *WitnessVoteStore) Delete(account, witness auth.AccountName) {
	a, w := string(account), string(witness)
	set, ok := s.votes[a]
	if !ok {
		if s.db.UndoEnabled {
			s.undoLog = append(s.undoLog, witnessVoteUndo{account: a, witness: w, before: false})
		}
		return
	}
	_, exists := set[w]
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, witnessVoteUndo{account: a, witness: w, before: exists})
	}
	if exists {
		delete(set, w)
		if len(set) == 0 {
			delete(s.votes, a)
		}
	}
}

// VotesFor returns the witnesses `account` currently votes for, in
// arbitrary (map-iteration) order. Returns a fresh slice; safe for
// callers to retain. Use VotesForSorted when byte-order iteration is
// required (rarely — only paths that need to match the legacy K/V
// Range ordering deterministically). Prefer VotesForEach when only
// iteration (not retention) is needed — that path is alloc-free.
func (s *WitnessVoteStore) VotesFor(account auth.AccountName) []auth.AccountName {
	set, ok := s.votes[string(account)]
	if !ok || len(set) == 0 {
		return nil
	}
	out := make([]auth.AccountName, 0, len(set))
	for w := range set {
		out = append(out, auth.AccountName(w))
	}
	return out
}

// VotesForEach calls fn for each witness `account` currently votes
// for, in arbitrary order. Returning false from fn stops the walk.
// Zero allocation per call — used by the hot AdjustWitnessVotesForVoter
// path (fires from every CreateVesting / vote-delta adjustment, 30+
// witnesses each, 1.17 GB cum allocs on a 4M-block run with the
// alloc-returning VotesFor path).
//
// IMPORTANT: callers MUST NOT mutate the underlying vote set during
// iteration. The hot path is read-only; for paths that need to mutate,
// snapshot via VotesFor / VotesForSorted first.
func (s *WitnessVoteStore) VotesForEach(account auth.AccountName, fn func(witness auth.AccountName) bool) {
	set, ok := s.votes[string(account)]
	if !ok {
		return
	}
	for w := range set {
		if !fn(auth.AccountName(w)) {
			return
		}
	}
}

// VotesForEachWitness is VotesForEach but resolves each witness to its
// *Witness slot pointer (lazily caching the resolution). Skips a
// WitnessStore.primary map lookup per iteration when the cache is
// warm — that lookup is the hottest entry in mapaccess2_faststr on
// long replays.
func (s *WitnessVoteStore) VotesForEachWitness(account auth.AccountName, wstore *WitnessStore, fn func(witness auth.AccountName, w *Witness) bool) {
	set, ok := s.votes[string(account)]
	if !ok {
		return
	}
	for name, w := range set {
		if w == nil {
			cur, hit := wstore.primary[name]
			if !hit {
				continue
			}
			w = cur
			set[name] = w
		}
		if !fn(auth.AccountName(name), w) {
			return
		}
	}
}

// VotesForCount returns how many witnesses `account` votes for.
func (s *WitnessVoteStore) VotesForCount(account auth.AccountName) int {
	return len(s.votes[string(account)])
}

// VotesForSorted is VotesFor with deterministic ascending owner order.
func (s *WitnessVoteStore) VotesForSorted(account auth.AccountName) []auth.AccountName {
	set, ok := s.votes[string(account)]
	if !ok || len(set) == 0 {
		return nil
	}
	owners := make([]string, 0, len(set))
	for w := range set {
		owners = append(owners, w)
	}
	sort.Strings(owners)
	out := make([]auth.AccountName, len(owners))
	for i, o := range owners {
		out[i] = auth.AccountName(o)
	}
	return out
}

// CountFor returns how many witnesses account votes for.
func (s *WitnessVoteStore) CountFor(account auth.AccountName) int {
	return len(s.votes[string(account)])
}

var witnessVoteStoreSnapshotMagic = [4]byte{'W', 'V', 'O', 'T'}

const witnessVoteStoreSnapshotVersion uint32 = 1

// MarshalSnapshot serializes the account → witnesses graph to w.
// Format: magic | version | num_voters_u32 |
//
//	(varuint(account_len) | account |
//	 varuint(num_witnesses) |
//	 (varuint(witness_len) | witness)*)*
//
// Both voters and per-voter witnesses are emitted in ascending order
// for byte-stable snapshots.
func (s *WitnessVoteStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(witnessVoteStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], witnessVoteStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.votes)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	accounts := make([]string, 0, len(s.votes))
	for a := range s.votes {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	var varBuf [binary.MaxVarintLen64]byte
	witBuf := make([]string, 0, 32)
	for _, a := range accounts {
		set := s.votes[a]
		// account name
		n := binary.PutUvarint(varBuf[:], uint64(len(a)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, a); err != nil {
			return err
		}
		// num witnesses
		n = binary.PutUvarint(varBuf[:], uint64(len(set)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		// witness names sorted ascending
		witBuf = witBuf[:0]
		for ww := range set {
			witBuf = append(witBuf, ww)
		}
		sort.Strings(witBuf)
		for _, ww := range witBuf {
			n = binary.PutUvarint(varBuf[:], uint64(len(ww)))
			if _, err := w.Write(varBuf[:n]); err != nil {
				return err
			}
			if _, err := io.WriteString(w, ww); err != nil {
				return err
			}
		}
	}
	return nil
}

// UnmarshalSnapshot replaces the votes graph from r. Undo log cleared.
func (s *WitnessVoteStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("witness_vote_store: read magic: %w", err)
	}
	if magic != witnessVoteStoreSnapshotMagic {
		return fmt.Errorf("witness_vote_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("witness_vote_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != witnessVoteStoreSnapshotVersion {
		return fmt.Errorf("witness_vote_store: unsupported version %d", ver)
	}
	numVoters := binary.LittleEndian.Uint32(hdr[4:8])
	s.votes = make(map[string]map[string]*Witness, numVoters)
	s.undoLog = s.undoLog[:0]
	for i := uint32(0); i < numVoters; i++ {
		nameLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("witness_vote_store: read voter len[%d]: %w", i, err)
		}
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(br, name); err != nil {
			return fmt.Errorf("witness_vote_store: read voter[%d]: %w", i, err)
		}
		numW, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("witness_vote_store: read num_w[%d]: %w", i, err)
		}
		set := make(map[string]*Witness, numW)
		for j := uint64(0); j < numW; j++ {
			wlen, err := binary.ReadUvarint(br)
			if err != nil {
				return fmt.Errorf("witness_vote_store: read w len[%d/%d]: %w", i, j, err)
			}
			wbuf := make([]byte, wlen)
			if _, err := io.ReadFull(br, wbuf); err != nil {
				return fmt.Errorf("witness_vote_store: read w[%d/%d]: %w", i, j, err)
			}
			set[string(wbuf)] = nil // resolved lazily on first read
		}
		s.votes[string(name)] = set
	}
	return nil
}

// RangeAllVoters iterates every account that has at least one
// witness vote, in sorted order. Used by HF code that touches every
// voter (rare).
func (s *WitnessVoteStore) RangeAllVoters(fn func(account auth.AccountName, witnesses []auth.AccountName) (bool, error)) error {
	accounts := make([]string, 0, len(s.votes))
	for a := range s.votes {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	for _, a := range accounts {
		ws := s.VotesFor(auth.AccountName(a))
		ok, err := fn(auth.AccountName(a), ws)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}
