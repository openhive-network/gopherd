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
	// Beneficiaries (HF17+) — comment_payout_beneficiaries extension on
	// comment_options. Each beneficiary takes a percentage of the
	// author payout AT cashout. Sum of weights ≤ HIVE_100_PERCENT (10000).
	Beneficiaries []Beneficiary
	// AbsRshares accumulates |rshares| over all votes; used by hived's
	// curation weight calc and the "must not have been voted on yet"
	// gate for setting beneficiaries.
	AbsRshares int64
	// WasPaid is set true after the first cashout. Used by HF12-HF17
	// to gate the second-cashout window: pre-HF17 root posts that
	// haven't been paid get rescheduled at +30d after first cashout;
	// already-paid posts go to max (frozen).
	WasPaid bool
	// LastPayout — time_point_sec of the most recent cashout that
	// fired on this comment. Mirrors comment_cashout_ex_object::last_payout.
	// Used by HF12 to schedule the second cashout window
	// (last_payout + HIVE_SECOND_CASHOUT_WINDOW).
	LastPayout uint32
	// VoteRshares accumulates POSITIVE rshares only — used as the
	// input to the curation-weight curve W(R) so each vote's weight
	// is W(vote_rshares_after) - W(vote_rshares_before). Mirrors
	// comment_cashout_object::vote_rshares.
	VoteRshares uint64
	// TotalVoteWeight is the sum of every vote's curation weight on
	// this comment. At cashout, each voter's slice of the curator
	// pool is curatorPool * vote.curation_weight / total_vote_weight.
	// Mirrors comment_cashout_object::total_vote_weight.
	TotalVoteWeight uint64
	// RootAuthor / RootPermlink point to the discussion root — the
	// top-level post in this thread. Root posts have Root* == self.
	// Mirrors comment_cashout_ex_object::root_id. Pre-HF17
	// process_comment_cashout iterates by root_id and pays every
	// comment under the same root together; that path needs this
	// field to find siblings.
	RootAuthor   auth.AccountName
	RootPermlink string
	// Children counts direct replies. Mirrors comment_cashout_object::children
	// (libraries/chain/include/.../comment_object.hpp:241). Bumped by
	// on_reply when a child is created, decremented when a child is
	// deleted. delete_comment rejects when Children > 0 ("Cannot delete
	// a comment with replies").
	Children uint32
	// ID is a monotonic insertion-order identifier mirroring C++'s
	// auto-incrementing comment_object::id. Used as the tie-breaker in
	// the by_cashout_time schedule so cashouts iterate in the same order
	// as hived (same cashout_time → smaller comment_id first), matching
	// composite_key<cashout_time, comment_id> at by_cashout_time.
	// Allocated from dgpo.NextCommentID at create time.
	ID uint64
	// ChildrenAbsRshares — sum of |rshares| over every vote across the
	// whole thread (root + every reply). Lives only on the ROOT post;
	// pre-HF17 the vote evaluator at hive_evaluator_social.cpp:412-415
	// reads/writes it via comment_cashout_ex_object on the root_id.
	// Used as the denominator in the weighted-average cashout_time
	// formula at line 437.
	ChildrenAbsRshares uint64
	// MaxCashoutTime — the upper bound on a thread's cashout_time. Set
	// on the ROOT post by the FIRST vote to (now + HIVE_MAX_CASHOUT_WINDOW
	// = 14d). The weighted-average cashout_time clamps to this value
	// (hive_evaluator_social.cpp:439). 0 == not yet set (treat as max).
	MaxCashoutTime uint32
	// RewardWeight is the per-comment haircut applied to the cashout
	// pool claim — `claim = claim × RewardWeight / HIVE_100_PERCENT`.
	// Set on creation for HF12+ pre-HF17 ROOT posts based on the
	// author's PostBandwidth decay (hive_evaluator_social.cpp:216-225;
	// reward.cpp:18). 10000 (HIVE_100_PERCENT) = no haircut. Stored on
	// comment_cashout_ex_object in C++. 0 means "not initialized";
	// treat as 10000 for legacy rows and replies.
	RewardWeight uint16
}

// Beneficiary is one row in a comment's beneficiaries list.
type Beneficiary struct {
	Account auth.AccountName
	Weight  uint16 // basis points of HIVE_100_PERCENT
}

// WasPaid is implicitly tracked via a sentinel value in CashoutTime
// (we mirror hived's "set to time_point_sec::max after first cashout").
// In our model CashoutTime==0 means "max"/frozen; once set, we know
// the comment has been paid at least once.
//
// This file's struct definition only declares the explicit fields
// already present; the flag below is a virtual property derived from
// CashoutTime + flag bit packed into reserved space.

// CommentVote is one (voter, author, permlink) edge — a comment_vote_object.
type CommentVote struct {
	Voter      auth.AccountName
	Author     auth.AccountName
	Permlink   string
	Weight     int16 // 0..10000 (HIVE_100_PERCENT) — the vote's percent
	NumChanges int16 // 0 on first cast; -1 marks a delete in hived
	Rshares    int64 // signed rshares contribution
	LastUpdate uint32
	// CurationWeight is the W(R_N) - W(R_N-1) increment computed at
	// vote time, post-reverse-auction time scaling. Used at cashout
	// to distribute the curator pool proportionally. Mirrors
	// comment_vote_object::weight.
	CurationWeight uint64
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

// CommentByRootIndex mirrors comment_cashout_ex_index::by_root in hived
// — every comment_cashout_ex_object stores a root_id and is indexed by it
// so process_comment_cashout pre-HF17 can pay all replies under a root
// when that root cashes out (libraries/chain/database_comment.cpp:485-507).
// Key: [root_author_padded_16][root_permlink_varlen][author_padded_16][permlink_varlen].
const CommentByRootIndex state.IndexID = 1

func commentByRootKey(rootAuthor auth.AccountName, rootPermlink string, author auth.AccountName, permlink string) []byte {
	out := state.AppendAccountName(nil, rootAuthor)
	out = append(out, rootPermlink...)
	// Separator between root permlink and author so the by-root prefix
	// scan stops cleanly at the next root. The 0x00 byte cannot appear
	// inside a permlink (validate_permlink_*) so it's a safe sentinel.
	out = append(out, 0x00)
	out = state.AppendAccountName(out, author)
	return append(out, permlink...)
}

// CommentByRootPrefix returns the prefix that selects every comment
// under (rootAuthor, rootPermlink).
func CommentByRootPrefix(rootAuthor auth.AccountName, rootPermlink string) []byte {
	out := state.AppendAccountName(nil, rootAuthor)
	out = append(out, rootPermlink...)
	return append(out, 0x00)
}

// CommentCashoutScheduleKey encodes (cashoutTime, commentID) — the
// schedule iterates in this order, mirroring C++'s
// composite_key<cashout_time, comment_id> on by_cashout_time. The
// (author, permlink) pair is stored in the value blob so callers can
// recover it during iteration without a second lookup.
//
// 4 BE bytes time + 8 BE bytes id = 12-byte fixed key.
func CommentCashoutScheduleKey(cashoutTime uint32, commentID uint64) []byte {
	out := state.AppendUint32BE(nil, cashoutTime)
	return state.AppendUint64BE(out, commentID)
}

// commentCashoutScheduleValue serializes (author, permlink) into the
// schedule entry's value: 16-byte padded author + permlink raw. The
// permlink length is implicit from the value-buffer length.
func commentCashoutScheduleValue(author auth.AccountName, permlink string) []byte {
	out := state.AppendAccountName(nil, author)
	return append(out, permlink...)
}

// DecodeCommentCashoutScheduleValue extracts (author, permlink) from a
// schedule entry's value blob.
func DecodeCommentCashoutScheduleValue(body []byte) (auth.AccountName, string) {
	if len(body) < state.AccountNameKeyLen {
		return "", ""
	}
	authorBytes := body[:state.AccountNameKeyLen]
	n := state.AccountNameKeyLen
	for n > 0 && authorBytes[n-1] == 0 {
		n--
	}
	return auth.AccountName(authorBytes[:n]), string(body[state.AccountNameKeyLen:])
}

// PutCommentCashoutSchedule registers (cashoutTime, commentID, author,
// permlink) in the schedule index. Routes through the typed store.
func PutCommentCashoutSchedule(s *state.Session, cashoutTime uint32, commentID uint64, author auth.AccountName, permlink string) {
	EnsureCommentCashoutScheduleStore(s).Put(cashoutTime, commentID, author, permlink)
}

// DeleteCommentCashoutSchedule removes a (cashoutTime, commentID)
// schedule entry. Routes through the typed store.
func DeleteCommentCashoutSchedule(s *state.Session, cashoutTime uint32, commentID uint64) {
	EnsureCommentCashoutScheduleStore(s).Delete(cashoutTime, commentID)
}

// commentKey encodes (author, permlink) as a fixed-width 16-byte author
// followed by the raw permlink bytes. Fixed-width author means no
// (author, permlink) collisions possible since the boundary between
// author and permlink is unambiguous. Used as both the primary key
// for Comment and the (author, permlink) suffix of CommentVote keys.
//
// Single-allocation form: pre-size the output to exactly
// AccountNameKeyLen + len(permlink) so neither append() resizes.
// The inline name+permlink fill avoids the prior pair of growslice
// allocations (~3.3GB on a 200K-block run pre-fix).
func commentKey(author auth.AccountName, permlink string) []byte {
	out := make([]byte, state.AccountNameKeyLen+len(permlink))
	copy(out, author)
	copy(out[state.AccountNameKeyLen:], permlink)
	return out
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
	dst = fcraw.AppendVarUint(dst, uint32(len(c.Beneficiaries)))
	for _, b := range c.Beneficiaries {
		dst = fcraw.AppendVarUint(dst, uint32(len(b.Account)))
		dst = append(dst, b.Account...)
		dst = append(dst, byte(b.Weight), byte(b.Weight>>8))
	}
	dst = appendI64LE(dst, c.AbsRshares)
	if c.WasPaid {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	dst = appendU64LE(dst, c.VoteRshares)
	dst = appendU64LE(dst, c.TotalVoteWeight)
	dst = appendU32LE(dst, c.LastPayout)
	dst = fcraw.AppendVarUint(dst, uint32(len(c.RootAuthor)))
	dst = append(dst, c.RootAuthor...)
	dst = fcraw.AppendVarUint(dst, uint32(len(c.RootPermlink)))
	dst = append(dst, c.RootPermlink...)
	dst = appendU32LE(dst, c.Children)
	dst = appendU64LE(dst, c.ID)
	dst = appendU64LE(dst, c.ChildrenAbsRshares)
	dst = appendU32LE(dst, c.MaxCashoutTime)
	dst = append(dst, byte(c.RewardWeight), byte(c.RewardWeight>>8))
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
	// Beneficiaries + AbsRshares are a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return c, nil
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return c, fmt.Errorf("comment.beneficiaries.len: %w", err)
	}
	if count > 8 {
		return c, fmt.Errorf("comment.beneficiaries: count %d exceeds HIVE_MAX_COMMENT_BENEFICIARIES (8)", count)
	}
	c.Beneficiaries = make([]Beneficiary, count)
	for i := uint32(0); i < count; i++ {
		acct, err := r.ReadString()
		if err != nil {
			return c, fmt.Errorf("comment.beneficiaries[%d].account: %w", i, err)
		}
		w, err := r.ReadUint16()
		if err != nil {
			return c, fmt.Errorf("comment.beneficiaries[%d].weight: %w", i, err)
		}
		c.Beneficiaries[i] = Beneficiary{Account: acct, Weight: w}
	}
	if c.AbsRshares, err = r.ReadInt64(); err != nil {
		return c, fmt.Errorf("comment.abs_rshares: %w", err)
	}
	// WasPaid is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return c, nil
	}
	if c.WasPaid, err = r.ReadBool(); err != nil {
		return c, fmt.Errorf("comment.was_paid: %w", err)
	}
	// VoteRshares + TotalVoteWeight are a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return c, nil
	}
	if c.VoteRshares, err = r.ReadUint64(); err != nil {
		return c, fmt.Errorf("comment.vote_rshares: %w", err)
	}
	if c.TotalVoteWeight, err = r.ReadUint64(); err != nil {
		return c, fmt.Errorf("comment.total_vote_weight: %w", err)
	}
	// LastPayout is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return c, nil
	}
	if c.LastPayout, err = r.ReadUint32(); err != nil {
		return c, fmt.Errorf("comment.last_payout: %w", err)
	}
	// RootAuthor + RootPermlink are a later schema addition; tolerate EOF
	// for legacy rows. New rows always populate them at create time.
	if r.AtEnd() {
		c.RootAuthor = c.Author
		c.RootPermlink = c.Permlink
		return c, nil
	}
	if c.RootAuthor, err = r.ReadString(); err != nil {
		return c, fmt.Errorf("comment.root_author: %w", err)
	}
	if c.RootPermlink, err = r.ReadString(); err != nil {
		return c, fmt.Errorf("comment.root_permlink: %w", err)
	}
	if r.AtEnd() {
		return c, nil
	}
	if c.Children, err = r.ReadUint32(); err != nil {
		return c, fmt.Errorf("comment.children: %w", err)
	}
	if r.AtEnd() {
		return c, nil
	}
	if c.ID, err = r.ReadUint64(); err != nil {
		return c, fmt.Errorf("comment.id: %w", err)
	}
	if r.AtEnd() {
		return c, nil
	}
	if c.ChildrenAbsRshares, err = r.ReadUint64(); err != nil {
		return c, fmt.Errorf("comment.children_abs_rshares: %w", err)
	}
	if c.MaxCashoutTime, err = r.ReadUint32(); err != nil {
		return c, fmt.Errorf("comment.max_cashout_time: %w", err)
	}
	// RewardWeight is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return c, nil
	}
	if c.RewardWeight, err = r.ReadUint16(); err != nil {
		return c, fmt.Errorf("comment.reward_weight: %w", err)
	}
	if !r.AtEnd() {
		return c, errors.New("comment: trailing bytes")
	}
	return c, nil
}

// PutComment stores c via the typed CommentStore.
func PutComment(s *state.Session, c *Comment) {
	EnsureCommentStore(s).Put(c)
}

// GetComment fetches a comment by (author, permlink).
func GetComment(s *state.Session, author auth.AccountName, permlink string) (Comment, error) {
	c, ok := EnsureCommentStore(s).Get(author, permlink)
	if !ok {
		return Comment{}, state.ErrNotFound
	}
	return c, nil
}

// GetCommentRef returns a read-only pointer to the comment slot for
// (author, permlink). Skips the ~256-byte Comment value-copy that
// GetComment pays. Aliases internal storage — see CommentStore.GetRef
// for the read-only contract.
func GetCommentRef(s *state.Session, author auth.AccountName, permlink string) (*Comment, bool) {
	return EnsureCommentStore(s).GetRef(author, permlink)
}

// MutateComment applies in-place modifications to the comment slot at
// (author, permlink). See CommentStore.Mutate for the constraints
// (must not change RootAuthor / RootPermlink). Returns false if the
// comment doesn't exist.
func MutateComment(s *state.Session, author auth.AccountName, permlink string, fn func(*Comment)) bool {
	return EnsureCommentStore(s).Mutate(author, permlink, fn)
}

// HasComment reports comment existence.
func HasComment(s *state.Session, author auth.AccountName, permlink string) (bool, error) {
	return EnsureCommentStore(s).Has(author, permlink), nil
}

// DeleteComment removes a comment from the typed store.
func DeleteComment(s *state.Session, author auth.AccountName, permlink string) {
	EnsureCommentStore(s).Delete(author, permlink)
}

// RangeCommentsByRoot iterates every comment in the discussion rooted at
// (rootAuthor, rootPermlink).
func RangeCommentsByRoot(s *state.Session, rootAuthor auth.AccountName, rootPermlink string, fn func(c *Comment) (bool, error)) error {
	return EnsureCommentStore(s).RangeByRoot(rootAuthor, rootPermlink, fn)
}

// RangeAllComments iterates every comment in name-ascending order.
func RangeAllComments(s *state.Session, fn func(c *Comment) (bool, error)) error {
	return EnsureCommentStore(s).RangePrimary(fn)
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
	dst = appendU64LE(dst, cv.CurationWeight)
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
	// CurationWeight is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return cv, nil
	}
	if cv.CurationWeight, err = r.ReadUint64(); err != nil {
		return cv, fmt.Errorf("comment_vote.curation_weight: %w", err)
	}
	if !r.AtEnd() {
		return cv, errors.New("comment_vote: trailing bytes")
	}
	return cv, nil
}

// CommentVoteByCommentIndex is the secondary index on CommentVote that
// orders entries by (author, permlink, voter) so that all votes on a
// given comment can be enumerated by prefix iteration. Used by the
// cashout pipeline to split the curator pool among voters.
const CommentVoteByCommentIndex state.IndexID = 1

// commentVoteByCommentKey encodes (author, permlink, voter) for the
// secondary index.
func commentVoteByCommentKey(author auth.AccountName, permlink string, voter auth.AccountName) []byte {
	out := state.AppendAccountName(nil, author)
	out = append(out, permlink...)
	// Separator between variable-length permlink and fixed-width voter.
	// The 0x00 byte is unambiguous because permlinks (account_names too)
	// are restricted to a-z, 0-9, '.', '-' so they can't contain NUL.
	out = append(out, 0)
	return state.AppendAccountName(out, voter)
}

// CommentVoteByCommentPrefix returns the (author, permlink, 0x00) prefix
// suitable for Range iteration over all voters of a comment.
func CommentVoteByCommentPrefix(author auth.AccountName, permlink string) []byte {
	out := state.AppendAccountName(nil, author)
	out = append(out, permlink...)
	out = append(out, 0)
	return out
}

// PutCommentVote stores cv via the typed CommentVoteStore.
func PutCommentVote(s *state.Session, cv *CommentVote) {
	EnsureCommentVoteStore(s).Put(cv)
}

// GetCommentVote fetches the vote (voter, author, permlink).
func GetCommentVote(s *state.Session, voter, author auth.AccountName, permlink string) (CommentVote, error) {
	cv, ok := EnsureCommentVoteStore(s).Get(voter, author, permlink)
	if !ok {
		return CommentVote{}, state.ErrNotFound
	}
	return cv, nil
}

// GetCommentVoteRef returns a read-only pointer; see CommentVoteStore.GetRef.
func GetCommentVoteRef(s *state.Session, voter, author auth.AccountName, permlink string) (*CommentVote, bool) {
	return EnsureCommentVoteStore(s).GetRef(voter, author, permlink)
}

// HasCommentVote reports vote existence.
func HasCommentVote(s *state.Session, voter, author auth.AccountName, permlink string) (bool, error) {
	return EnsureCommentVoteStore(s).Has(voter, author, permlink), nil
}

// DeleteCommentVote removes (voter, author, permlink).
func DeleteCommentVote(s *state.Session, voter, author auth.AccountName, permlink string) {
	EnsureCommentVoteStore(s).Delete(voter, author, permlink)
}

// RangeCommentVotesByComment iterates every vote on (author, permlink).
func RangeCommentVotesByComment(s *state.Session, author auth.AccountName, permlink string, fn func(cv *CommentVote) (bool, error)) error {
	return EnsureCommentVoteStore(s).RangeByComment(author, permlink, fn)
}

// DeleteAllCommentVotesForComment removes every vote on (author, permlink).
func DeleteAllCommentVotesForComment(s *state.Session, author auth.AccountName, permlink string) {
	EnsureCommentVoteStore(s).DeleteAllForComment(author, permlink)
}
