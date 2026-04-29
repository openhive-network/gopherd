package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// Comment is the chain-level comment_object — drastically slimmed
// from hived's full schema (which includes net_rshares, abs_rshares,
// children_count, cashout_time, beneficiaries, percent_hbd, total
// payout values, etc.). For the apply pipeline to drive forward we
// track existence and a few ledger-relevant fields. Voting math
// (rshares curves, voting power decay, payouts) lands later.
type Comment struct {
	Author         auth.AccountName
	Permlink       string
	ParentAuthor   auth.AccountName
	ParentPermlink string
	Created        uint32
	LastUpdate     uint32
	// CashoutTime is the time_point_sec when the comment is eligible for
	// payout. Set when the comment is created based on the active cashout
	// window (24h pre-HF12, 12h pre-HF17, 7d after) and used by the
	// per-block process_comment_cashouts hook to identify due comments.
	// Cleared (set to max uint32) once the comment has been paid out.
	CashoutTime uint32
	NetRshares  int64 // running sum of vote rshares (placeholder — exact math comes later)
	VoteCount   uint32
	// MaxAcceptedPayout / PercentHbd / AllowVotes / AllowCurationRewards
	// are mirrored from the most recent comment_options operation. In
	// hived these can only be set BEFORE the first vote.
	MaxAcceptedPayout    int64
	PercentHbd           uint16
	AllowVotes           bool
	AllowCurationRewards bool
}

// CommentVote is one (voter, author, permlink) edge — a comment_vote_object.
type CommentVote struct {
	Voter      auth.AccountName
	Author     auth.AccountName
	Permlink   string
	Weight     int16  // 0..10000 (HIVE_100_PERCENT)
	NumChanges int16  // 0 on first cast; -1 marks a delete in hived
	Rshares    int64  // simplified — full calc deferred
	LastUpdate uint32
}

// Table IDs.
const (
	CommentTable     state.TableID = 4
	CommentVoteTable state.TableID = 5
	// CommentCashoutScheduleTable indexes pending comment payouts by
	// cashout time. Key: [cashout_time_BE_4][author_padded_16][permlink_varlen].
	// Value: empty. process_comment_cashouts iterates the table in time
	// order each block and pays out every entry whose time has elapsed.
	CommentCashoutScheduleTable state.TableID = 19
)

// CommentCashoutScheduleKey encodes (cashoutTime, author, permlink) into
// the schedule index key. Time is BE-encoded so lex order matches numeric
// order; author is padded to AccountNameKeyLen; permlink follows raw.
func CommentCashoutScheduleKey(cashoutTime uint32, author auth.AccountName, permlink string) []byte {
	out := state.AppendUint32BE(nil, cashoutTime)
	out = state.AppendAccountName(out, author)
	return append(out, permlink...)
}

// PutCommentCashoutSchedule registers (cashoutTime, author, permlink) in
// the schedule index.
func PutCommentCashoutSchedule(s *state.Session, cashoutTime uint32, author auth.AccountName, permlink string) {
	s.Put(CommentCashoutScheduleTable, state.PrimaryIndex, CommentCashoutScheduleKey(cashoutTime, author, permlink), nil)
}

// DeleteCommentCashoutSchedule removes a (cashoutTime, author, permlink)
// schedule entry.
func DeleteCommentCashoutSchedule(s *state.Session, cashoutTime uint32, author auth.AccountName, permlink string) {
	s.Delete(CommentCashoutScheduleTable, state.PrimaryIndex, CommentCashoutScheduleKey(cashoutTime, author, permlink))
}

// commentKey encodes (author, permlink) as a fixed-width 16-byte author
// followed by the raw permlink bytes. Fixed-width author means no
// (author, permlink) collisions possible since the boundary between
// author and permlink is unambiguous. Used as both the primary key
// for Comment and the (author, permlink) suffix of CommentVote keys.
func commentKey(author auth.AccountName, permlink string) []byte {
	out := state.AppendAccountName(nil, author)
	return append(out, permlink...)
}

// AppendComment serializes c.
func AppendComment(dst []byte, c *Comment) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(c.Author)))
	dst = append(dst, c.Author...)
	dst = fcraw.AppendVarUint(dst, uint32(len(c.Permlink)))
	dst = append(dst, c.Permlink...)
	dst = fcraw.AppendVarUint(dst, uint32(len(c.ParentAuthor)))
	dst = append(dst, c.ParentAuthor...)
	dst = fcraw.AppendVarUint(dst, uint32(len(c.ParentPermlink)))
	dst = append(dst, c.ParentPermlink...)
	dst = appendU32LE(dst, c.Created)
	dst = appendU32LE(dst, c.LastUpdate)
	dst = appendU32LE(dst, c.CashoutTime)
	dst = appendI64LE(dst, c.NetRshares)
	dst = appendU32LE(dst, c.VoteCount)
	dst = appendI64LE(dst, c.MaxAcceptedPayout)
	dst = append(dst, byte(c.PercentHbd), byte(c.PercentHbd>>8))
	if c.AllowVotes {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	if c.AllowCurationRewards {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	return dst
}

// ReadComment decodes a Comment from src.
func ReadComment(src []byte) (Comment, error) {
	r := fcraw.NewReader(src)
	var c Comment
	var err error
	if c.Author, err = r.ReadString(); err != nil {
		return c, fmt.Errorf("comment.author: %w", err)
	}
	if c.Permlink, err = r.ReadString(); err != nil {
		return c, fmt.Errorf("comment.permlink: %w", err)
	}
	if c.ParentAuthor, err = r.ReadString(); err != nil {
		return c, fmt.Errorf("comment.parent_author: %w", err)
	}
	if c.ParentPermlink, err = r.ReadString(); err != nil {
		return c, fmt.Errorf("comment.parent_permlink: %w", err)
	}
	if c.Created, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if c.LastUpdate, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if c.CashoutTime, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if c.NetRshares, err = r.ReadInt64(); err != nil {
		return c, err
	}
	if c.VoteCount, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if c.MaxAcceptedPayout, err = r.ReadInt64(); err != nil {
		return c, err
	}
	if c.PercentHbd, err = r.ReadUint16(); err != nil {
		return c, err
	}
	if c.AllowVotes, err = r.ReadBool(); err != nil {
		return c, err
	}
	if c.AllowCurationRewards, err = r.ReadBool(); err != nil {
		return c, err
	}
	if !r.AtEnd() {
		return c, errors.New("comment: trailing bytes")
	}
	return c, nil
}

// PutComment stores c.
func PutComment(s *state.Session, c *Comment) {
	s.Put(CommentTable, state.PrimaryIndex, commentKey(c.Author, c.Permlink), AppendComment(nil, c))
}

// GetComment fetches a comment by (author, permlink).
func GetComment(s *state.Session, author auth.AccountName, permlink string) (Comment, error) {
	v, err := s.Get(CommentTable, state.PrimaryIndex, commentKey(author, permlink))
	if err != nil {
		return Comment{}, err
	}
	return ReadComment(v)
}

// HasComment reports comment existence.
func HasComment(s *state.Session, author auth.AccountName, permlink string) (bool, error) {
	return s.Has(CommentTable, state.PrimaryIndex, commentKey(author, permlink))
}

// DeleteComment removes a comment.
func DeleteComment(s *state.Session, author auth.AccountName, permlink string) {
	s.Delete(CommentTable, state.PrimaryIndex, commentKey(author, permlink))
}

// commentVoteKey encodes (voter, author, permlink). Voter is fixed 16
// bytes, then comment's (author, permlink) via commentKey.
func commentVoteKey(voter, author auth.AccountName, permlink string) []byte {
	out := state.AppendAccountName(nil, voter)
	return append(out, commentKey(author, permlink)...)
}

// AppendCommentVote serializes cv.
func AppendCommentVote(dst []byte, cv *CommentVote) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(cv.Voter)))
	dst = append(dst, cv.Voter...)
	dst = fcraw.AppendVarUint(dst, uint32(len(cv.Author)))
	dst = append(dst, cv.Author...)
	dst = fcraw.AppendVarUint(dst, uint32(len(cv.Permlink)))
	dst = append(dst, cv.Permlink...)
	dst = append(dst, byte(cv.Weight), byte(cv.Weight>>8))
	dst = append(dst, byte(cv.NumChanges), byte(cv.NumChanges>>8))
	dst = appendI64LE(dst, cv.Rshares)
	dst = appendU32LE(dst, cv.LastUpdate)
	return dst
}

// ReadCommentVote decodes a CommentVote.
func ReadCommentVote(src []byte) (CommentVote, error) {
	r := fcraw.NewReader(src)
	var cv CommentVote
	var err error
	if cv.Voter, err = r.ReadString(); err != nil {
		return cv, err
	}
	if cv.Author, err = r.ReadString(); err != nil {
		return cv, err
	}
	if cv.Permlink, err = r.ReadString(); err != nil {
		return cv, err
	}
	if w, err := r.ReadUint16(); err != nil {
		return cv, err
	} else {
		cv.Weight = int16(w)
	}
	if n, err := r.ReadUint16(); err != nil {
		return cv, err
	} else {
		cv.NumChanges = int16(n)
	}
	if cv.Rshares, err = r.ReadInt64(); err != nil {
		return cv, err
	}
	if cv.LastUpdate, err = r.ReadUint32(); err != nil {
		return cv, err
	}
	if !r.AtEnd() {
		return cv, errors.New("comment_vote: trailing bytes")
	}
	return cv, nil
}

// PutCommentVote stores cv.
func PutCommentVote(s *state.Session, cv *CommentVote) {
	s.Put(CommentVoteTable, state.PrimaryIndex, commentVoteKey(cv.Voter, cv.Author, cv.Permlink), AppendCommentVote(nil, cv))
}

// GetCommentVote fetches the vote (voter, author, permlink).
func GetCommentVote(s *state.Session, voter, author auth.AccountName, permlink string) (CommentVote, error) {
	v, err := s.Get(CommentVoteTable, state.PrimaryIndex, commentVoteKey(voter, author, permlink))
	if err != nil {
		return CommentVote{}, err
	}
	return ReadCommentVote(v)
}

// HasCommentVote reports vote existence.
func HasCommentVote(s *state.Session, voter, author auth.AccountName, permlink string) (bool, error) {
	return s.Has(CommentVoteTable, state.PrimaryIndex, commentVoteKey(voter, author, permlink))
}
