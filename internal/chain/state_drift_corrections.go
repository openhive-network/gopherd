package chain

import "hived-go/internal/state"

// applyStateDriftCorrections is intentionally a NO-OP.
//
// database.go calls this hook before the per-block tx loop. It was
// introduced as a band-aid: hardcoding per-block balance "corrections"
// to mask cumulative drifts whose root cause hadn't been found, so the
// replay could limp past the failing block. That approach treats the
// symptom (a balance is off at block B) instead of the disease (a
// consensus formula/ordering diverged from hived somewhere upstream),
// and every masked drift hides the next one.
//
// The principled replacement is reference-free invariant checking
// (see CheckInvariants in invariants.go): run the HIVE/HBD/VESTS
// conservation laws every N blocks during replay and abort at the FIRST
// block where the books stop balancing, which localizes the real bug.
// Keeping this hook as a no-op preserves the call site while we root
// cause drifts properly rather than papering over them.
//
// If a machine-local corrections table exists (untracked), it can
// redefine this function; this committed stub only guarantees the tree
// builds and that master carries no hidden balance fudges.
func applyStateDriftCorrections(_ *state.Session, _ uint32) {}
