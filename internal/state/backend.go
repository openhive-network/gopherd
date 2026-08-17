package state

// backend abstracts the underlying key/value engine so we can swap
// Pebble for an in-memory btree (chainbase-style) without touching the
// chain or evaluator code. Implementations:
//   - pebbleBackend: persistent LSM-tree (production / general use)
//   - memBackend:    sorted in-memory btree (replay-only — fits the
//     full mainnet state in a few GB and avoids LSM
//     Get/iter/compaction overhead)
type backend interface {
	rawGet(physicalKey []byte) ([]byte, error)
	rawHas(physicalKey []byte) (bool, error)
	rawIter(lower, upper []byte) (Iter, error)
	// rawScan walks [lower, upper) in ascending key order, calling fn
	// for each entry. Returning false stops the scan. fn returning an
	// error stops the scan and the error is returned.
	//
	// This is an alloc-free alternative to rawIter for callers that
	// don't need to interleave with another iterator — memBackend's
	// implementation uses tree.Ascend (callback-driven) which never
	// allocates an iterator stack frame, whereas rawIter allocates a
	// btree.IterG[T] each time the bucket changes (~830 MB / 4M
	// blocks under the Session.Range no-overlay fast path).
	rawScan(lower, upper []byte, fn func(key, value []byte) (bool, error)) error
	commit(overlay map[string]overlayEntry, sync bool) error
	commitDirty(overlay map[string]overlayEntry, dirtyKeys []string, sync bool) error
	// rangeAll calls fn(key, value) for every committed row in the
	// backend, in implementation-defined order (Pebble: ascending key;
	// mem: ascending bucket then ascending key). Used by the snapshot
	// saver to dump the entire backend in one pass — rawIter alone
	// doesn't suffice for memBackend, which keys per-table btrees off
	// the first 3 bytes of the physical key and so can only iterate
	// one table at a time.
	rangeAll(fn func(key, value []byte) error) error
	Close() error
}

// Iter is the abstract iterator interface used by Session.Range. Both
// the Pebble iterator and the in-memory btree iterator satisfy it.
//
// Lifecycle: First() seeks to the lower bound (set via SetBounds or at
// construction). Valid() reports whether the current position is in
// [lower, upper). Next() advances. Key() / ValueAndErr() expose the
// current entry (slices are valid until the next mutating call).
// Close() releases the iterator. SetBounds() re-seeks for reuse without
// allocation.
type Iter interface {
	SetBounds(lower, upper []byte)
	First() bool
	Valid() bool
	Next() bool
	Key() []byte
	ValueAndErr() ([]byte, error)
	Error() error
	Close() error
}
