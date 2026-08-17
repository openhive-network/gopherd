package objects

import (
	"bytes"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// SpecTyped holds the objects-layer (typed-store) speculative buffers for one
// transaction being executed optimistically in the parallel block-apply harness
// (see internal/chain/PARALLEL_DESIGN.md). It is stored opaquely in
// state.Session.Spec.Typed so the state package need not know object types.
//
// Only the Account family is speculative-aware so far. Reads come from the
// frozen committed store and are snapshotted (serialized) into a read-set for
// commit-time validation; writes are buffered and applied at commit in tx order.
// Every other typed store taints the transaction (forcing serial re-execution)
// until it is instrumented.
type SpecTyped struct {
	acctReads  map[auth.AccountName]acctSnap
	acctWrites map[auth.AccountName]Account
}

type acctSnap struct {
	a      Account // value observed on first read (for in-tx re-reads)
	ser    []byte  // serialized snapshot, captured at read time, for validation
	exists bool
}

func specTypedOf(s *state.Session) *SpecTyped {
	if s.Spec.Typed == nil {
		s.Spec.Typed = &SpecTyped{
			acctReads:  make(map[auth.AccountName]acctSnap, 4),
			acctWrites: make(map[auth.AccountName]Account, 4),
		}
	}
	return s.Spec.Typed.(*SpecTyped)
}

// specGetAccount returns the speculative view of an account: a buffered write
// if present, else the read-set snapshot, else the frozen committed value
// (recorded into the read-set).
func specGetAccount(s *state.Session, name auth.AccountName) (Account, bool) {
	st := specTypedOf(s)
	if a, ok := st.acctWrites[name]; ok {
		return a, true
	}
	if r, ok := st.acctReads[name]; ok {
		return r.a, r.exists
	}
	a, ok := EnsureAccountStore(s).Get(name) // shared store, frozen this phase
	var ser []byte
	if ok {
		ser = AppendAccount(nil, &a)
	}
	st.acctReads[name] = acctSnap{a: a, ser: ser, exists: ok}
	return a, ok
}

// specPutAccount buffers an account write.
func specPutAccount(s *state.Session, a *Account) {
	specTypedOf(s).acctWrites[a.Name] = *a
}

// specGetAccountRef returns a read-only pointer to a speculative copy of the
// account (write-buffer → read-set → committed). The pointer addresses a fresh
// copy; callers must honor GetAccountRef's read-only contract.
func specGetAccountRef(s *state.Session, name auth.AccountName) (*Account, bool) {
	a, ok := specGetAccount(s, name)
	if !ok {
		return nil, false
	}
	cp := a
	return &cp, true
}

// specHasAccount reports existence over the speculative view.
func specHasAccount(s *state.Session, name auth.AccountName) bool {
	_, ok := specGetAccount(s, name)
	return ok
}

// specMutateAccount mirrors MutateAccount over the speculative buffers.
func specMutateAccount(s *state.Session, name auth.AccountName, fn func(*Account)) bool {
	a, ok := specGetAccount(s, name)
	if !ok {
		return false
	}
	fn(&a)
	specPutAccount(s, &a)
	return true
}

// SpecTypedReadsValid reports whether every recorded typed-store read still
// matches the current (now-mutating) committed store. Called at commit before
// applying a clean tx's buffered writes. buf is a reusable scratch buffer.
func SpecTypedReadsValid(s *state.Session, buf *[]byte) bool {
	if s.Spec == nil || s.Spec.Typed == nil {
		return true
	}
	st := s.Spec.Typed.(*SpecTyped)
	store := EnsureAccountStore(s)
	for name, snap := range st.acctReads {
		cur, ok := store.GetRef(name)
		if ok != snap.exists {
			return false
		}
		if ok {
			*buf = AppendAccount((*buf)[:0], cur)
			if !bytes.Equal(*buf, snap.ser) {
				return false
			}
		}
	}
	return true
}

// SpecApplyTypedWrites applies a clean tx's buffered typed-store writes to the
// real stores, in commit (transaction) order. Account writes within one tx
// target distinct names, so map-iteration order is immaterial.
func SpecApplyTypedWrites(s *state.Session) {
	if s.Spec == nil || s.Spec.Typed == nil {
		return
	}
	st := s.Spec.Typed.(*SpecTyped)
	store := EnsureAccountStore(s)
	for name := range st.acctWrites {
		a := st.acctWrites[name]
		store.Put(&a)
	}
}
