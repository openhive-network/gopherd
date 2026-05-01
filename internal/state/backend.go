package state

// backend abstracts the underlying key/value engine so we can swap
// Pebble for an in-memory btree (chainbase-style) without touching the
// chain or evaluator code. Implementations:
//   - pebbleBackend: persistent LSM-tree (production / general use)
//   - memBackend:    sorted in-memory btree (replay-only — fits the
//                    full mainnet state in a few GB and avoids LSM
//                    Get/iter/compaction overhead)
type backend interface {
	rawGet(physicalKey []byte) ([]byte, error)
	rawHas(physicalKey []byte) (bool, error)
	rawIter(lower, upper []byte) (Iter, error)
	commit(overlay map[string]overlayEntry, sync bool) error
	commitDirty(overlay map[string]overlayEntry, dirtyKeys []string, sync bool) error
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
