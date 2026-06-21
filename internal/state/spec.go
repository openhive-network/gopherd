package state

// SpecContext is the per-transaction speculative-execution state used by the
// optimistic parallel block-application path (see
// internal/chain/PARALLEL_DESIGN.md). It hangs off a Session as Session.Spec.
//
//   - Spec == nil  → the normal SERIAL path. Every hot method has a single
//     predictable `if s.Spec != nil` branch that is always-not-taken here, so
//     the serial path (the golden reference) is behaviorally unchanged.
//   - Spec != nil  → the Session is executing one transaction speculatively
//     during the parallel phase. READS come from the frozen, committed state
//     (no other goroutine mutates committed state during the parallel phase)
//     and are RECORDED into a read-set. WRITES are BUFFERED and never touch the
//     committed state. Any access to state that is not yet speculative-aware
//     calls Taint(), flagging the tx so the serial commit phase re-executes it
//     against real state — the always-correct fallback. Therefore a missed
//     instrumentation can only ever cost performance (an extra serial re-exec),
//     never correctness.
//
// The commit phase (in package chain) validates a clean tx's read-set against
// the now-mutating committed state: if every recorded read still matches, the
// buffered write-set is applied (cheap); otherwise the tx is re-executed.
type SpecContext struct {
	// tainted is set when this speculative tx touched state that the
	// speculative layer does not yet support. Tainted txs are discarded and
	// re-executed serially at commit, so their speculative reads/writes need
	// not be correct — only non-corrupting (writes are buffered, not applied).
	tainted bool

	// K/V-backed objects (those accessed via Session.Get/Put/Has/Delete:
	// DGPO, FeedHistory, escrow, convert requests, ...). Keyed by the encoded
	// physical key string.
	kvReads  map[string]kvSnap
	kvWrites map[string]kvWrite

	// Typed holds the objects-package speculative buffers (Account/Comment/...)
	// as an opaque value, type-asserted in package objects. Keeping it `any`
	// avoids a state→objects import cycle.
	Typed any
}

// kvSnap records the value observed on the first read of a K/V key (nil with
// existed=false means "absent"). Used to validate the read at commit.
type kvSnap struct {
	value   []byte
	existed bool
}

// kvWrite is a buffered K/V write: a value, or a tombstone (delete).
type kvWrite struct {
	value     []byte
	tombstone bool
}

// ForkSpeculative returns a lightweight Session that SHARES this session's
// committed read view (overlay, overlayKeys, readCache, and the backend via db)
// but has its own speculative write buffers (Spec), key scratch, serialization
// scratch, and Aux cache. The parallel block-apply harness creates one fork per
// worker: during the parallel phase committed state is frozen (no committing
// thread mutates it), so many forks read it concurrently, lock-free, while
// buffering their writes into per-fork Spec.
//
// mistrustDBAux is set so speculative reads bypass the cross-Session DatabaseAux
// cache (shared, would race) and read from overlay/backend instead. NOT a
// general-purpose clone — only valid while committed state is frozen.
func (s *Session) ForkSpeculative() *Session {
	return &Session{
		db:            s.db,
		snapshotGen:   s.snapshotGen,
		overlay:       s.overlay,
		overlayKeys:   s.overlayKeys,
		readCache:     s.readCache,
		scratch:       make([]byte, 0, 64),
		SerScratch:    make([]byte, 0, 256),
		Aux:           make(map[string]any, 8),
		mistrustDBAux: true,
		Spec:          NewSpecContext(),
	}
}

// NewSpecContext returns a fresh speculative context.
func NewSpecContext() *SpecContext {
	return &SpecContext{
		kvReads:  make(map[string]kvSnap, 8),
		kvWrites: make(map[string]kvWrite, 8),
	}
}

// Taint marks the speculative tx for serial re-execution at commit.
func (sp *SpecContext) Taint() { sp.tainted = true }

// Tainted reports whether the speculative tx must be re-executed serially.
func (sp *SpecContext) Tainted() bool { return sp.tainted }

// Reset clears the context for reuse on the next speculative tx.
func (sp *SpecContext) Reset() {
	sp.tainted = false
	clear(sp.kvReads)
	clear(sp.kvWrites)
	sp.Typed = nil
}

// specKVGet returns the speculative view of a K/V key for a Session whose
// Spec != nil: a buffered write if present, else the frozen committed value
// (recorded into the read-set). The bool reports existence (matching Get's
// ErrNotFound contract at the call site).
func (s *Session) specKVGet(fullKey []byte) ([]byte, bool) {
	sp := s.Spec
	k := string(fullKey)
	if w, ok := sp.kvWrites[k]; ok {
		if w.tombstone {
			return nil, false
		}
		return w.value, true
	}
	if r, ok := sp.kvReads[k]; ok {
		return r.value, r.existed
	}
	// First touch: read frozen committed state (overlay+readCache+backend) and
	// record it. During the parallel phase no one mutates committed state, so
	// this read is stable and lock-free. Copy the value into the read-set: the
	// commit phase validates it WHILE committed state is being mutated, so an
	// aliased overlay slice could otherwise be compared against mutated bytes.
	v, existed := s.committedGet(fullKey)
	var snap []byte
	if existed {
		snap = make([]byte, len(v))
		copy(snap, v)
	}
	sp.kvReads[k] = kvSnap{value: snap, existed: existed}
	return v, existed
}

// committedGet reads the committed value for an already-encoded physical key,
// bypassing any speculative buffers. Mirrors Get's overlay→readCache→backend
// order but returns an existence bool instead of ErrNotFound.
func (s *Session) committedGet(fullKey []byte) ([]byte, bool) {
	k := string(fullKey)
	if e, ok := s.overlay[k]; ok {
		if e.tombstone {
			return nil, false
		}
		return e.value, true
	}
	if v, ok := s.readCache[k]; ok {
		return v, true
	}
	v, err := s.db.rawGet(fullKey)
	if err != nil {
		return nil, false
	}
	// Note: do NOT populate readCache here — that map is shared committed state
	// and writing it from parallel workers would race. Speculative reads are
	// memoized in sp.kvReads instead.
	return v, true
}

// specKVPut buffers a K/V write on the speculative context.
func (s *Session) specKVPut(fullKey, value []byte) {
	cv := make([]byte, len(value))
	copy(cv, value)
	s.Spec.kvWrites[string(fullKey)] = kvWrite{value: cv}
}

// specKVDelete buffers a K/V tombstone on the speculative context.
func (s *Session) specKVDelete(fullKey []byte) {
	s.Spec.kvWrites[string(fullKey)] = kvWrite{tombstone: true}
}

// SpecKVWrites invokes fn for each buffered K/V write (value or tombstone) so
// the commit phase can replay them into committed state in transaction order.
// fullKey is the encoded physical key.
func (sp *SpecContext) SpecKVWrites(fn func(fullKey string, value []byte, tombstone bool)) {
	for k, w := range sp.kvWrites {
		fn(k, w.value, w.tombstone)
	}
}

// SpecKVReadsValid reports whether every recorded K/V read still matches the
// committed state reachable via getter (used at commit to validate a clean tx).
func (sp *SpecContext) SpecKVReadsValid(getter func(fullKey string) (value []byte, existed bool)) bool {
	for k, r := range sp.kvReads {
		v, existed := getter(k)
		if existed != r.existed {
			return false
		}
		if existed && !bytesEqual(v, r.value) {
			return false
		}
	}
	return true
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
