package objects

import (
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
	// votes[account] is the set of witnesses `account` votes for.
	// Empty/missing entry == no votes.
	votes map[string]map[string]struct{}

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
	if existing := db.GetSubsystem(witnessVoteStoreSubsystemKey); existing != nil {
		if vs, ok := existing.(*WitnessVoteStore); ok {
			return vs
		}
	}
	vs := &WitnessVoteStore{
		votes:   make(map[string]map[string]struct{}, 256),
		undoLog: make([]witnessVoteUndo, 0, 32),
	}
	db.RegisterSubsystem(witnessVoteStoreSubsystemKey, vs, vs)
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
				set = make(map[string]struct{}, 4)
				s.votes[e.account] = set
			}
			set[e.witness] = struct{}{}
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
		set = make(map[string]struct{}, 4)
		s.votes[a] = set
	}
	_, exists := set[w]
	s.undoLog = append(s.undoLog, witnessVoteUndo{account: a, witness: w, before: exists})
	set[w] = struct{}{}
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
		s.undoLog = append(s.undoLog, witnessVoteUndo{account: a, witness: w, before: false})
		return
	}
	_, exists := set[w]
	s.undoLog = append(s.undoLog, witnessVoteUndo{account: a, witness: w, before: exists})
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
// Range ordering deterministically).
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
