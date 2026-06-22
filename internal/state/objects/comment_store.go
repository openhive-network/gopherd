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
//
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

	// cold is the on-disk tier (nil = disabled, all-RAM behavior). Frozen/
	// paid comments evicted from RAM live here, keyed by commentKey (primary,
	// table 0x01) and commentByRootKey (by-root index, table 0x02).
	cold coldStore
	// coldTotal counts all comments resident only in the cold tier; a RAM
	// miss probes the cold tier only when coldTotal > 0 (so the all-RAM and
	// eviction-off paths pay zero disk overhead).
	coldTotal int
	// coldRoot[rootKey] = number of this thread's comments in the cold tier;
	// RangeByRoot skips the cold by-root scan when 0 (live threads → pure RAM).
	coldRoot map[commentPK]int
	// evictQueue holds resident comments that are eviction CANDIDATES, in
	// roughly creation/freeze order. Populated on new-comment Put and on
	// promote (so a re-accessed cold comment becomes a candidate again).
	// Bounded: entries are popped when evicted, so the queue size tracks the
	// resident-but-not-yet-evicted population, not the full comment count.
	evictQueue  []commentPK
	residentCap int
	// coldByRootEnabled controls whether evictions/spills write the by-root
	// cold index (and bump coldRoot). The by-root cold index exists ONLY to
	// serve RangeByRoot's cold scan, whose only consensus caller is the
	// PRE-HF17 cashout sibling-walk (comment_cashout.go); the HF17+ cashout
	// path pays each comment individually and never walks by-root. So once HF17
	// is active the by-root cold index is write-only dead weight (~half of all
	// cold writes + the dominant snapshot-load cost). The driver sets this
	// false for post-HF17 blocks. Default true preserves pre-HF17 correctness.
	coldByRootEnabled bool
	// coldBloom answers "is this comment DEFINITELY not in the cold tier?"
	// so the hot path (every new comment does Has() → RAM-miss) skips the
	// Pebble probe for genuinely-new keys. Add-only (promotion leaves a
	// false-positive that costs one wasted, correct probe). nil until cold
	// tier attached.
	bloom *coldBloom

	// Reusable scratch buffers for the cold-tier eviction/promote hot path.
	// bset (pebble batch.Set) and bloom.add/coldKV.get all COPY or HASH the
	// key/val rather than retaining the slice, so a single grow-once buffer
	// can back every per-evict serialization. Eliminates ~6 allocations per
	// eviction (AppendComment + 2 cold keys ×(commentKey+tableKey) + bloom
	// key) — the dominant alloc source at block 16M (cap=1.5M): AppendComment
	// 9.8GB + commentKey/byRootKey/tableKey ~16GB / 60s.
	scratchBlob []byte
	scratchKey  []byte
	scratchKey2 []byte
}

// appendColdPrimaryKey writes coldPrimaryKey(author,permlink) into dst[:0]
// without the intermediate commentKey/tableKey allocations.
func appendColdPrimaryKey(dst []byte, author auth.AccountName, permlink string) []byte {
	dst = append(dst, coldTableCommentPrimary)
	dst = state.AppendAccountName(dst, string(author))
	return append(dst, permlink...)
}

// appendColdByRootKey writes coldByRootKeyOf(c) into dst[:0] alloc-free.
func appendColdByRootKey(dst []byte, c *Comment) []byte {
	ra, rp := c.RootAuthor, c.RootPermlink
	if ra == "" {
		ra, rp = c.Author, c.Permlink
	}
	dst = append(dst, coldTableCommentByRoot)
	dst = state.AppendAccountName(dst, string(ra))
	dst = append(dst, rp...)
	dst = append(dst, 0x00)
	dst = state.AppendAccountName(dst, string(c.Author))
	return append(dst, c.Permlink...)
}

// appendCommentKeyRaw writes commentKey(author,permlink) into dst[:0]
// (used for the bloom key, which is hashed not retained).
func appendCommentKeyRaw(dst []byte, author auth.AccountName, permlink string) []byte {
	dst = state.AppendAccountName(dst, string(author))
	return append(dst, permlink...)
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
		primary:           make(map[commentPK]*Comment, 4096),
		byRoot:            make(map[commentPK][]*Comment, 1024),
		undoLog:           make([]commentUndo, 0, 64),
		freeComment:       make([]*Comment, 0, 256),
		coldRoot:          make(map[commentPK]int),
		coldByRootEnabled: true,
	}
}

// SetColdByRootEnabled controls whether eviction/spill writes the by-root cold
// index. Set false once HF17 is active (post-HF17 never reads it). See the field
// doc. Idempotent; cheap to call per block.
func (s *CommentStore) SetColdByRootEnabled(b bool) { s.coldByRootEnabled = b }

// SetColdTier attaches (or detaches, with nil) the on-disk cold tier. Must be
// called before any eviction. Unset → pure in-RAM behavior.
func (s *CommentStore) SetColdTier(c coldStore) {
	s.cold = c
	if c != nil && s.bloom == nil {
		s.bloom = newColdBloom(100_000_000) // ~120MB, ~1% FP at 100M cold keys
	}
}

// coldMaybe reports whether (author, permlink) might be in the cold tier.
// false ⇒ definitely absent ⇒ skip the Pebble probe. Always true when no
// bloom (shouldn't happen once cold attached) so correctness never depends
// on the bloom.
func (s *CommentStore) coldMaybe(author auth.AccountName, permlink string) bool {
	if s.coldTotal == 0 {
		return false
	}
	if s.bloom == nil {
		return true
	}
	// Alloc-free: build the bloom key into reused scratch (maybePresent only
	// hashes it, never retains). commentKey() here was ~2.2GB/60s at 16M — it
	// runs on every RAM-miss / Has probe in the dense era. scratchKey2 is the
	// bloom-key buffer (also used by Evict, which never interleaves with this).
	s.scratchKey2 = appendCommentKeyRaw(s.scratchKey2[:0], author, permlink)
	return s.bloom.maybePresent(s.scratchKey2)
}

// SetResidentCap sets the soft cap on RAM-resident comments. 0 disables
// eviction (the queue is not populated). EvictFrozen evicts down toward this
// cap at block boundaries.
func (s *CommentStore) SetResidentCap(n int) { s.residentCap = n }

// EvictFrozen evicts the OLDEST comments — and their votes via cvs — from RAM
// to the cold tier until the resident count is at/under the cap or the budget
// is spent. MUST be called only at a safe point (after a block commit:
// UndoEnabled false, undo log empty, no live *Comment alias). Returns the
// number evicted.
//
// Eviction is by AGE, NOT by frozen status — exactly as a hived consensus node
// lets the OS page out cold pages of its memory-mapped shared_memory regardless
// of whether a comment is still in its cashout window. Pinning every live
// (CashoutTime!=0) comment in the Go heap is what blew RAM up to the ~8M-live-
// comment working set; a consensus node does not need them resident. A live
// comment that is later voted on or cashed out is promoted back on access
// (GetRef/Mutate/RangeByRoot all promote from cold) — bit-identically. The
// resident set therefore tracks the genuinely-hot comments (recent + currently
// cashing out), bounded by the cap.
//
// evictQueue holds comments in creation/promotion order (oldest at the front),
// so popping the front evicts the oldest — the least likely to be touched
// (vote activity is on recent posts; a due comment is promoted at cashout).
// Stale entries (already evicted / deleted) are dropped. No spin: every
// resident pop evicts.
func (s *CommentStore) EvictFrozen(cvs *CommentVoteStore, budget int) int {
	if s.cold == nil || s.residentCap <= 0 {
		return 0
	}
	evicted := 0
	// Bound the TOTAL scan per call to O(budget), independent of queue length.
	// Deferred-unpaid entries are re-appended to the back; without this bound
	// the scan grew with the queue every block (O(N²)), grinding the dense era
	// to a halt at ~19.x M when the unpaid working set first exceeds the cap.
	scanBudget := budget*8 + 4096
	scanned := 0
	// Hard ceiling: when the UNPAID working set itself exceeds the cap, we can
	// never get under it by evicting only paid comments. Past this margin we
	// MUST evict the oldest comments regardless of WasPaid — a bounded amount of
	// promote-back thrash (the excess only), which is the lesser evil vs. either
	// unbounded RAM growth or the O(N²) defer-rescan blowup. Recomputed each
	// call; the margin lets the genuine cashout-window working set stay resident
	// (no thrash) while capping RAM at hardCap.
	hardCap := s.residentCap + s.residentCap/3 // ~1.33× cap
	for len(s.primary) > s.residentCap && evicted < budget && len(s.evictQueue) > 0 && scanned < scanBudget {
		forceAny := len(s.primary) > hardCap
		key := s.evictQueue[0]
		s.evictQueue = s.evictQueue[1:]
		scanned++
		c, ok := s.primary[key]
		if !ok {
			continue // stale (already evicted/deleted) — drop
		}
		// Prefer to keep an un-cashed-out comment resident: it is GUARANTEED to
		// be read back at its cashout, so evicting it is pure promote-back
		// thrash. Defer it (move to the back) — UNLESS we're past the hard cap,
		// where bounding RAM wins and we evict it anyway. Paid comments are
		// older, so they sit at the FRONT of this age-ordered queue and are hit
		// first; the common path evicts without ever reaching an unpaid one.
		if !c.WasPaid && !forceAny {
			s.evictQueue = append(s.evictQueue, key)
			continue
		}
		author, permlink := auth.AccountName(key.author), key.permlink
		if s.Evict(author, permlink) {
			if cvs != nil {
				cvs.EvictComment(author, permlink)
			}
			evicted++
		}
	}
	// Commit this pass's batched cold-tier writes (comments + their votes) in
	// one Pebble Apply — far cheaper than per-key Set + flush. Must happen
	// before the next block's promote-back reads (those also flush defensively).
	if evicted > 0 {
		s.cold.flushBatch()
	}
	// Periodically compact the queue's backing array so the sliced-off front
	// doesn't pin unbounded memory.
	if len(s.evictQueue) == 0 {
		s.evictQueue = nil
	} else if cap(s.evictQueue) > 4096 && cap(s.evictQueue) > 4*len(s.evictQueue) {
		q := make([]commentPK, len(s.evictQueue))
		copy(q, s.evictQueue)
		s.evictQueue = q
	}
	return evicted
}

// coldKeyOf / coldRootKeyOf build the table-prefixed cold keys for a comment.
func coldPrimaryKey(author auth.AccountName, permlink string) []byte {
	return tableKey(coldTableCommentPrimary, commentKey(author, permlink))
}

func (s *CommentStore) coldByRootKeyOf(c *Comment) []byte {
	rootAuthor, rootPermlink := c.RootAuthor, c.RootPermlink
	if rootAuthor == "" {
		rootAuthor, rootPermlink = c.Author, c.Permlink
	}
	return tableKey(coldTableCommentByRoot, commentByRootKey(rootAuthor, rootPermlink, c.Author, c.Permlink))
}

// coldGet fetches a comment from the cold tier by primary key.
func (s *CommentStore) coldGet(author auth.AccountName, permlink string) (Comment, bool) {
	if s.cold == nil || !s.coldMaybe(author, permlink) {
		return Comment{}, false
	}
	s.scratchKey = appendColdPrimaryKey(s.scratchKey[:0], author, permlink)
	blob, ok := s.cold.get(s.scratchKey)
	if !ok {
		return Comment{}, false
	}
	c, err := ReadComment(blob)
	if err != nil {
		return Comment{}, false
	}
	return c, true
}

// promote pulls a comment from the cold tier into a real RAM slot (primary +
// byRoot), removes its cold entries, and returns the slot. (nil,false) if not
// cold. The returned pointer is a genuine RAM slot so callers can
// alias-and-mutate it exactly as for an always-resident comment.
func (s *CommentStore) promote(author auth.AccountName, permlink string) (*Comment, bool) {
	c, ok := s.coldGet(author, permlink)
	if !ok {
		return nil, false
	}
	var cp *Comment
	if n := len(s.freeComment); n > 0 {
		cp = s.freeComment[n-1]
		s.freeComment = s.freeComment[:n-1]
	} else {
		cp = new(Comment)
	}
	*cp = c
	key := newCommentPK(author, permlink)
	rootKey := s.rootKeyOf(cp)
	s.primary[key] = cp
	s.byRoot[rootKey] = append(s.byRoot[rootKey], cp)
	if s.residentCap > 0 {
		// Re-accessed cold comment is an eviction candidate again.
		s.evictQueue = append(s.evictQueue, key)
	}
	// Drop cold entries + counters (scratch reused — coldGet above already
	// returned, so its buffer use is complete).
	s.scratchKey = appendColdPrimaryKey(s.scratchKey[:0], author, permlink)
	s.cold.del(s.scratchKey)
	s.scratchKey = appendColdByRootKey(s.scratchKey[:0], cp)
	s.cold.del(s.scratchKey)
	s.coldTotal--
	if n := s.coldRoot[rootKey] - 1; n <= 0 {
		delete(s.coldRoot, rootKey)
	} else {
		s.coldRoot[rootKey] = n
	}
	return cp, true
}

// Evict moves one RAM-resident comment to the cold tier. Safe-point only
// (CommitPoint / block boundary, UndoEnabled==false, no alias held). Returns
// true if evicted.
func (s *CommentStore) Evict(author auth.AccountName, permlink string) bool {
	if s.cold == nil {
		return false
	}
	key := newCommentPK(author, permlink)
	c, ok := s.primary[key]
	if !ok {
		return false
	}
	rootKey := s.rootKeyOf(c)
	// Alloc-free: serialize once + build both cold keys into reused scratch
	// buffers. bset copies into the pebble batch, so the buffers are free to
	// reuse immediately after each call.
	s.scratchBlob = AppendComment(s.scratchBlob[:0], c)
	s.scratchKey = appendColdPrimaryKey(s.scratchKey[:0], author, permlink)
	s.cold.bset(s.scratchKey, s.scratchBlob)
	if s.coldByRootEnabled {
		s.scratchKey = appendColdByRootKey(s.scratchKey[:0], c)
		s.cold.bset(s.scratchKey, s.scratchBlob)
	}
	if s.bloom != nil {
		s.scratchKey2 = appendCommentKeyRaw(s.scratchKey2[:0], author, permlink)
		s.bloom.add(s.scratchKey2)
	}
	s.byRootRemove(rootKey, c)
	delete(s.primary, key)
	// Do NOT recycle the evicted slot into freeComment. The freelist is
	// reused by Put and trimmed only at CommitPoint; under eviction (millions
	// evicted, few promoted back) it would grow without bound and retain every
	// evicted *Comment (and its strings) forever — the dominant heap leak
	// (CommentStore.Put 3.94GB + ReadString 2.07GB at block 16M). Dropping the
	// reference lets GC reclaim the slot + its strings immediately; the cold
	// blob on disk is the retained copy.
	s.coldTotal++
	if s.coldByRootEnabled {
		s.coldRoot[rootKey]++
	}
	return true
}

// spillToCold writes a comment DIRECTLY to the cold tier without it ever being
// resident. Used by UnmarshalSnapshot to load a deep snapshot (hundreds of
// millions of comments) without OOMing — frozen/paid comments go straight to
// disk instead of the resident map. Mirrors Evict's cold-write half but skips
// the resident-map removal (the comment was never inserted).
func (s *CommentStore) spillToCold(c *Comment) {
	rootKey := s.rootKeyOf(c)
	s.scratchBlob = AppendComment(s.scratchBlob[:0], c)
	s.scratchKey = appendColdPrimaryKey(s.scratchKey[:0], c.Author, c.Permlink)
	s.cold.bset(s.scratchKey, s.scratchBlob)
	if s.coldByRootEnabled {
		s.scratchKey = appendColdByRootKey(s.scratchKey[:0], c)
		s.cold.bset(s.scratchKey, s.scratchBlob)
	}
	if s.bloom != nil {
		s.scratchKey2 = appendCommentKeyRaw(s.scratchKey2[:0], c.Author, c.Permlink)
		s.bloom.add(s.scratchKey2)
	}
	s.coldTotal++
	if s.coldByRootEnabled {
		s.coldRoot[rootKey]++
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

// Has reports whether (author, permlink) has a comment row in either tier.
func (s *CommentStore) Has(author auth.AccountName, permlink string) bool {
	if _, ok := s.primary[newCommentPK(author, permlink)]; ok {
		return true
	}
	if s.cold != nil && s.coldMaybe(author, permlink) {
		return s.cold.has(coldPrimaryKey(author, permlink))
	}
	return false
}

// Get returns a value-copy of the comment for (author, permlink). Consults the
// cold tier on a RAM miss (read-only; does not promote).
func (s *CommentStore) Get(author auth.AccountName, permlink string) (Comment, bool) {
	c, ok := s.primary[newCommentPK(author, permlink)]
	if ok {
		return *c, true
	}
	return s.coldGet(author, permlink)
}

// GetRef returns a read-only pointer to the comment slot. Caller MUST
// NOT mutate the returned struct (use Mutate or Put). Aliases internal
// storage. Skips the ~256-byte Comment value-copy that Get pays.
func (s *CommentStore) GetRef(author auth.AccountName, permlink string) (*Comment, bool) {
	c, ok := s.primary[newCommentPK(author, permlink)]
	if ok {
		return c, true
	}
	// RAM miss — promote from cold so the caller gets a real mutable slot
	// (preserves the alias-and-mutate-then-Put contract).
	if s.cold != nil && s.coldMaybe(author, permlink) {
		return s.promote(author, permlink)
	}
	return nil, false
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
		// Promote from cold so the in-place mutate + undo snapshot operate on
		// a real RAM slot.
		if s.cold != nil && s.coldMaybe(author, permlink) {
			cur, ok = s.promote(author, permlink)
		}
		if !ok {
			return false
		}
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

	// RAM miss but present in cold (edit/vote on an evicted comment): promote
	// so `prev` is the true prior row — undo captures it and byRoot stays
	// consistent.
	if prev == nil && s.cold != nil && s.coldMaybe(c.Author, c.Permlink) {
		if p, ok := s.promote(c.Author, c.Permlink); ok {
			prev = p
		}
	}

	// New comment → eviction candidate (creation order = age order; EvictFrozen
	// evicts oldest-first). Re-accessed cold comments are re-queued by promote.
	if s.residentCap > 0 && prev == nil {
		s.evictQueue = append(s.evictQueue, key)
	}

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
		// Maybe cold.
		if s.cold != nil && s.coldMaybe(author, permlink) && s.cold.has(coldPrimaryKey(author, permlink)) {
			if s.db.UndoEnabled {
				// Promote so the delete is undoable; fall through to RAM path.
				if p, pok := s.promote(author, permlink); pok {
					prev, ok = p, true
				}
			} else {
				// Delete directly from cold (primary + by-root).
				c, _ := s.coldGet(author, permlink)
				rootKey := s.rootKeyOf(&c)
				s.cold.del(coldPrimaryKey(author, permlink))
				s.cold.del(s.coldByRootKeyOf(&c))
				s.coldTotal--
				if n := s.coldRoot[rootKey] - 1; n <= 0 {
					delete(s.coldRoot, rootKey)
				} else {
					s.coldRoot[rootKey] = n
				}
				return
			}
		}
		if !ok {
			return
		}
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

	// Pure-RAM fast path: no cold members for this thread.
	if s.cold == nil || s.coldRoot[rootKey] == 0 {
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

	// Merge the sorted RAM members with the cold by-root prefix scan (also
	// ascending by (author, permlink) — commentByRootKey orders that way).
	// Disjoint sets → global (author, permlink) order, byte-for-byte the same
	// sequence as an all-RAM RangeByRoot of the same logical thread.
	it, err := s.cold.newPrefixIter(tableKey(coldTableCommentByRoot, CommentByRootPrefix(rootAuthor, rootPermlink)))
	if err != nil {
		return err
	}
	defer it.Close()
	var coldC Comment
	haveCold := false
	peekCold := func() error {
		if it.valid() {
			v, e := it.val()
			if e != nil {
				return e
			}
			c, e := ReadComment(v)
			if e != nil {
				return e
			}
			coldC = c
			haveCold = true
		} else {
			haveCold = false
		}
		return nil
	}
	if err := peekCold(); err != nil {
		return err
	}
	ri := 0
	for ri < len(sorted) || haveCold {
		emitRAM := false
		if ri < len(sorted) && haveCold {
			r := sorted[ri]
			ra, ca := string(r.Author), string(coldC.Author)
			if ra != ca {
				emitRAM = ra < ca
			} else {
				emitRAM = r.Permlink < coldC.Permlink
			}
		} else {
			emitRAM = ri < len(sorted)
		}
		if emitRAM {
			ok, err := fn(sorted[ri])
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			ri++
		} else {
			cc := coldC // stable address for fn (transient; must not be retained)
			ok, err := fn(&cc)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			it.next()
			if err := peekCold(); err != nil {
				return err
			}
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

	if s.cold == nil || s.coldTotal == 0 {
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

	// Merge sorted RAM keys with the cold primary prefix scan (both ascending
	// by (author, permlink); cold primary key = author(16)|permlink).
	it, err := s.cold.newPrefixIter(tableKey(coldTableCommentPrimary, nil))
	if err != nil {
		return err
	}
	defer it.Close()
	var coldC Comment
	haveCold := false
	peekCold := func() error {
		if it.valid() {
			v, e := it.val()
			if e != nil {
				return e
			}
			c, e := ReadComment(v)
			if e != nil {
				return e
			}
			coldC, haveCold = c, true
		} else {
			haveCold = false
		}
		return nil
	}
	if err := peekCold(); err != nil {
		return err
	}
	ri := 0
	for ri < len(keys) || haveCold {
		emitRAM := false
		if ri < len(keys) && haveCold {
			k := keys[ri]
			if k.author != string(coldC.Author) {
				emitRAM = k.author < string(coldC.Author)
			} else {
				emitRAM = k.permlink < coldC.Permlink
			}
		} else {
			emitRAM = ri < len(keys)
		}
		if emitRAM {
			ok, err := fn(s.primary[keys[ri]])
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			ri++
		} else {
			cc := coldC
			ok, err := fn(&cc)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			it.next()
			if err := peekCold(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Count returns the comment count across both tiers.
func (s *CommentStore) Count() int { return len(s.primary) + s.coldTotal }

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
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)+s.coldTotal))
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
	writeRec := func(rec []byte) error {
		n := binary.PutUvarint(varBuf[:], uint64(len(rec)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		_, err := w.Write(rec)
		return err
	}

	if s.cold == nil || s.coldTotal == 0 {
		for _, k := range keys {
			recBuf = AppendComment(recBuf[:0], s.primary[k])
			if err := writeRec(recBuf); err != nil {
				return err
			}
		}
		return nil
	}

	// Streaming 2-way merge of sorted RAM keys + cold primary scan (both
	// ascending by (author, permlink)) → byte-identical to an all-RAM
	// snapshot of the same logical contents. Cold blobs are passed through
	// verbatim (already AppendComment bytes).
	it, err := s.cold.newPrefixIter(tableKey(coldTableCommentPrimary, nil))
	if err != nil {
		return err
	}
	defer it.Close()
	ri := 0
	for ri < len(keys) || it.valid() {
		emitRAM := false
		if ri < len(keys) && it.valid() {
			rk := commentKey(auth.AccountName(keys[ri].author), keys[ri].permlink)
			ck := it.key()[1:] // strip table prefix
			emitRAM = bytesLess(rk, ck)
		} else {
			emitRAM = ri < len(keys)
		}
		if emitRAM {
			recBuf = AppendComment(recBuf[:0], s.primary[keys[ri]])
			if err := writeRec(recBuf); err != nil {
				return err
			}
			ri++
		} else {
			v, err := it.val()
			if err != nil {
				return err
			}
			if err := writeRec(v); err != nil {
				return err
			}
			it.next()
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
	// When a cold tier + resident cap are attached BEFORE load (deep-snapshot
	// resume), spill already-paid (frozen) comments straight to the cold tier
	// instead of loading every comment resident. A 35M-block snapshot holds
	// ~175M comments (~22 GB serialized); loading all resident is ~30 GB+ and
	// OOMs a 31 GB box. Paid comments are never mutated again by consensus
	// (cashout is done), so disk is the right home; only the unpaid working set
	// stays resident. This is what makes warm-start from a deep snapshot viable.
	spill := s.cold != nil && s.residentCap > 0
	residentHint := int(count)
	if spill && residentHint > 4*s.residentCap {
		residentHint = 4 * s.residentCap // expect ~unpaid set resident
	}
	s.primary = make(map[commentPK]*Comment, residentHint)
	s.byRoot = make(map[commentPK][]*Comment, residentHint/8+1)
	s.undoLog = s.undoLog[:0]
	if s.coldRoot == nil {
		s.coldRoot = make(map[commentPK]int)
	}
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
		if spill && c.WasPaid {
			s.spillToCold(&c)
			continue
		}
		// Resident (unpaid working set, or all-RAM when no cold tier).
		// Reconstruct WasVotedOn for snapshots written before that field
		// existed: any accumulated rshares implies the comment was voted on
		// (the comment_options tightening gate reads WasVotedOn). Harmless on
		// newer snapshots — the value is already set, the OR keeps it.
		c.WasVotedOn = c.WasVotedOn || c.AbsRshares != 0 || c.NetRshares != 0 || c.VoteRshares != 0
		cp := c
		key := newCommentPK(cp.Author, cp.Permlink)
		s.primary[key] = &cp
		rootKey := s.rootKeyOf(&cp)
		s.byRoot[rootKey] = append(s.byRoot[rootKey], &cp)
		if s.residentCap > 0 {
			s.evictQueue = append(s.evictQueue, key)
		}
	}
	return nil
}
