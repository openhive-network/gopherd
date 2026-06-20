package objects

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// CommentVoteStore is the in-memory typed container for the
// CommentVote table — replaces the K/V path including the by-comment
// secondary range scan.
//
// Why typed-store: distributeCuratorPool walked the by-comment Range
// THEN called GetCommentVote per voter (one random K/V probe each),
// and deleteCommentVotes did the same for cleanup. With ~50M votes by
// 3.5M blocks, those probes dominated the cashout-heavy blocks. The
// per-comment slice gives O(votes_on_post) iteration with zero btree
// probes; the primary map gives O(1) GetCommentVote.
//
// Invariants
//
//   - primary[(voter, author, permlink)] points to canonical *CommentVote.
//   - byComment[(author, permlink)] holds every voter on that comment as
//     pointers identical to those in primary.
//   - byVoter[voter] holds every comment that voter has voted on (used
//     by ClearWitnessVotesForAccount-style sweeps to find a voter's
//     full vote set).
//   - undoLog captures pre-mutation state for snapshot rollback.
// commentVotePK is the struct primary key for the CommentVote store —
// Go hashes the three string fields natively, avoiding the per-lookup
// commentVoteMapKey string-concat allocation.
type commentVotePK struct {
	voter    string
	author   string
	permlink string
}

func newCommentVotePK(voter, author auth.AccountName, permlink string) commentVotePK {
	return commentVotePK{
		voter:    string(voter),
		author:   string(author),
		permlink: permlink,
	}
}

type CommentVoteStore struct {
	db        *state.Database
	primary   map[commentVotePK]*CommentVote
	byComment map[commentPK][]*CommentVote
	undoLog   []commentVoteUndo
	// freeCV recycles *CommentVote slots that became unreachable after
	// CommitPoint/RestoreToPoint. Put pulls from here before allocating
	// a fresh struct, eliminating the per-Put heap allocation.
	freeCV []*CommentVote

	// cold is the on-disk tier (nil = disabled, all-RAM behavior). Votes
	// evicted from RAM live here, keyed by commentVoteKey (primary) and
	// commentVoteByCommentKey (secondary). See coldkv.go +
	// project_disk_backed_stores_2026_05_31.
	cold coldStore
	// coldCount[cKey] = number of this comment's votes currently resident
	// only in the cold tier. RangeByComment/DeleteAllForComment skip the
	// cold scan entirely when the count is 0 (the live-comment common case),
	// so hot threads pay zero disk overhead.
	coldCount map[commentPK]int

	// Reusable scratch for the per-vote cold-write hot path (coldPutVote):
	// bset copies into the pebble batch, so one grow-once buffer backs every
	// eviction. Eliminates AppendCommentVote + 2 vote-key allocs per evicted
	// vote.
	scratchBlobV []byte
	scratchKeyV  []byte
}

// appendColdVotePrimaryKey writes tableKey(coldTableVotePrimary,
// commentVoteKey(voter,author,permlink)) into dst[:0] alloc-free.
func appendColdVotePrimaryKey(dst []byte, voter, author auth.AccountName, permlink string) []byte {
	dst = append(dst, coldTableVotePrimary)
	dst = state.AppendAccountName(dst, string(voter))
	dst = state.AppendAccountName(dst, string(author))
	return append(dst, permlink...)
}

// appendColdVoteByCommentKey writes tableKey(coldTableVoteByComment,
// commentVoteByCommentKey(author,permlink,voter)) into dst[:0] alloc-free.
func appendColdVoteByCommentKey(dst []byte, author auth.AccountName, permlink string, voter auth.AccountName) []byte {
	dst = append(dst, coldTableVoteByComment)
	dst = state.AppendAccountName(dst, string(author))
	dst = append(dst, permlink...)
	dst = append(dst, 0)
	return state.AppendAccountName(dst, string(voter))
}

type commentVoteUndo struct {
	key  commentVotePK // primary key
	prev *CommentVote  // nil = the row didn't exist before this mutation
	cur  *CommentVote  // post-mutation pointer (nil = mutation was a Delete)
	// commentKey identifies the (author, permlink) slice in byComment.
	commentKey commentPK
}

const commentVoteStoreSubsystemKey = "comment_vote_store"

// EnsureCommentVoteStore returns the database's CommentVoteStore.
func EnsureCommentVoteStore(s *state.Session) *CommentVoteStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotCommentVote); x != nil {
		return x.(*CommentVoteStore)
	}
	if existing := db.GetSubsystem(commentVoteStoreSubsystemKey); existing != nil {
		if cs, ok := existing.(*CommentVoteStore); ok {
			db.SetHotSubsystem(hotSlotCommentVote, cs)
			return cs
		}
	}
	cs := newCommentVoteStore()
	cs.db = db
	db.RegisterSubsystem(commentVoteStoreSubsystemKey, cs, cs)
	db.SetHotSubsystem(hotSlotCommentVote, cs)
	return cs
}

func newCommentVoteStore() *CommentVoteStore {
	return &CommentVoteStore{
		primary:   make(map[commentVotePK]*CommentVote, 8192),
		byComment: make(map[commentPK][]*CommentVote, 1024),
		undoLog:   make([]commentVoteUndo, 0, 64),
		freeCV:    make([]*CommentVote, 0, 512),
		coldCount: make(map[commentPK]int),
	}
}

// SetColdTier attaches (or detaches, with nil) the on-disk cold tier. Must be
// called before any eviction. Safe to leave unset → pure in-RAM behavior.
func (s *CommentVoteStore) SetColdTier(c coldStore) { s.cold = c }

// coldHas reports whether any of this comment's votes are in the cold tier.
func (s *CommentVoteStore) coldHas(cKey commentPK) bool {
	return s.cold != nil && s.coldCount[cKey] > 0
}

// coldGetVote fetches a single vote from the cold tier by primary key.
func (s *CommentVoteStore) coldGetVote(voter, author auth.AccountName, permlink string) (CommentVote, bool) {
	if s.cold == nil {
		return CommentVote{}, false
	}
	k := tableKey(coldTableVotePrimary, commentVoteKey(voter, author, permlink))
	blob, ok := s.cold.get(k)
	if !ok {
		return CommentVote{}, false
	}
	cv, err := ReadCommentVote(blob)
	if err != nil {
		return CommentVote{}, false
	}
	return cv, true
}

// coldPutVote writes a vote into the cold tier (primary + by-comment index)
// and bumps the per-comment cold counter.
func (s *CommentVoteStore) coldPutVote(cv *CommentVote) {
	s.scratchBlobV = AppendCommentVote(s.scratchBlobV[:0], cv)
	s.scratchKeyV = appendColdVotePrimaryKey(s.scratchKeyV[:0], cv.Voter, cv.Author, cv.Permlink)
	s.cold.bset(s.scratchKeyV, s.scratchBlobV)
	s.scratchKeyV = appendColdVoteByCommentKey(s.scratchKeyV[:0], cv.Author, cv.Permlink, cv.Voter)
	s.cold.bset(s.scratchKeyV, s.scratchBlobV)
	s.coldCount[newCommentPK(cv.Author, cv.Permlink)]++
}

// coldDelVote removes a single vote from the cold tier and decrements the
// per-comment counter.
func (s *CommentVoteStore) coldDelVote(voter, author auth.AccountName, permlink string) {
	if s.cold == nil {
		return
	}
	cKey := newCommentPK(author, permlink)
	if s.coldCount[cKey] == 0 {
		return
	}
	s.cold.del(tableKey(coldTableVotePrimary, commentVoteKey(voter, author, permlink)))
	s.cold.del(tableKey(coldTableVoteByComment, commentVoteByCommentKey(author, permlink, voter)))
	if n := s.coldCount[cKey] - 1; n <= 0 {
		delete(s.coldCount, cKey)
	} else {
		s.coldCount[cKey] = n
	}
}

// Evict moves a single vote from the RAM tier to the cold tier. No-op if the
// vote is not resident or the cold tier is disabled. MUST be called only at a
// safe point (CommitPoint / block boundary, UndoEnabled==false, no outstanding
// savepoint) so no undo entry or caller alias references the evicted slot.
// Returns true if a vote was evicted.
func (s *CommentVoteStore) Evict(voter, author auth.AccountName, permlink string) bool {
	if s.cold == nil {
		return false
	}
	key := newCommentVotePK(voter, author, permlink)
	cv, ok := s.primary[key]
	if !ok {
		return false
	}
	cKey := newCommentPK(author, permlink)
	s.coldPutVote(cv)
	s.byCommentRemove(cKey, cv)
	delete(s.primary, key)
	// Do NOT recycle into freeCV — under eviction the freelist would grow
	// unbounded and retain every evicted vote. Let GC reclaim it (cold blob
	// is the retained copy).
	return true
}

// EvictComment moves every RAM-resident vote on (author, permlink) to the cold
// tier. Used by the coordinated eviction pass when a comment is frozen/paid.
// Safe-point only (see Evict). Returns the number of votes evicted.
func (s *CommentVoteStore) EvictComment(author auth.AccountName, permlink string) int {
	if s.cold == nil {
		return 0
	}
	cKey := newCommentPK(author, permlink)
	list := s.byComment[cKey]
	n := len(list)
	if n == 0 {
		return 0
	}
	for _, cv := range list {
		s.coldPutVote(cv)
		delete(s.primary, newCommentVotePK(cv.Voter, cv.Author, cv.Permlink))
		// Not recycled into freeCV (would grow unbounded under eviction) —
		// GC reclaims; cold blob is the retained copy.
	}
	delete(s.byComment, cKey)
	return n
}

func (s *CommentVoteStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *CommentVoteStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		// Undo the byComment membership change first.
		if e.cur != nil && e.prev == nil {
			// Mutation added a new entry — remove it from byComment.
			s.byCommentRemove(e.commentKey, e.cur)
		} else if e.cur == nil && e.prev != nil {
			// Mutation deleted an entry — re-add it.
			s.byComment[e.commentKey] = append(s.byComment[e.commentKey], e.prev)
		} else if e.cur != nil && e.prev != nil {
			// Modification — replace the slice's pointer with prev.
			list := s.byComment[e.commentKey]
			for j, p := range list {
				if p == e.cur {
					list[j] = e.prev
					break
				}
			}
		}
		// Restore primary.
		if e.prev != nil {
			s.primary[e.key] = e.prev
		} else {
			delete(s.primary, e.key)
		}
	}
	s.undoLog = s.undoLog[:snap]
}

// CommitPoint releases accumulated undo entries — every prev/cur slot
// that became unreachable is recycled into freeCV for reuse by Put.
func (s *CommentVoteStore) CommitPoint() {
	for i := range s.undoLog {
		e := &s.undoLog[i]
		// `prev` is unreachable when the row was overwritten (cur != prev)
		// or deleted (cur == nil). `cur` is unreachable on Delete (cur == nil)
		// — but in that case we have nothing to recycle. The post-Put
		// `cur` pointer remains in primary, NOT recyclable yet.
		if e.prev != nil && e.prev != e.cur {
			s.freeCV = append(s.freeCV, e.prev)
		}
		e.prev = nil
		e.cur = nil
	}
	s.undoLog = s.undoLog[:0]
	if cap(s.freeCV) > 8192 && len(s.freeCV) > 8192 {
		s.freeCV = s.freeCV[:8192]
	}
}

// byCommentRemove drops the entry whose pointer matches `target` from
// byComment[commentKey]. Pointer-identity matching avoids the per-loop
// commentVoteKey() allocation that the previous string-equality form
// performed (~2GB of needless allocations across a 1M-block window).
func (s *CommentVoteStore) byCommentRemove(commentKey commentPK, target *CommentVote) {
	list := s.byComment[commentKey]
	for i, cv := range list {
		if cv == target {
			list[i] = list[len(list)-1]
			list = list[:len(list)-1]
			if len(list) == 0 {
				delete(s.byComment, commentKey)
			} else {
				s.byComment[commentKey] = list
			}
			return
		}
	}
}

// promoteVote pulls a vote from the cold tier into RAM (primary + byComment),
// removes it from the cold tier, and returns the resident slot. Returns
// (nil,false) if the vote is not in the cold tier. The promoted slot is a real
// RAM pointer so callers can alias-and-mutate it exactly as for a vote that
// was resident all along.
func (s *CommentVoteStore) promoteVote(voter, author auth.AccountName, permlink string) (*CommentVote, bool) {
	cv, ok := s.coldGetVote(voter, author, permlink)
	if !ok {
		return nil, false
	}
	var cp *CommentVote
	if n := len(s.freeCV); n > 0 {
		cp = s.freeCV[n-1]
		s.freeCV = s.freeCV[:n-1]
	} else {
		cp = new(CommentVote)
	}
	*cp = cv
	key := newCommentVotePK(voter, author, permlink)
	cKey := newCommentPK(author, permlink)
	s.primary[key] = cp
	s.byComment[cKey] = append(s.byComment[cKey], cp)
	s.coldDelVote(voter, author, permlink)
	return cp, true
}

// coldDelAllForComment removes every cold-tier vote on (author, permlink),
// both the primary and by-comment index entries, and clears the counter.
func (s *CommentVoteStore) coldDelAllForComment(author auth.AccountName, permlink string) {
	if s.cold == nil {
		return
	}
	cKey := newCommentPK(author, permlink)
	if s.coldCount[cKey] == 0 {
		return
	}
	// Enumerate voters via the by-comment index (its blobs decode to the
	// vote, giving the voter) so we can delete the matching primary keys.
	byCommentPrefix := tableKey(coldTableVoteByComment, CommentVoteByCommentPrefix(author, permlink))
	var voters []auth.AccountName
	var byCommentKeys [][]byte
	s.cold.rangePrefix(byCommentPrefix, func(k, val []byte) bool {
		if cv, err := ReadCommentVote(val); err == nil {
			voters = append(voters, cv.Voter)
		}
		// Collect the index key so we can POINT-delete it below instead of
		// issuing a DeleteRange. DeleteRange here was the dominant replay
		// cost in the cold-tier-thrash CPU/alloc profile: its range
		// tombstones force pebble's keyspan fragmenter to rebuild fragments
		// on EVERY subsequent cold Get (promote), ~19 GB of garbage + a GC
		// storm. Point tombstones don't go through the fragmenter. The
		// rangePrefix callback's key is only valid during the call — copy it.
		byCommentKeys = append(byCommentKeys, append([]byte(nil), k...))
		return true
	})
	for _, v := range voters {
		s.cold.del(tableKey(coldTableVotePrimary, commentVoteKey(v, author, permlink)))
	}
	for _, k := range byCommentKeys {
		s.cold.del(k)
	}
	delete(s.coldCount, cKey)
}

// promoteAllForComment pulls every cold-tier vote on (author, permlink) into
// RAM. Used by DeleteAllForComment under UndoEnabled so the subsequent RAM
// delete loop captures undo entries for them.
func (s *CommentVoteStore) promoteAllForComment(author auth.AccountName, permlink string) {
	if s.cold == nil {
		return
	}
	cKey := newCommentPK(author, permlink)
	if s.coldCount[cKey] == 0 {
		return
	}
	byCommentPrefix := tableKey(coldTableVoteByComment, CommentVoteByCommentPrefix(author, permlink))
	var voters []auth.AccountName
	s.cold.rangePrefix(byCommentPrefix, func(_, val []byte) bool {
		if cv, err := ReadCommentVote(val); err == nil {
			voters = append(voters, cv.Voter)
		}
		return true
	})
	for _, v := range voters {
		s.promoteVote(v, author, permlink)
	}
}

// Has reports whether (voter, author, permlink) has a vote row.
func (s *CommentVoteStore) Has(voter, author auth.AccountName, permlink string) bool {
	if _, ok := s.primary[newCommentVotePK(voter, author, permlink)]; ok {
		return true
	}
	if s.coldHas(newCommentPK(author, permlink)) {
		return s.cold.has(tableKey(coldTableVotePrimary, commentVoteKey(voter, author, permlink)))
	}
	return false
}

// Get returns a value-copy of the vote. Consults the cold tier on a RAM miss
// (read-only; does not promote).
func (s *CommentVoteStore) Get(voter, author auth.AccountName, permlink string) (CommentVote, bool) {
	cv, ok := s.primary[newCommentVotePK(voter, author, permlink)]
	if ok {
		return *cv, true
	}
	if s.coldHas(newCommentPK(author, permlink)) {
		return s.coldGetVote(voter, author, permlink)
	}
	return CommentVote{}, false
}

// GetRef returns a read-only pointer to the CommentVote slot — skips
// the value-copy that Get does. The caller must NOT mutate the slot;
// use Put for that. Returns (nil, false) when no row exists.
//
// Hot use: voteEvaluator does GetCommentVote to read prev on recasts;
// most votes are NEW (prev is unused) so the value-copy is pure cost
// in the common case. Using GetRef + dereferencing prev.* lazily saves
// ~75-byte copies × 9.5M votes ≈ 700 MB cum on an 8M replay.
func (s *CommentVoteStore) GetRef(voter, author auth.AccountName, permlink string) (*CommentVote, bool) {
	cv, ok := s.primary[newCommentVotePK(voter, author, permlink)]
	if ok {
		return cv, true
	}
	// RAM miss — promote from cold so the caller gets a real mutable slot.
	if s.coldHas(newCommentPK(author, permlink)) {
		return s.promoteVote(voter, author, permlink)
	}
	return nil, false
}

// Put inserts or replaces a vote. Maintains byComment membership.
//
// Allocation: pulls a *CommentVote slot from the freelist when available
// (refilled at CommitPoint); otherwise allocates fresh.
func (s *CommentVoteStore) Put(cv *CommentVote) {
	key := newCommentVotePK(cv.Voter, cv.Author, cv.Permlink)
	prev := s.primary[key]

	// RAM miss but the vote exists in the cold tier (recast of an evicted
	// vote): promote it so `prev` is the true prior row. This makes undo
	// capture the correct prev and keeps byComment membership consistent.
	if prev == nil && s.coldHas(newCommentPK(cv.Author, cv.Permlink)) {
		if p, ok := s.promoteVote(cv.Voter, cv.Author, cv.Permlink); ok {
			prev = p
		}
	}

	// Fast path: modify-in-place when there's no outstanding savepoint
	// + the row already exists. Skips the freelist pop, the byComment
	// swap-scan, and the primary map assign. Pointer identity is
	// preserved so byComment[cKey] continues to reference the same slot.
	// Hot path: voteEvaluator's per-vote PutCommentVote in per-block hook
	// context where UndoEnabled=false.
	if prev != nil && !s.db.UndoEnabled {
		*prev = *cv
		return
	}

	cKey := newCommentPK(cv.Author, cv.Permlink)

	var cp *CommentVote
	if n := len(s.freeCV); n > 0 {
		cp = s.freeCV[n-1]
		s.freeCV = s.freeCV[:n-1]
	} else {
		cp = new(CommentVote)
	}
	*cp = *cv
	if prev == nil {
		// New entry — append to byComment slice.
		s.byComment[cKey] = append(s.byComment[cKey], cp)
	} else {
		// Modification — find the prev pointer in the slice and
		// swap to the new one. The new pointer carries the new state
		// so all readers see fresh data.
		list := s.byComment[cKey]
		for i, p := range list {
			if p == prev {
				list[i] = cp
				break
			}
		}
	}
	s.primary[key] = cp

	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, commentVoteUndo{
			key:        key,
			prev:       prev,
			cur:        cp,
			commentKey: cKey,
		})
	}
}

// Delete removes (voter, author, permlink). Captures undo state.
func (s *CommentVoteStore) Delete(voter, author auth.AccountName, permlink string) {
	key := newCommentVotePK(voter, author, permlink)
	prev, ok := s.primary[key]
	if !ok {
		// Not resident — it may be in the cold tier.
		if s.coldHas(newCommentPK(author, permlink)) {
			if s.db.UndoEnabled {
				// Promote so the delete is captured in the undo log with the
				// real prev pointer; then fall through to the RAM delete path.
				if p, pok := s.promoteVote(voter, author, permlink); pok {
					prev, ok = p, true
				}
			} else {
				// No undo needed — delete directly from cold.
				s.coldDelVote(voter, author, permlink)
				return
			}
		}
		if !ok {
			return
		}
	}
	cKey := newCommentPK(author, permlink)
	s.byCommentRemove(cKey, prev)
	delete(s.primary, key)
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, commentVoteUndo{
			key:        key,
			prev:       prev,
			cur:        nil,
			commentKey: cKey,
		})
	} else {
		s.freeCV = append(s.freeCV, prev)
	}
}

// RangeByComment iterates every vote on (author, permlink). Order is
// not specified — callers that need a sort do it themselves. Returning
// false from fn stops.
func (s *CommentVoteStore) RangeByComment(author auth.AccountName, permlink string, fn func(cv *CommentVote) (bool, error)) error {
	cKey := newCommentPK(author, permlink)
	list := s.byComment[cKey]
	for _, cv := range list {
		ok, err := fn(cv)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	// Cold-resident votes for this comment (order unspecified, so we just
	// continue after the RAM members). Skipped entirely when none are cold.
	if s.coldHas(cKey) {
		var rerr error
		stop := false
		s.cold.rangePrefix(tableKey(coldTableVoteByComment, CommentVoteByCommentPrefix(author, permlink)), func(_, val []byte) bool {
			cv, err := ReadCommentVote(val)
			if err != nil {
				rerr = err
				return false
			}
			ok, ferr := fn(&cv)
			if ferr != nil {
				rerr = ferr
				return false
			}
			if !ok {
				stop = true
				return false
			}
			return true
		})
		if rerr != nil {
			return rerr
		}
		_ = stop
	}
	return nil
}

// DeleteAllForComment removes every vote on (author, permlink). Used
// by the cashout cleanup path. More efficient than RangeByComment +
// per-vote Delete since we can drop the slice in one shot.
func (s *CommentVoteStore) DeleteAllForComment(author auth.AccountName, permlink string) {
	cKey := newCommentPK(author, permlink)
	// Cold-resident votes for this comment (rare — only when a paid/old
	// comment had votes evicted then receives this cleanup). DeleteAllForComment
	// is only ever called at cashout where UndoEnabled is false, so a direct
	// cold range-delete is correct (no undo to capture). Promote-then-delete if
	// that invariant ever changes.
	if s.coldHas(cKey) {
		if s.db.UndoEnabled {
			s.promoteAllForComment(author, permlink) // makes them resident → deleted by loop below
		} else {
			s.coldDelAllForComment(author, permlink)
		}
	}
	list := s.byComment[cKey]
	for _, cv := range list {
		// Per the store invariant, `cv` IS the pointer stored in primary,
		// so we don't need a primary[key] lookup to recover prev.
		// Skipping that probe saved ~4.25s on an 8M replay (the largest
		// flat in this hot path).
		key := newCommentVotePK(cv.Voter, cv.Author, cv.Permlink)
		delete(s.primary, key)
		if s.db.UndoEnabled {
			s.undoLog = append(s.undoLog, commentVoteUndo{
				key:        key,
				prev:       cv,
				cur:        nil,
				commentKey: cKey,
			})
		} else {
			s.freeCV = append(s.freeCV, cv)
		}
	}
	delete(s.byComment, cKey)
}

// CountForComment returns the number of votes on (author, permlink) across
// both tiers.
func (s *CommentVoteStore) CountForComment(author auth.AccountName, permlink string) int {
	cKey := newCommentPK(author, permlink)
	return len(s.byComment[cKey]) + s.coldCount[cKey]
}

var commentVoteStoreSnapshotMagic = [4]byte{'C', 'V', 'O', 'T'}

const commentVoteStoreSnapshotVersion uint32 = 1

// MarshalSnapshot serializes the primary CommentVote map to w. Format
// mirrors witness_store: magic | version | count | (varuint(key)|key|varuint(rec)|AppendCommentVote(rec))*
// Keys are the raw bytes returned by commentVoteKey(voter, author, permlink),
// emitted in ascending order. byComment is rebuilt on UnmarshalSnapshot.
func (s *CommentVoteStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(commentVoteStoreSnapshotMagic[:]); err != nil {
		return err
	}
	total := len(s.primary)
	for _, n := range s.coldCount {
		total += n
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], commentVoteStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(total))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}

	// RAM side: sorted (voter, author, permlink) keys + their encoded key
	// bytes, so the merge can compare RAM vs cold by the same byte ordering
	// the cold tier uses (voter fixed-16 | author fixed-16 | permlink).
	keys := make([]commentVotePK, 0, len(s.primary))
	for k := range s.primary {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].voter != keys[j].voter {
			return keys[i].voter < keys[j].voter
		}
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

	// Fast path: no cold tier → emit RAM in sorted order (unchanged format).
	if s.cold == nil {
		for _, k := range keys {
			recBuf = AppendCommentVote(recBuf[:0], s.primary[k])
			if err := writeRec(recBuf); err != nil {
				return err
			}
		}
		return nil
	}

	// Two-way streaming merge of the sorted RAM keys and the cold primary
	// prefix scan (also ascending by the same encoded key). Disjoint sets;
	// emit in global ascending order so the byte stream is identical to an
	// all-RAM snapshot of the same logical contents.
	it, err := s.cold.newPrefixIter(tableKey(coldTableVotePrimary, nil))
	if err != nil {
		return err
	}
	defer it.Close()
	ri := 0
	ramKeyBytes := func(k commentVotePK) []byte {
		return commentVoteKey(auth.AccountName(k.voter), auth.AccountName(k.author), k.permlink)
	}
	for ri < len(keys) || it.valid() {
		emitRAM := false
		if ri < len(keys) && it.valid() {
			rk := ramKeyBytes(keys[ri])
			ck := it.key()[1:] // strip table prefix
			emitRAM = bytesLess(rk, ck)
		} else {
			emitRAM = ri < len(keys)
		}
		if emitRAM {
			recBuf = AppendCommentVote(recBuf[:0], s.primary[keys[ri]])
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

// UnmarshalSnapshot rebuilds the primary map AND the byComment secondary
// index from r. Undo log cleared.
func (s *CommentVoteStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("comment_vote_store: read magic: %w", err)
	}
	if magic != commentVoteStoreSnapshotMagic {
		return fmt.Errorf("comment_vote_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("comment_vote_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != commentVoteStoreSnapshotVersion {
		return fmt.Errorf("comment_vote_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.primary = make(map[commentVotePK]*CommentVote, count)
	s.byComment = make(map[commentPK][]*CommentVote, count/8+1)
	s.undoLog = s.undoLog[:0]
	// Everything loads into RAM; the cold tier starts empty after a load.
	s.coldCount = make(map[commentPK]int)
	for i := uint32(0); i < count; i++ {
		recLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("comment_vote_store: read rec len[%d]: %w", i, err)
		}
		rec := make([]byte, recLen)
		if _, err := io.ReadFull(br, rec); err != nil {
			return fmt.Errorf("comment_vote_store: read rec[%d]: %w", i, err)
		}
		cv, err := ReadCommentVote(rec)
		if err != nil {
			return fmt.Errorf("comment_vote_store: decode rec[%d]: %w", i, err)
		}
		cp := cv
		key := newCommentVotePK(cv.Voter, cv.Author, cv.Permlink)
		s.primary[key] = &cp
		// Rebuild byComment.
		cKey := newCommentPK(cv.Author, cv.Permlink)
		s.byComment[cKey] = append(s.byComment[cKey], &cp)
	}
	return nil
}

