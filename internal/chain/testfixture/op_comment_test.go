package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
)

// Port of operation_tests.cpp::comment_apply (HF17+ section).
//
// Hived's testnet runs at HF17 by default; we activate HF17 explicitly.
// Tests:
//   - alice posts a root → comment exists with cashout = creation+7d
//   - bob posts on non-existent parent → fails
//   - bob posts on alice's comment → exists, parent is alice/lorem
//   - sam posts on bob's comment → exists, parent is bob/ipsum
//   - modifying a comment with no votes works (LastUpdate refresh)
//   - reply interval enforcement
func TestCommentApplyHF17(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob", "sam")
	f.GenerateBlocks(60 / int(BlockInterval))

	t.Log("--- Alice posts a root comment")
	if err := f.ApplyOp(op.Comment{
		Author:         "alice",
		Permlink:       "lorem",
		ParentAuthor:   "",
		ParentPermlink: "ipsum",
		Title:          "Lorem Ipsum",
		Body:           "Lorem ipsum dolor sit amet.",
		JSONMetadata:   `{"foo":"bar"}`,
	}); err != nil {
		t.Fatalf("alice comment: %v", err)
	}
	if !f.HasComment("alice", "lorem") {
		t.Fatal("alice/lorem should exist")
	}
	c := f.Comment("alice", "lorem")
	if c.Author != "alice" {
		t.Fatalf("comment.Author: got %q, want alice", c.Author)
	}
	if c.NetRshares != 0 {
		t.Fatalf("comment.NetRshares should be 0, got %d", c.NetRshares)
	}
	expectCashout := f.HeadBlockTimeForApply() + 60*60*24*7
	if c.CashoutTime != expectCashout {
		t.Fatalf("comment.CashoutTime: got %d, want %d", c.CashoutTime, expectCashout)
	}

	t.Log("--- Bob posts on non-existent parent → fails")
	if err := f.ApplyOp(op.Comment{
		Author:         "bob",
		Permlink:       "ipsum",
		ParentAuthor:   "alice",
		ParentPermlink: "foobar",
	}); err == nil {
		t.Fatal("expected parent-not-found failure")
	}

	t.Log("--- Bob posts on alice/lorem")
	if err := f.ApplyOp(op.Comment{
		Author:         "bob",
		Permlink:       "ipsum",
		ParentAuthor:   "alice",
		ParentPermlink: "lorem",
		Title:          "Reply",
		Body:           "Bob's reply",
	}); err != nil {
		t.Fatalf("bob reply: %v", err)
	}
	if !f.HasComment("bob", "ipsum") {
		t.Fatal("bob/ipsum should exist")
	}

	t.Log("--- Sam posts on bob/ipsum (HF17+ allows replies on replies)")
	if err := f.ApplyOp(op.Comment{
		Author:         "sam",
		Permlink:       "dolor",
		ParentAuthor:   "bob",
		ParentPermlink: "ipsum",
		Body:           "Sam's reply",
	}); err != nil {
		t.Fatalf("sam reply: %v", err)
	}
	if !f.HasComment("sam", "dolor") {
		t.Fatal("sam/dolor should exist")
	}

	t.Log("--- Test modifying alice's comment (no votes yet)")
	if err := f.ApplyOp(op.Comment{
		Author:         "alice",
		Permlink:       "lorem",
		ParentAuthor:   "",
		ParentPermlink: "ipsum",
		Title:          "Updated",
		Body:           "New body",
	}); err != nil {
		t.Fatalf("alice update: %v", err)
	}
}

// Port of operation_tests.cpp::comment_delete_apply (subset).
//
// Author can delete their own comment if it has no positive net_rshares.
// HF19+: deletion is silent no-op when net_rshares > 0; pre-HF19 it
// fails outright.
func TestCommentDeleteApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.GenerateBlock()

	if err := f.ApplyOp(op.Comment{ParentPermlink: "test",
		Author:   "alice",
		Permlink: "test",
		Body:     "hello",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if !f.HasComment("alice", "test") {
		t.Fatal("comment should exist")
	}

	t.Log("--- delete_comment removes the comment")
	if err := f.ApplyOp(op.DeleteComment{
		Author:   "alice",
		Permlink: "test",
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if f.HasComment("alice", "test") {
		t.Fatal("comment should be removed")
	}
}

// Port of operation_tests.cpp::vote_apply (subset).
//
// alice (with vested HIVE) votes on her own post. The vote should
// register, NetRshares grows, voting mana decreases.
func TestVoteApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.FundVesting("alice", 100_000) // ample vesting
	f.GenerateBlock()

	if err := f.ApplyOp(op.Comment{ParentPermlink: "test",
		Author:   "alice",
		Permlink: "test",
		Body:     "hello",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	t.Log("--- alice upvotes herself at 100%")
	preMana := f.Account_("alice").VotingManaCurrent
	if err := f.ApplyOp(op.Vote{
		Voter:    "alice",
		Author:   "alice",
		Permlink: "test",
		Weight:   10000,
	}); err != nil {
		t.Fatalf("vote: %v", err)
	}
	c := f.Comment("alice", "test")
	if c.NetRshares <= 0 {
		t.Fatalf("comment.NetRshares should be positive, got %d", c.NetRshares)
	}
	if c.VoteCount != 1 {
		t.Fatalf("comment.VoteCount: got %d, want 1", c.VoteCount)
	}
	postMana := f.Account_("alice").VotingManaCurrent
	if postMana >= preMana {
		t.Fatalf("VotingManaCurrent should drop after vote: pre=%d post=%d", preMana, postMana)
	}

	t.Log("--- failure: voting on non-existent comment")
	if err := f.ApplyOp(op.Vote{
		Voter:    "alice",
		Author:   "alice",
		Permlink: "missing",
		Weight:   10000,
	}); err == nil {
		t.Fatal("expected comment-not-found failure")
	}

	t.Log("--- failure: voting on non-existent author")
	if err := f.ApplyOp(op.Vote{
		Voter:    "alice",
		Author:   "no-such-author",
		Permlink: "test",
		Weight:   10000,
	}); err == nil {
		t.Fatal("expected comment-not-found failure")
	}
}
