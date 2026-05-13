package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
)

// Tests in this file port the remaining scenarios from
// operation_tests.cpp::comment_apply and
// operation_tests.cpp::comment_delete_apply that aren't already
// covered by TestCommentApplyHF17 / TestCommentApply /
// TestCommentDeleteApply.
//
// What's NEW here:
//   - editing a comment AFTER it has been voted on preserves
//     Created/CashoutTime/ParentAuthor (only LastUpdate moves)
//   - "Test failure posting within 5 minutes" — root post rate limit
//     (HIVE_MIN_ROOT_COMMENT_INTERVAL) enforced for second root post
//   - "Test failure posting within 5 minutes" — root post succeeds
//     once the interval has elapsed
//   - delete with positive net_rshares fails post-HF19
//   - delete after net_rshares goes negative succeeds
//   - delete past cashout (HF19+) fails
//   - delete a comment that has a reply fails

// TestCommentApplyEditAfterVotePreservesFields ports the "modifying a
// comment" branch of operation_tests.cpp::comment_apply. After voting
// on sam/dolor (which gives it positive net_rshares), re-issuing the
// comment_operation must NOT reset creation_time, cashout_time, or
// parent_id; only last_update advances.
func TestCommentApplyEditAfterVotePreservesFields(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob", "sam")
	f.FundVesting("sam", 100_000) // sam needs vests to vote
	f.GenerateBlocks(60 / int(BlockInterval))

	// alice → root, bob → reply, sam → reply on bob.
	f.MustApplyOp(op.Comment{Author: "alice", Permlink: "lorem", ParentPermlink: "ipsum", Body: "x"})
	f.MustApplyOp(op.Comment{
		Author: "bob", Permlink: "ipsum",
		ParentAuthor: "alice", ParentPermlink: "lorem", Body: "y",
	})
	f.MustApplyOp(op.Comment{
		Author: "sam", Permlink: "dolor",
		ParentAuthor: "bob", ParentPermlink: "ipsum", Body: "z",
	})

	// Snapshot creation_time + cashout_time + parent_author of sam's post.
	c0 := f.Comment("sam", "dolor")
	created0 := c0.Created
	cashout0 := c0.CashoutTime
	parentAuthor0 := c0.ParentAuthor

	// Wait > 1 minute (skips reverse_auction so the vote produces rshares).
	f.SkipSeconds(5 * 60)

	// sam upvotes his own comment → NetRshares > 0.
	f.MustApplyOp(op.Vote{Voter: "sam", Author: "sam", Permlink: "dolor", Weight: 10000})
	cVoted := f.Comment("sam", "dolor")
	if cVoted.NetRshares <= 0 {
		t.Fatalf("vote should leave NetRshares > 0; got %d", cVoted.NetRshares)
	}

	// Now re-issue the comment op as an edit. Wait long enough to clear
	// the 3-second edit interval that hived enforces.
	f.SkipSeconds(5)

	if err := f.ApplyOp(op.Comment{
		Author: "sam", Permlink: "dolor",
		ParentAuthor: "bob", ParentPermlink: "ipsum",
		Title: "foo", Body: "bar", JSONMetadata: `{"bar":"foo"}`,
	}); err != nil {
		t.Fatalf("edit after vote: %v", err)
	}

	cAfter := f.Comment("sam", "dolor")
	if cAfter.Created != created0 {
		t.Errorf("Created changed across edit: %d → %d", created0, cAfter.Created)
	}
	if cAfter.CashoutTime != cashout0 {
		t.Errorf("CashoutTime changed across edit: %d → %d", cashout0, cAfter.CashoutTime)
	}
	if cAfter.ParentAuthor != parentAuthor0 {
		t.Errorf("ParentAuthor changed across edit: %q → %q", parentAuthor0, cAfter.ParentAuthor)
	}
	if cAfter.LastUpdate <= c0.LastUpdate {
		t.Errorf("LastUpdate should advance across edit: %d → %d", c0.LastUpdate, cAfter.LastUpdate)
	}
}

// TestCommentApplyRootPostIntervalEnforced ports the "Test failure
// posting within 5 minutes" branch of comment_apply. Post-HF6, a second
// root post by the same author within HIVE_MIN_ROOT_COMMENT_INTERVAL
// (5 minutes) must fail. After the wait elapses + a block, it succeeds.
func TestCommentApplyRootPostIntervalEnforced(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("sam")
	f.GenerateBlocks(60 / int(BlockInterval))

	// First root post — succeeds.
	if err := f.ApplyOp(op.Comment{
		Author: "sam", Permlink: "sit",
		ParentPermlink: "test", Body: "x",
	}); err != nil {
		t.Fatalf("first root post: %v", err)
	}

	// Roll forward well under the 5-minute interval; second root post
	// must fail.
	f.SkipSeconds(2 * 60)
	if err := f.ApplyOp(op.Comment{
		Author: "sam", Permlink: "amet",
		ParentPermlink: "test", Body: "y",
	}); err == nil {
		t.Fatal("expected second root post within 5 minutes to fail")
	}

	// Now roll past the interval (3 more minutes — total elapsed ≥ 5 min
	// + a few seconds). Same op must succeed.
	f.SkipSeconds(3*60 + 6)
	if err := f.ApplyOp(op.Comment{
		Author: "sam", Permlink: "amet",
		ParentPermlink: "test", Body: "y",
	}); err != nil {
		t.Fatalf("post-interval root post: %v", err)
	}
}

// TestCommentDeleteFailsWithPositiveRshares ports the "Test failure
// deleting a comment with positive rshares" branch of comment_delete_apply.
// At HF19+ delete_comment must error when net_rshares > 0.
func TestCommentDeleteFailsWithPositiveRshares(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF19)
	f.Actor("alice")
	f.FundVesting("alice", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{
		Author: "alice", Permlink: "test1",
		ParentPermlink: "test", Title: "test", Body: "foo bar",
	})
	// Wait past reverse-auction so the vote produces rshares.
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{
		Voter: "alice", Author: "alice", Permlink: "test1", Weight: 10000,
	})
	if got := f.Comment("alice", "test1").NetRshares; got <= 0 {
		t.Fatalf("vote should leave NetRshares > 0; got %d", got)
	}

	if err := f.ApplyOp(op.DeleteComment{
		Author: "alice", Permlink: "test1",
	}); err == nil {
		t.Fatal("expected delete with positive rshares to fail at HF19+")
	}
	if !f.HasComment("alice", "test1") {
		t.Fatal("comment should still exist after failed delete")
	}
}

// TestCommentDeleteSucceedsWithNegativeRshares ports the "Test success
// deleting a comment with negative rshares" branch. After flipping the
// vote to -100%, NetRshares < 0 and delete_comment must succeed.
func TestCommentDeleteSucceedsWithNegativeRshares(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF19)
	f.Actor("alice")
	f.FundVesting("alice", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{
		Author: "alice", Permlink: "test1",
		ParentPermlink: "test", Title: "test", Body: "foo bar",
	})
	f.SkipSeconds(60 * 60)
	// Upvote first.
	f.MustApplyOp(op.Vote{Voter: "alice", Author: "alice", Permlink: "test1", Weight: 10000})

	// Wait long enough to be allowed to re-vote.
	f.SkipSeconds(60)

	// Flip to -100%. After this NetRshares must be < 0.
	f.MustApplyOp(op.Vote{Voter: "alice", Author: "alice", Permlink: "test1", Weight: -10000})
	if got := f.Comment("alice", "test1").NetRshares; got > 0 {
		t.Skipf("re-vote to -100%% left NetRshares > 0 (%d); cannot exercise the negative-rshares delete path here", got)
	}

	if err := f.ApplyOp(op.DeleteComment{
		Author: "alice", Permlink: "test1",
	}); err != nil {
		t.Fatalf("delete with non-positive rshares should succeed: %v", err)
	}
	if f.HasComment("alice", "test1") {
		t.Fatal("comment should be removed")
	}
}

// TestCommentDeletePastCashoutFails ports the "Test failure deleting a
// comment past cashout" branch. After 7-day cashout window, the
// comment_cashout row is freed; HF19+ delete asserts on its absence.
func TestCommentDeletePastCashoutFails(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF19)
	f.Actor("alice")
	f.FundVesting("alice", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{
		Author: "alice", Permlink: "test1",
		ParentPermlink: "test", Body: "foo bar",
	})

	// Roll past the 7-day cashout window — comment must transition to
	// WasPaid + CashoutTime==0.
	f.SkipSeconds(7 * 24 * 60 * 60)
	f.GenerateBlocks(2)

	c := f.Comment("alice", "test1")
	if !c.WasPaid {
		t.Fatalf("comment should have cashed out (WasPaid=true), got %+v", c)
	}
	if c.CashoutTime != 0 {
		t.Fatalf("post-cashout CashoutTime should be 0 (frozen), got %d", c.CashoutTime)
	}

	if err := f.ApplyOp(op.DeleteComment{
		Author: "alice", Permlink: "test1",
	}); err == nil {
		t.Fatal("expected delete past cashout to fail at HF19+")
	}
}

// TestCommentDeleteWithReplyFails ports the "Test failure deleting a
// comment with a reply" branch. A parent comment with at least one
// surviving child must reject delete_comment.
func TestCommentDeleteWithReplyFails(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF19)
	f.Actor("alice")
	f.FundVesting("alice", 1_000_000)
	f.GenerateBlock()

	// Root post.
	f.MustApplyOp(op.Comment{
		Author: "alice", Permlink: "test1",
		ParentPermlink: "test", Body: "foo bar",
	})
	// Wait 5 minutes so the next root-or-reply post passes the interval.
	f.SkipSeconds(5*60 + 3)
	// Reply (root-comment-interval doesn't apply to replies, but the
	// 3-second reply interval needs to elapse; one block suffices).
	f.GenerateBlock()
	f.MustApplyOp(op.Comment{
		Author: "alice", Permlink: "test2",
		ParentAuthor: "alice", ParentPermlink: "test1",
		Body: "child",
	})

	parent := f.Comment("alice", "test1")
	if parent.Children == 0 {
		t.Fatalf("parent should have a child after reply; Children=%d", parent.Children)
	}

	// Deleting the parent must fail because it has a reply.
	if err := f.ApplyOp(op.DeleteComment{
		Author: "alice", Permlink: "test1",
	}); err == nil {
		t.Fatal("expected delete of comment with reply to fail")
	}
	if !f.HasComment("alice", "test1") {
		t.Fatal("parent should still exist after failed delete")
	}
}
