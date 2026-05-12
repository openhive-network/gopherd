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
// commentPK is the struct key used by CommentStore's maps. Go hashes
// the contents of both strings natively, eliminating the per-lookup
// commentMapKey alloc that previously dominated cashout-heavy blocks
// (~2.4 GB / 4M-block run).
type commentPK struct {
	author   string // auth.AccountName underlying string
	permlink string
}

func newCommentPK(author auth.AccountName, permlink string) commentPK {
	return commentPK{author: string(author), permlink: permlink}
}

type CommentStore struct {
	// db is consulted only for db.UndoEnabled in Put/Delete to decide
	// whether to capture an undo snapshot.
	db      *state.Database
	primary map[commentPK]*Comment
	byRoot  map[commentPK][]*Comment
	undoLog []commentUndo
	// freeComment is a freelist of *Comment slots reclaimed at
	// CommitPoint / RestoreToPoint when the prev pointers in undo
	// entries become unreachable. Put pulls from here before allocating
	// a fresh struct, eliminating the per-Put heap allocation that
	// dominated the cashout-heavy alloc profile (~2 GB / 4M blocks).
	freeComment []*Comment
}

type commentUndo struct {
	key  commentPK // primary key
	prev *Comment  // nil = the row didn't exist before this mutation
	cur  *Comment  // pointer that was added to byRoot[curInRoot] in the
	// post-mutation state; identity-equal removal is O(N) but
	// avoids the per-comparison commentKey allocation.
	prevInRoot    commentPK // root_key of `prev` (for restoring to byRoot)
	curInRoot     commentPK // root_key of the post-mutation row (for removing from byRoot)
	prevInRootHad bool      // distinguishes zero-value from absent
	curInRootHad  bool      // distinguishes zero-value from absent
	// inPlaceMutate=true means Put kept the slot pointer (no swap) and
	// stamped the new value over the prev. Restore reverts via
	// `*slot = snap`. Used when prev exists + rootKey unchanged — most
	// vote-triggered PutComments. Replaces the freelist-pop + 256-byte
	// struct copy + map assign + byRoot in-place swap with a single
	// `*prev = *c`.
	inPlaceMutate bool
	snap          Comment // pre-mutation value when inPlaceMutate=true
}

const commentStoreSubsystemKey = "comment_store"

// EnsureCommentStore returns the database's CommentStore, creating on
// first call.
func EnsureCommentStore(s *state.Session) *CommentStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotComment); x != nil {
		return x.(*CommentStore)
	}
	if existing := db.GetSubsystem(commentStoreSubsystemKey); existing != nil {
		if cs, ok := existing.(*CommentStore); ok {
			db.SetHotSubsystem(hotSlotComment, cs)
			return cs
		}
	}
	cs := newCommentStore()
	cs.db = db
	db.RegisterSubsystem(commentStoreSubsystemKey, cs, cs)
	db.SetHotSubsystem(hotSlotComment, cs)
	return cs
}

func newCommentStore() *CommentStore {
	return &CommentStore{
		primary:     make(map[commentPK]*Comment, 4096),
		byRoot:      make(map[commentPK][]*Comment, 1024),
		undoLog:     make([]commentUndo, 0, 64),
		freeComment: make([]*Comment, 0, 256),
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
		if e.inPlaceMutate {
			// Fast path undo: same slot, stamp the snap back.
			if cur, ok := s.primary[e.key]; ok {
				*cur = e.snap
			}
			continue
		}
		// Reverse the byRoot membership change first.
		if e.curInRootHad && e.cur != nil {
			// Mutation added e.cur with cur root — remove it.
			s.byRootRemove(e.curInRoot, e.cur)
			// The displaced *Comment becomes unreachable once it's
			// out of primary AND byRoot — recycle into freelist.
			if cur, ok := s.primary[e.key]; ok && cur == e.cur && e.prev != e.cur {
				// primary still points to e.cur → it gets overwritten below
				s.freeComment = append(s.freeComment, e.cur)
			} else if !ok {
				s.freeComment = append(s.freeComment, e.cur)
			}
		}
		if e.prev != nil {
			s.primary[e.key] = e.prev
			if e.prevInRootHad {
				// Restore prev into byRoot (if not already there).
				s.byRoot[e.prevInRoot] = append(s.byRoot[e.prevInRoot], e.prev)
			}
		} else {
			delete(s.primary, e.key)
		}
	}
	s.undoLog = s.undoLog[:snap]
}

// CommitPoint releases accumulated undo entries — called from
// Session.Commit. Every prev pointer in the undo log becomes
// unreachable (it was displaced by a Put or removed by a Delete);
// reclaim into freeComment for reuse.
func (s *CommentStore) CommitPoint() {
	for i := range s.undoLog {
		e := &s.undoLog[i]
		if e.prev != nil {
			// `prev` is reachable only via this undo entry now — recycle.
			s.freeComment = append(s.freeComment, e.prev)
			e.prev = nil
		}
		e.cur = nil
	}
	s.undoLog = s.undoLog[:0]
	if cap(s.freeComment) > 4096 && len(s.freeComment) > 4096 {
		s.freeComment = s.freeComment[:4096]
	}
}

// rootKeyOf returns the byRoot map key for a comment's RootAuthor/Permlink.
func (s *CommentStore) rootKeyOf(c *Comment) commentPK {
	rootAuthor := c.RootAuthor
	rootPermlink := c.RootPermlink
	if rootAuthor == "" {
		rootAuthor = c.Author
		rootPermlink = c.Permlink
	}
	return newCommentPK(rootAuthor, rootPermlink)
}

// byRootRemove drops the comment whose pointer matches `target` from
// byRoot[rootKey]. No-op if not present. Pointer identity (not key
// equality) is the matching predicate so the inner loop is a cheap
// integer compare rather than per-iteration commentKey() allocation
// + string compare. byRoot stores the SAME pointer that primary[]
// stores for each comment, so identity is the correct predicate.
func (s *CommentStore) byRootRemove(rootKey commentPK, target *Comment) {
	list := s.byRoot[rootKey]
	for i, c := range list {
		if c == target {
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
	_, ok := s.primary[newCommentPK(author, permlink)]
	return ok
}

// Get returns a value-copy of the comment for (author, permlink).
func (s *CommentStore) Get(author auth.AccountName, permlink string) (Comment, bool) {
	c, ok := s.primary[newCommentPK(author, permlink)]
	if !ok {
		return Comment{}, false
	}
	return *c, true
}

// GetRef returns a read-only pointer to the comment slot. Caller MUST
// NOT mutate the returned struct (use Mutate or Put). Aliases internal
// storage. Skips the ~256-byte Comment value-copy that Get pays.
func (s *CommentStore) GetRef(author auth.AccountName, permlink string) (*Comment, bool) {
	c, ok := s.primary[newCommentPK(author, permlink)]
	return c, ok
}

// Mutate fetches the live *Comment for (author, permlink) and calls fn
// to apply in-place mutations. Captures a value-snapshot for undo when
// UndoEnabled; rolls back via *slot = snap.
//
// IMPORTANT: fn MUST NOT change c.RootAuthor / c.RootPermlink — the
// byRoot index is keyed off the root and the in-place path here
// deliberately skips byRoot bookkeeping. Vote-evaluator's per-vote
// modifications (NetRshares, AbsRshares, VoteRshares, VoteCount,
// CashoutTime, WasPaid, etc.) all leave the root unchanged so this
// constraint is satisfied. For cross-thread reparenting use Put.
//
// Replaces the Get (256-byte value copy) + modify + Put (in-place
// mutate via the same slot) pattern with a single in-place mutate +
// optional snap. Returns false if the row doesn't exist.
func (s *CommentStore) Mutate(author auth.AccountName, permlink string, fn func(*Comment)) bool {
	key := newCommentPK(author, permlink)
	cur, ok := s.primary[key]
	if !ok {
		return false
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, commentUndo{
			key:           key,
			inPlaceMutate: true,
			snap:          *cur,
		})
	}
	fn(cur)
	return true
}

// Put inserts or replaces. Updates byRoot membership: removes prev from
// its old root list (if root changed), adds new to its current root list.
//
// Allocation: pulls a *Comment slot from the freelist when available
// (refilled at CommitPoint / RestoreToPoint); otherwise allocates fresh.
func (s *CommentStore) Put(c *Comment) {
	if c.RootAuthor == "" {
		c.RootAuthor = c.Author
		c.RootPermlink = c.Permlink
	}
	key := newCommentPK(c.Author, c.Permlink)
	prev := s.primary[key]

	// Fast path: modify-in-place when prev exists AND rootKey didn't
	// change. Skips the freelist pop, the *cp = *c struct copy, the
	// byRoot in-place pointer swap, and the primary map assign. The
	// path captures a snap for undo when UndoEnabled — Restore replays
	// `*slot = snap` (same shape as WitnessStore in-place mutate).
	// This covers vote-triggered PutComments (rootKey is fixed at
	// create-time and votes only mutate per-comment fields).
	if prev != nil {
		curRoot := s.rootKeyOf(c)
		prevRoot := s.rootKeyOf(prev)
		if curRoot == prevRoot {
			if s.db.UndoEnabled {
				s.undoLog = append(s.undoLog, commentUndo{
					key:           newCommentPK(c.Author, c.Permlink),
					inPlaceMutate: true,
					snap:          *prev,
				})
			}
			// Skip the self-copy when caller already mutated via the
			// slot pointer (GetCommentRef + in-place mutate + Put).
			if prev != c {
				*prev = *c
			}
			return
		}
	}

	var cp *Comment
	if n := len(s.freeComment); n > 0 {
		cp = s.freeComment[n-1]
		s.freeComment = s.freeComment[:n-1]
	} else {
		cp = new(Comment)
	}
	*cp = *c
	curRoot := s.rootKeyOf(cp)
	var prevRoot commentPK
	prevHad := false
	if prev != nil {
		prevRoot = s.rootKeyOf(prev)
		prevHad = true
		if prevRoot == curRoot {
			// In-thread overwrite (the common case for vote-triggered
			// PutComment): swap the pointer in byRoot[curRoot] in place
			// — saves the byRoot map mutation + the append's slice
			// growth check, and reduces the O(N) scan to a single pass
			// instead of one for Remove and another for Append+grow.
			list := s.byRoot[curRoot]
			for i := range list {
				if list[i] == prev {
					list[i] = cp
					break
				}
			}
		} else {
			// Cross-thread move (rare): traditional Remove + Add.
			s.byRootRemove(prevRoot, prev)
			s.byRoot[curRoot] = append(s.byRoot[curRoot], cp)
		}
	} else {
		s.byRoot[curRoot] = append(s.byRoot[curRoot], cp)
	}
	s.primary[key] = cp

	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, commentUndo{
			key:           key,
			prev:          prev,
			cur:           cp,
			prevInRoot:    prevRoot,
			curInRoot:     curRoot,
			prevInRootHad: prevHad,
			curInRootHad:  true,
		})
	} else if prev != nil {
		// Without undo, the displaced prev pointer is unreachable —
		// recycle it immediately into the freelist (Save the alloc that
		// CommitPoint would otherwise do).
		s.freeComment = append(s.freeComment, prev)
	}
}

// Delete removes (author, permlink). Captures undo state.
func (s *CommentStore) Delete(author auth.AccountName, permlink string) {
	key := newCommentPK(author, permlink)
	prev, ok := s.primary[key]
	if !ok {
		return
	}
	prevRoot := s.rootKeyOf(prev)
	s.byRootRemove(prevRoot, prev)
	delete(s.primary, key)
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, commentUndo{
			key:           key,
			prev:          prev,
			cur:           nil,
			prevInRoot:    prevRoot,
			prevInRootHad: true,
		})
	} else {
		// No undo needed — slot is unreachable, recycle.
		s.freeComment = append(s.freeComment, prev)
	}
}

// RangeByRoot iterates every comment in the discussion rooted at
// (rootAuthor, rootPermlink). Order is name-ascending (stable across
// runs) — sorting happens once at iter time, which is amortized over
// per-comment work in the cashout. Returning false from fn stops.
func (s *CommentStore) RangeByRoot(rootAuthor auth.AccountName, rootPermlink string, fn func(c *Comment) (bool, error)) error {
	rootKey := newCommentPK(rootAuthor, rootPermlink)
	list := s.byRoot[rootKey]
	if len(list) == 0 {
		return nil
	}
	// Sort by (author, permlink) so iteration order matches the K/V
	// backend's by-root prefix scan.
	sorted := append([]*Comment(nil), list...)
	sort.Slice(sorted, func(i, j int) bool {
		ai, aj := string(sorted[i].Author), string(sorted[j].Author)
		if ai != aj {
			return ai < aj
		}
		return sorted[i].Permlink < sorted[j].Permlink
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
	keys := make([]commentPK, 0, len(s.primary))
	for k := range s.primary {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].author != keys[j].author {
			return keys[i].author < keys[j].author
		}
		return keys[i].permlink < keys[j].permlink
	})
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

// MarshalSnapshot serializes the primary comment map to w. The
// (author, permlink) pair is the implicit primary key — we don't need
// to write a separate key because the record itself carries them.
// Format:
//
//	magic | version | count | (varuint(rec) | AppendComment(rec))*
//
// Entries emitted in (author, permlink) ascending order.
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
	keys := make([]commentPK, 0, len(s.primary))
	for k := range s.primary {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].author != keys[j].author {
			return keys[i].author < keys[j].author
		}
		return keys[i].permlink < keys[j].permlink
	})
	var (
		varBuf [binary.MaxVarintLen64]byte
		recBuf []byte
	)
	for _, k := range keys {
		cp := s.primary[k]
		recBuf = AppendComment(recBuf[:0], cp)
		n := binary.PutUvarint(varBuf[:], uint64(len(recBuf)))
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
	s.primary = make(map[commentPK]*Comment, count)
	s.byRoot = make(map[commentPK][]*Comment, count/8+1)
	s.undoLog = s.undoLog[:0]
	for i := uint32(0); i < count; i++ {
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
		key := newCommentPK(cp.Author, cp.Permlink)
		s.primary[key] = &cp
		// Rebuild byRoot.
		rootKey := s.rootKeyOf(&cp)
		s.byRoot[rootKey] = append(s.byRoot[rootKey], &cp)
	}
	return nil
}

