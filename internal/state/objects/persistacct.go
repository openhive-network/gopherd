package objects

import (
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state/persiststore"
)

// PersistAccountStore is a persiststore-backed account store (chainbase-in-Go
// phase 3): the same Account codec (AppendAccount/ReadAccount) and the same
// public operations as the in-memory AccountStore, but state lives in an
// mmap-backed log-structured store that SURVIVES RESTART without replay.
//
// Trade-off vs the in-memory AccountStore: there is no resident *Account, so
// Get/Mutate decode and Put re-encodes on every access (the serialization the
// typed-store refactor removed). That makes it the right home for append-mostly
// tables (comments/votes — phase 4) and a correctness PROOF for the integration
// pattern here; hot, constantly-mutated accounts may stay in-memory + flush.
//
// Implements the SnapshotSubsystem contract (SnapshotPoint/RestoreToPoint/
// CommitPoint) by delegating to the underlying store, so it drops into the
// chain's savepoint/restore machinery unchanged.
type PersistAccountStore struct {
	ps  *persiststore.Store
	ser []byte // reusable AppendAccount buffer
}

// OpenPersistAccountStore opens (or restores) the account table under dir.
func OpenPersistAccountStore(dir string, reserveBytes uint64) (*PersistAccountStore, error) {
	ps, err := persiststore.Open(dir, reserveBytes)
	if err != nil {
		return nil, err
	}
	return &PersistAccountStore{ps: ps}, nil
}

func (p *PersistAccountStore) Has(name auth.AccountName) bool {
	_, ok := p.ps.Get(string(name))
	return ok
}

func (p *PersistAccountStore) Get(name auth.AccountName) (Account, bool) {
	b, ok := p.ps.Get(string(name))
	if !ok {
		return Account{}, false
	}
	a, err := ReadAccount(b)
	if err != nil {
		return Account{}, false
	}
	return a, true
}

// GetRef returns a pointer to a FRESH decoded copy (not a stable in-store
// pointer — persiststore holds bytes, not structs). Callers must Put to persist
// mutations; the in-place Ref-and-mutate-without-Put optimization is N/A here.
func (p *PersistAccountStore) GetRef(name auth.AccountName) (*Account, bool) {
	a, ok := p.Get(name)
	if !ok {
		return nil, false
	}
	return &a, true
}

func (p *PersistAccountStore) Put(a *Account) {
	p.ser = AppendAccount(p.ser[:0], a)
	_ = p.ps.Set(string(a.Name), p.ser)
}

func (p *PersistAccountStore) Mutate(name auth.AccountName, fn func(*Account)) bool {
	a, ok := p.Get(name)
	if !ok {
		return false
	}
	fn(&a)
	p.Put(&a)
	return true
}

func (p *PersistAccountStore) Delete(name auth.AccountName) {
	p.ps.Delete(string(name))
}

// RangePrimary iterates every account in ascending name order (matching the
// in-memory store's RangePrimary, which also sorts).
func (p *PersistAccountStore) RangePrimary(fn func(a *Account) (bool, error)) error {
	for _, k := range p.ps.SortedKeys() {
		b, ok := p.ps.Get(k)
		if !ok {
			continue
		}
		a, err := ReadAccount(b)
		if err != nil {
			return err
		}
		ok2, err := fn(&a)
		if err != nil {
			return err
		}
		if !ok2 {
			return nil
		}
	}
	return nil
}

func (p *PersistAccountStore) Len() int { return p.ps.Len() }

// --- SnapshotSubsystem contract (per-tx rollback) ---
func (p *PersistAccountStore) SetUndoEnabled(b bool) { p.ps.SetUndoEnabled(b) }
func (p *PersistAccountStore) SnapshotPoint() int    { return p.ps.SnapshotPoint() }
func (p *PersistAccountStore) RestoreToPoint(n int)  { p.ps.RestoreToPoint(n) }
func (p *PersistAccountStore) CommitPoint()          { p.ps.CommitPoint() }

// --- persistence lifecycle ---
func (p *PersistAccountStore) Flush() error       { return p.ps.Flush() }
func (p *PersistAccountStore) Compact() error     { return p.ps.Compact() }
func (p *PersistAccountStore) Close() error       { return p.ps.Close() }
func (p *PersistAccountStore) DeadRatio() float64 { return p.ps.DeadRatio() }
