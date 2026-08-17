package objects

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state/persiststore"
)

// PersistCommentStore is a persiststore-backed comment store (chainbase-in-Go
// phase 4c): comments live in an mmap-backed log-structured store that survives
// restart without replay, replacing the in-memory map + the evict/promote cold
// tier (and its slow re-serializing snapshot). Comments are append-mostly
// (created once, mutated by votes until cashout, then frozen), so persiststore
// fits — and they're the bulk of state volume + the slow-save culprit.
//
// Each comment is stored under two keys (reusing the cold-tier helpers + codec):
// PRIMARY (author,permlink) for point ops, and BY-ROOT
// (rootAuthor,rootPermlink,author,permlink), registered as an ordered table for
// the pre-HF17 cashout sibling-walk (RangeByRoot). Post-HF17 the by-root scan is
// unused, but the index stays correct.
type PersistCommentStore struct {
	ps   *persiststore.Store
	serC []byte
	keyP []byte
	keyR []byte
}

func OpenPersistCommentStore(dir string, reserveBytes uint64) (*PersistCommentStore, error) {
	ps, err := persiststore.Open(dir, reserveBytes)
	if err != nil {
		return nil, err
	}
	ps.RegisterOrderedTable(coldTableCommentByRoot)
	return &PersistCommentStore{ps: ps}, nil
}

// Put writes the comment under its primary + by-root keys. INVARIANT: a
// comment's root is immutable (fixed at creation by the chain), so re-Put
// (e.g. a vote updating NetRshares) keeps the same by-root key and just
// overwrites — no stale secondary entry. We rely on this to avoid a prior-state
// lookup on the hot per-vote Put path.
func (s *PersistCommentStore) Put(c *Comment) {
	s.serC = AppendComment(s.serC[:0], c)
	s.keyP = appendColdPrimaryKey(s.keyP[:0], c.Author, c.Permlink)
	_ = s.ps.Set(string(s.keyP), s.serC)
	s.keyR = appendColdByRootKey(s.keyR[:0], c)
	_ = s.ps.Set(string(s.keyR), s.serC)
}

func (s *PersistCommentStore) Get(author auth.AccountName, permlink string) (Comment, bool) {
	s.keyP = appendColdPrimaryKey(s.keyP[:0], author, permlink)
	b, ok := s.ps.Get(string(s.keyP))
	if !ok {
		return Comment{}, false
	}
	c, err := ReadComment(b)
	if err != nil {
		return Comment{}, false
	}
	return c, true
}

func (s *PersistCommentStore) Has(author auth.AccountName, permlink string) bool {
	s.keyP = appendColdPrimaryKey(s.keyP[:0], author, permlink)
	_, ok := s.ps.Get(string(s.keyP))
	return ok
}

// Delete removes a comment (both keys). It must look up the comment first to
// reconstruct its by-root key (the root may differ from self).
func (s *PersistCommentStore) Delete(author auth.AccountName, permlink string) {
	c, ok := s.Get(author, permlink)
	if !ok {
		return
	}
	s.keyP = appendColdPrimaryKey(s.keyP[:0], author, permlink)
	s.ps.Delete(string(s.keyP))
	s.keyR = appendColdByRootKey(s.keyR[:0], &c)
	s.ps.Delete(string(s.keyR))
}

// RangeByRoot iterates every comment under (rootAuthor, rootPermlink) — the root
// and all replies — in ascending (author,permlink) order. The pre-HF17 cashout
// sibling-walk. fn returns false to stop.
func (s *PersistCommentStore) RangeByRoot(rootAuthor auth.AccountName, rootPermlink string, fn func(c *Comment) bool) {
	prefix := append([]byte{coldTableCommentByRoot}, CommentByRootPrefix(rootAuthor, rootPermlink)...)
	s.ps.RangePrefix(prefix, func(_ string, val []byte) bool {
		c, err := ReadComment(val)
		if err != nil {
			return true
		}
		return fn(&c)
	})
}

// RangePrimary iterates every comment in ascending (author,permlink) order.
// Occasional use only (pre-HF17 fixups, diagnostics) — it filters the full
// sorted key set to the primary table, which is O(total keys); not a hot path.
func (s *PersistCommentStore) RangePrimary(fn func(c *Comment) bool) {
	for _, k := range s.ps.SortedKeys() {
		if len(k) == 0 || k[0] != coldTableCommentPrimary {
			continue
		}
		b, ok := s.ps.Get(k)
		if !ok {
			continue
		}
		c, err := ReadComment(b)
		if err != nil {
			continue
		}
		if !fn(&c) {
			return
		}
	}
}

func (s *PersistCommentStore) Len() int { return s.ps.Len() } // counts both key kinds

// --- SnapshotSubsystem contract ---
func (s *PersistCommentStore) SetUndoEnabled(b bool) { s.ps.SetUndoEnabled(b) }
func (s *PersistCommentStore) SnapshotPoint() int    { return s.ps.SnapshotPoint() }
func (s *PersistCommentStore) RestoreToPoint(n int)  { s.ps.RestoreToPoint(n) }
func (s *PersistCommentStore) CommitPoint()          { s.ps.CommitPoint() }

// --- lifecycle ---
func (s *PersistCommentStore) Flush() error   { return s.ps.Flush() }
func (s *PersistCommentStore) Compact() error { return s.ps.Compact() }
func (s *PersistCommentStore) Close() error   { return s.ps.Close() }
