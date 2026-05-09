package objects

import (
	"sort"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// AccountStore is the in-memory typed container for the Account table.
// It replaces the generic K/V state path for accounts: mutations
// happen in-place against the primary map, with no serialize → overlay
// copy → K/V commit chain. Mirrors the chainbase-style WitnessStore.
//
// Why typed-store: at 3.5M blocks the chain holds ~400K accounts, each
// touched on every vote / transfer / cashout / witness op. The previous
// K/V path serialized each Put (~150 bytes / account) and re-parsed each
// Get into a fresh Account struct. Profiling showed Account serde
// dominating the cashout path past block 3M; the typed-store eliminates
// that overhead and is ~3-5× faster across the board.
//
// Invariants
//
//   - primary[name] points to the canonical *Account for `name`. Get
//     returns a value-copy so callers never mutate the stored record
//     except via Put.
//   - undoLog captures the prior pointer for every Put. RestoreToPoint
//     replays in reverse to undo a tx's writes.
//
// Account has no secondary indexes (the by-name primary is the only
// one used in consensus-critical paths). Cross-account scans (e.g.
// removeExpiredGovernanceVotes) walk RangePrimary in name order.
type AccountStore struct {
	primary map[string]*Account
	undoLog []accountUndo
}

type accountUndo struct {
	name string
	prev *Account // nil = the row didn't exist before this Put
}

const accountStoreSubsystemKey = "account_store"

// EnsureAccountStore returns the database's AccountStore, lazily
// creating + registering it on first call. Domain code calls this
// instead of building stores by hand so the snapshot/commit hooks are
// always wired.
func EnsureAccountStore(s *state.Session) *AccountStore {
	db := s.Database()
	if existing := db.GetSubsystem(accountStoreSubsystemKey); existing != nil {
		if as, ok := existing.(*AccountStore); ok {
			return as
		}
	}
	as := newAccountStore()
	db.RegisterSubsystem(accountStoreSubsystemKey, as, as)
	return as
}

func newAccountStore() *AccountStore {
	return &AccountStore{
		primary: make(map[string]*Account, 4096),
		undoLog: make([]accountUndo, 0, 64),
	}
}

// SnapshotPoint returns the current undo-log length.
func (s *AccountStore) SnapshotPoint() int { return len(s.undoLog) }

// RestoreToPoint replays the undo log in reverse from the back until
// the log length equals `snap`.
func (s *AccountStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		if e.prev != nil {
			s.primary[e.name] = e.prev
		} else {
			delete(s.primary, e.name)
		}
	}
	s.undoLog = s.undoLog[:snap]
}

// CommitPoint releases accumulated undo entries — called from
// Session.Commit. After this point the prior images can no longer be
// restored.
func (s *AccountStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Has reports whether `name` has an account row.
func (s *AccountStore) Has(name auth.AccountName) bool {
	_, ok := s.primary[string(name)]
	return ok
}

// Get returns a value-copy of the account for `name`. The boolean is
// false if the row doesn't exist; the returned Account in that case
// is the zero value.
func (s *AccountStore) Get(name auth.AccountName) (Account, bool) {
	a, ok := s.primary[string(name)]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

// Put inserts or replaces the account keyed by a.Name. The caller's
// `a` is copied — subsequent caller mutations don't affect the store.
// The previous pointer (if any) is captured in the undo log so a
// later RestoreToPoint can reinstate it; that means the Account
// pointed to by the prior slot must NOT be mutated externally — it
// becomes the canonical "before image". Use Put again to update.
func (s *AccountStore) Put(a *Account) {
	name := string(a.Name)
	prev := s.primary[name]
	s.undoLog = append(s.undoLog, accountUndo{name: name, prev: prev})
	cp := *a
	s.primary[name] = &cp
}

// Delete removes the account row for `name`. Captures the prior
// pointer in the undo log.
func (s *AccountStore) Delete(name auth.AccountName) {
	n := string(name)
	prev, ok := s.primary[n]
	if !ok {
		return
	}
	s.undoLog = append(s.undoLog, accountUndo{name: n, prev: prev})
	delete(s.primary, n)
}

// RangePrimary calls fn for every account in ascending name order.
// Used by cross-account hooks (governance vote expiration sweeps,
// VoteRevoked scans). Returning false from fn stops iteration.
func (s *AccountStore) RangePrimary(fn func(a *Account) (bool, error)) error {
	names := make([]string, 0, len(s.primary))
	for n := range s.primary {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ok, err := fn(s.primary[n])
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// Count returns the number of accounts currently in the store.
func (s *AccountStore) Count() int { return len(s.primary) }
