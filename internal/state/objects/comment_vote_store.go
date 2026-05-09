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
type CommentVoteStore struct {
	primary   map[string]*CommentVote
	byComment map[string][]*CommentVote
	undoLog   []commentVoteUndo
}

type commentVoteUndo struct {
	key  string       // primary key
	prev *CommentVote // nil = the row didn't exist before this mutation
	cur  *CommentVote // post-mutation pointer (nil = mutation was a Delete)
	// commentKey of the (author, permlink) — we need this even when prev
	// is nil since we have to know which byComment slice to undo from.
	commentKey string
}

const commentVoteStoreSubsystemKey = "comment_vote_store"

// EnsureCommentVoteStore returns the database's CommentVoteStore.
func EnsureCommentVoteStore(s *state.Session) *CommentVoteStore {
	db := s.Database()
	if existing := db.GetSubsystem(commentVoteStoreSubsystemKey); existing != nil {
		if cs, ok := existing.(*CommentVoteStore); ok {
			return cs
		}
	}
	cs := newCommentVoteStore()
	db.RegisterSubsystem(commentVoteStoreSubsystemKey, cs, cs)
	return cs
}

func newCommentVoteStore() *CommentVoteStore {
	return &CommentVoteStore{
		primary:   make(map[string]*CommentVote, 8192),
		byComment: make(map[string][]*CommentVote, 1024),
		undoLog:   make([]commentVoteUndo, 0, 64),
	}
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
			s.byCommentRemove(e.commentKey, e.key)
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

func (s *CommentVoteStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// byCommentRemove drops the entry whose primary key == primKey from
// byComment[commentKey].
func (s *CommentVoteStore) byCommentRemove(commentKey, primKey string) {
	list := s.byComment[commentKey]
	for i, cv := range list {
		if string(commentVoteKey(cv.Voter, cv.Author, cv.Permlink)) == primKey {
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

// Has reports whether (voter, author, permlink) has a vote row.
func (s *CommentVoteStore) Has(voter, author auth.AccountName, permlink string) bool {
	_, ok := s.primary[string(commentVoteKey(voter, author, permlink))]
	return ok
}

// Get returns a value-copy of the vote.
func (s *CommentVoteStore) Get(voter, author auth.AccountName, permlink string) (CommentVote, bool) {
	cv, ok := s.primary[string(commentVoteKey(voter, author, permlink))]
	if !ok {
		return CommentVote{}, false
	}
	return *cv, true
}

// Put inserts or replaces a vote. Maintains byComment membership.
func (s *CommentVoteStore) Put(cv *CommentVote) {
	key := string(commentVoteKey(cv.Voter, cv.Author, cv.Permlink))
	cKey := string(commentKey(cv.Author, cv.Permlink))
	prev := s.primary[key]

	cp := *cv
	if prev == nil {
		// New entry — append to byComment slice.
		s.byComment[cKey] = append(s.byComment[cKey], &cp)
	} else {
		// Modification — find the prev pointer in the slice and
		// swap to the new one. The new pointer carries the new state
		// so all readers see fresh data.
		list := s.byComment[cKey]
		for i, p := range list {
			if p == prev {
				list[i] = &cp
				break
			}
		}
	}
	s.primary[key] = &cp

	s.undoLog = append(s.undoLog, commentVoteUndo{
		key:        key,
		prev:       prev,
		cur:        &cp,
		commentKey: cKey,
	})
}

// Delete removes (voter, author, permlink). Captures undo state.
func (s *CommentVoteStore) Delete(voter, author auth.AccountName, permlink string) {
	key := string(commentVoteKey(voter, author, permlink))
	prev, ok := s.primary[key]
	if !ok {
		return
	}
	cKey := string(commentKey(author, permlink))
	s.byCommentRemove(cKey, key)
	delete(s.primary, key)
	s.undoLog = append(s.undoLog, commentVoteUndo{
		key:        key,
		prev:       prev,
		cur:        nil,
		commentKey: cKey,
	})
}

// RangeByComment iterates every vote on (author, permlink). Order is
// not specified — callers that need a sort do it themselves. Returning
// false from fn stops.
func (s *CommentVoteStore) RangeByComment(author auth.AccountName, permlink string, fn func(cv *CommentVote) (bool, error)) error {
	cKey := string(commentKey(author, permlink))
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
	return nil
}

// DeleteAllForComment removes every vote on (author, permlink). Used
// by the cashout cleanup path. More efficient than RangeByComment +
// per-vote Delete since we can drop the slice in one shot.
func (s *CommentVoteStore) DeleteAllForComment(author auth.AccountName, permlink string) {
	cKey := string(commentKey(author, permlink))
	list := s.byComment[cKey]
	for _, cv := range list {
		key := string(commentVoteKey(cv.Voter, cv.Author, cv.Permlink))
		prev := s.primary[key]
		delete(s.primary, key)
		s.undoLog = append(s.undoLog, commentVoteUndo{
			key:        key,
			prev:       prev,
			cur:        nil,
			commentKey: cKey,
		})
	}
	delete(s.byComment, cKey)
}

// CountForComment returns the number of votes on (author, permlink).
func (s *CommentVoteStore) CountForComment(author auth.AccountName, permlink string) int {
	return len(s.byComment[string(commentKey(author, permlink))])
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
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], commentVoteStoreSnapshotVersion)
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
		cv := s.primary[k]
		n := binary.PutUvarint(varBuf[:], uint64(len(k)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, k); err != nil {
			return err
		}
		recBuf = AppendCommentVote(recBuf[:0], cv)
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
	s.primary = make(map[string]*CommentVote, count)
	s.byComment = make(map[string][]*CommentVote, count/8+1)
	s.undoLog = s.undoLog[:0]
	for i := uint32(0); i < count; i++ {
		keyLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("comment_vote_store: read key len[%d]: %w", i, err)
		}
		keyBuf := make([]byte, keyLen)
		if _, err := io.ReadFull(br, keyBuf); err != nil {
			return fmt.Errorf("comment_vote_store: read key[%d]: %w", i, err)
		}
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
		s.primary[string(keyBuf)] = &cp
		// Rebuild byComment.
		cKey := string(commentKey(cv.Author, cv.Permlink))
		s.byComment[cKey] = append(s.byComment[cKey], &cp)
	}
	return nil
}

