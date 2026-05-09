package objects

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// CommentStore is the in-memory typed container for the Comment table.
// Replaces the K/V serde path with a primary map + per-root index.
//
// Why typed-store: by 3.5M blocks the chain holds ~5M comments and
// every cashout walks the by-root index for the discussion ROOT,
// reading every reply via GetComment. The K/V path serialized each
// Get into a fresh Comment struct on every call. Typed-store eliminates
// that overhead — the per-root slice gives O(1) access to a thread's
// reply list, and the primary map gives O(1) GetComment.
//
// Invariants
//
//   - primary[commentKey(author, permlink)] points to canonical *Comment.
//   - byRoot[commentKey(rootAuthor, rootPermlink)] holds every comment
//     in that thread (root + replies); each *Comment in the slice is
//     the SAME pointer as primary[its-own-key].
//   - undoLog captures pre-mutation state so RestoreToPoint reverts
//     atomically to a savepoint.
type CommentStore struct {
	primary map[string]*Comment
	byRoot  map[string][]*Comment
	undoLog []commentUndo
}

type commentUndo struct {
	key        string  // primary key
	prev       *Comment // nil = the row didn't exist before this mutation
	prevInRoot string  // root_key of `prev` (for restoring to byRoot)
	curInRoot  string  // root_key of the post-mutation row (for removing from byRoot)
}

const commentStoreSubsystemKey = "comment_store"

// EnsureCommentStore returns the database's CommentStore, creating on
// first call.
func EnsureCommentStore(s *state.Session) *CommentStore {
	db := s.Database()
	if existing := db.GetSubsystem(commentStoreSubsystemKey); existing != nil {
		if cs, ok := existing.(*CommentStore); ok {
			return cs
		}
	}
	cs := newCommentStore()
	db.RegisterSubsystem(commentStoreSubsystemKey, cs, cs)
	return cs
}

func newCommentStore() *CommentStore {
	return &CommentStore{
		primary: make(map[string]*Comment, 4096),
		byRoot:  make(map[string][]*Comment, 1024),
		undoLog: make([]commentUndo, 0, 64),
	}
}

func (s *CommentStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *CommentStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		// Reverse the byRoot membership change first.
		if e.curInRoot != "" {
			// Mutation added a comment with cur root — remove it.
			s.byRootRemove(e.curInRoot, e.key)
		}
		if e.prev != nil {
			s.primary[e.key] = e.prev
			if e.prevInRoot != "" {
				// Restore prev into byRoot (if not already there).
				s.byRoot[e.prevInRoot] = append(s.byRoot[e.prevInRoot], e.prev)
			}
		} else {
			delete(s.primary, e.key)
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *CommentStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// rootKeyOf returns the byRoot map key for a comment's RootAuthor/Permlink.
func (s *CommentStore) rootKeyOf(c *Comment) string {
	rootAuthor := c.RootAuthor
	rootPermlink := c.RootPermlink
	if rootAuthor == "" {
		rootAuthor = c.Author
		rootPermlink = c.Permlink
	}
	return string(commentKey(rootAuthor, rootPermlink))
}

// byRootRemove drops `key` from byRoot[rootKey]. No-op if not present.
func (s *CommentStore) byRootRemove(rootKey, key string) {
	list := s.byRoot[rootKey]
	for i, c := range list {
		if string(commentKey(c.Author, c.Permlink)) == key {
			// Swap-remove: O(1) instead of O(N).
			list[i] = list[len(list)-1]
			list = list[:len(list)-1]
			if len(list) == 0 {
				delete(s.byRoot, rootKey)
			} else {
				s.byRoot[rootKey] = list
			}
			return
		}
	}
}

// Has reports whether (author, permlink) has a comment row.
func (s *CommentStore) Has(author auth.AccountName, permlink string) bool {
	_, ok := s.primary[string(commentKey(author, permlink))]
	return ok
}

// Get returns a value-copy of the comment for (author, permlink).
func (s *CommentStore) Get(author auth.AccountName, permlink string) (Comment, bool) {
	c, ok := s.primary[string(commentKey(author, permlink))]
	if !ok {
		return Comment{}, false
	}
	return *c, true
}

// Put inserts or replaces. Updates byRoot membership: removes prev from
// its old root list (if root changed), adds new to its current root list.
func (s *CommentStore) Put(c *Comment) {
	if c.RootAuthor == "" {
		c.RootAuthor = c.Author
		c.RootPermlink = c.Permlink
	}
	key := string(commentKey(c.Author, c.Permlink))
	prev := s.primary[key]

	cp := *c
	curRoot := s.rootKeyOf(&cp)
	prevRoot := ""
	if prev != nil {
		prevRoot = s.rootKeyOf(prev)
		// Remove the OLD pointer from byRoot[prevRoot] so we can re-add
		// the new pointer cleanly.
		s.byRootRemove(prevRoot, key)
	}
	s.byRoot[curRoot] = append(s.byRoot[curRoot], &cp)
	s.primary[key] = &cp

	s.undoLog = append(s.undoLog, commentUndo{
		key:        key,
		prev:       prev,
		prevInRoot: prevRoot,
		curInRoot:  curRoot,
	})
}

// Delete removes (author, permlink). Captures undo state.
func (s *CommentStore) Delete(author auth.AccountName, permlink string) {
	key := string(commentKey(author, permlink))
	prev, ok := s.primary[key]
	if !ok {
		return
	}
	prevRoot := s.rootKeyOf(prev)
	s.byRootRemove(prevRoot, key)
	delete(s.primary, key)
	s.undoLog = append(s.undoLog, commentUndo{
		key:        key,
		prev:       prev,
		prevInRoot: prevRoot,
		curInRoot:  "",
	})
}

// RangeByRoot iterates every comment in the discussion rooted at
// (rootAuthor, rootPermlink). Order is name-ascending (stable across
// runs) — sorting happens once at iter time, which is amortized over
// per-comment work in the cashout. Returning false from fn stops.
func (s *CommentStore) RangeByRoot(rootAuthor auth.AccountName, rootPermlink string, fn func(c *Comment) (bool, error)) error {
	rootKey := string(commentKey(rootAuthor, rootPermlink))
	list := s.byRoot[rootKey]
	if len(list) == 0 {
		return nil
	}
	// Sort by primary key (= alphabetical (author, permlink)) so iteration
	// order matches the K/V backend's by-root prefix scan.
	sorted := append([]*Comment(nil), list...)
	sort.Slice(sorted, func(i, j int) bool {
		return string(commentKey(sorted[i].Author, sorted[i].Permlink)) <
			string(commentKey(sorted[j].Author, sorted[j].Permlink))
	})
	for _, c := range sorted {
		ok, err := fn(c)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// RangePrimary iterates every comment in name-ascending order. Used by
// HF12 cashout-fix and similar full-table sweeps.
func (s *CommentStore) RangePrimary(fn func(c *Comment) (bool, error)) error {
	keys := make([]string, 0, len(s.primary))
	for k := range s.primary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ok, err := fn(s.primary[k])
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// Count returns the comment count.
func (s *CommentStore) Count() int { return len(s.primary) }

var commentStoreSnapshotMagic = [4]byte{'C', 'M', 'N', 'T'}

const commentStoreSnapshotVersion uint32 = 1

// MarshalSnapshot serializes the primary comment map to w. Format mirrors
// witness_store: magic | version | count | (varuint(key)|key|varuint(rec)|AppendComment(rec))*
// Keys are the raw bytes returned by commentKey(author, permlink), emitted
// in ascending order. byRoot is rebuilt on UnmarshalSnapshot.
func (s *CommentStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(commentStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], commentStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	keys := make([]string, 0, len(s.primary))
	for k := range s.primary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var (
		varBuf [binary.MaxVarintLen64]byte
		recBuf []byte
	)
	for _, k := range keys {
		cp := s.primary[k]
		n := binary.PutUvarint(varBuf[:], uint64(len(k)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, k); err != nil {
			return err
		}
		recBuf = AppendComment(recBuf[:0], cp)
		n = binary.PutUvarint(varBuf[:], uint64(len(recBuf)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		if _, err := w.Write(recBuf); err != nil {
			return err
		}
	}
	return nil
}

// UnmarshalSnapshot rebuilds the primary map AND the byRoot secondary
// index from r. Undo log cleared.
func (s *CommentStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("comment_store: read magic: %w", err)
	}
	if magic != commentStoreSnapshotMagic {
		return fmt.Errorf("comment_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("comment_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != commentStoreSnapshotVersion {
		return fmt.Errorf("comment_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.primary = make(map[string]*Comment, count)
	s.byRoot = make(map[string][]*Comment, count/8+1)
	s.undoLog = s.undoLog[:0]
	for i := uint32(0); i < count; i++ {
		keyLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("comment_store: read key len[%d]: %w", i, err)
		}
		keyBuf := make([]byte, keyLen)
		if _, err := io.ReadFull(br, keyBuf); err != nil {
			return fmt.Errorf("comment_store: read key[%d]: %w", i, err)
		}
		recLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("comment_store: read rec len[%d]: %w", i, err)
		}
		rec := make([]byte, recLen)
		if _, err := io.ReadFull(br, rec); err != nil {
			return fmt.Errorf("comment_store: read rec[%d]: %w", i, err)
		}
		c, err := ReadComment(rec)
		if err != nil {
			return fmt.Errorf("comment_store: decode rec[%d]: %w", i, err)
		}
		cp := c
		s.primary[string(keyBuf)] = &cp
		// Rebuild byRoot.
		rootKey := s.rootKeyOf(&cp)
		s.byRoot[rootKey] = append(s.byRoot[rootKey], &cp)
	}
	return nil
}

