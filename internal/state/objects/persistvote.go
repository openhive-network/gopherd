package objects

import (
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state/persiststore"
)

// PersistCommentVoteStore is a persiststore-backed comment-vote store
// (chainbase-in-Go phase 4): votes live in an mmap-backed log-structured store
// that survives restart without replay, replacing the in-memory map + the
// evict/promote cold tier. Each vote is stored under TWO keys (mirroring the
// cold tier): the PRIMARY key (voter,author,permlink) for point ops, and the
// BY-COMMENT key (author,permlink,voter) registered as an ordered table so
// distributeCuratorPool can prefix-scan a comment's voters at cashout.
//
// Votes are append-mostly (written at vote time, read at cashout, rarely
// mutated after), so the decode-on-read / encode-on-write cost is paid once —
// exactly the workload persiststore suits (unlike hot accounts).
type PersistCommentVoteStore struct {
	ps   *persiststore.Store
	serV []byte
	keyP []byte
	keyC []byte
}

func OpenPersistCommentVoteStore(dir string, reserveBytes uint64) (*PersistCommentVoteStore, error) {
	ps, err := persiststore.Open(dir, reserveBytes)
	if err != nil {
		return nil, err
	}
	ps.RegisterOrderedTable(coldTableVoteByComment) // by-comment prefix scans
	return &PersistCommentVoteStore{ps: ps}, nil
}

func (s *PersistCommentVoteStore) Put(cv *CommentVote) {
	s.serV = AppendCommentVote(s.serV[:0], cv)
	s.keyP = appendColdVotePrimaryKey(s.keyP[:0], cv.Voter, cv.Author, cv.Permlink)
	_ = s.ps.Set(string(s.keyP), s.serV)
	s.keyC = appendColdVoteByCommentKey(s.keyC[:0], cv.Author, cv.Permlink, cv.Voter)
	_ = s.ps.Set(string(s.keyC), s.serV)
}

func (s *PersistCommentVoteStore) Get(voter, author auth.AccountName, permlink string) (CommentVote, bool) {
	s.keyP = appendColdVotePrimaryKey(s.keyP[:0], voter, author, permlink)
	b, ok := s.ps.Get(string(s.keyP))
	if !ok {
		return CommentVote{}, false
	}
	cv, err := ReadCommentVote(b)
	if err != nil {
		return CommentVote{}, false
	}
	return cv, true
}

func (s *PersistCommentVoteStore) Has(voter, author auth.AccountName, permlink string) bool {
	s.keyP = appendColdVotePrimaryKey(s.keyP[:0], voter, author, permlink)
	_, ok := s.ps.Get(string(s.keyP))
	return ok
}

func (s *PersistCommentVoteStore) Delete(voter, author auth.AccountName, permlink string) {
	s.keyP = appendColdVotePrimaryKey(s.keyP[:0], voter, author, permlink)
	s.ps.Delete(string(s.keyP))
	s.keyC = appendColdVoteByCommentKey(s.keyC[:0], author, permlink, voter)
	s.ps.Delete(string(s.keyC))
}

// RangeByComment iterates every vote on (author, permlink) in ascending voter
// order — the distributeCuratorPool scan. fn returns false to stop.
func (s *PersistCommentVoteStore) RangeByComment(author auth.AccountName, permlink string, fn func(cv *CommentVote) bool) {
	prefix := append([]byte{coldTableVoteByComment}, CommentVoteByCommentPrefix(author, permlink)...)
	s.ps.RangePrefix(prefix, func(_ string, val []byte) bool {
		cv, err := ReadCommentVote(val)
		if err != nil {
			return true
		}
		return fn(&cv)
	})
}

func (s *PersistCommentVoteStore) Len() int { return s.ps.Len() } // counts both key kinds

// --- SnapshotSubsystem contract ---
func (s *PersistCommentVoteStore) SetUndoEnabled(b bool) { s.ps.SetUndoEnabled(b) }
func (s *PersistCommentVoteStore) SnapshotPoint() int    { return s.ps.SnapshotPoint() }
func (s *PersistCommentVoteStore) RestoreToPoint(n int)  { s.ps.RestoreToPoint(n) }
func (s *PersistCommentVoteStore) CommitPoint()          { s.ps.CommitPoint() }

// --- lifecycle ---
func (s *PersistCommentVoteStore) Flush() error   { return s.ps.Flush() }
func (s *PersistCommentVoteStore) Compact() error { return s.ps.Compact() }
func (s *PersistCommentVoteStore) Close() error   { return s.ps.Close() }
