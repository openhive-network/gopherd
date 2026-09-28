package evaluator

// Per-evaluator virtual-op payload types. Evaluators construct one of
// these and call ctx.EmitVOp(payload); the chain.Database closure
// supplied at Context construction routes them onto its per-block
// chain.BlockVOp buffer.
//
// These mirror the relevant fields of the C++ virtual operations
// (proposal_fee_operation, proxy_cleared_operation). Keeping them in
// the evaluator package avoids an import cycle on chain.

// ProposalFeeVOp mirrors hived's proposal_fee_operation. Emitted from
// the create_proposal evaluator after the fee transfer to the treasury
// account, before the proposal row is stored.
type ProposalFeeVOp struct {
	Creator    string
	Treasury   string
	ProposalID int64
	Fee        int64 // satoshi HBD
}

// ProxyClearedVOp mirrors hived's proxy_cleared_operation. Emitted in
// four places:
//
//  1. account_witness_proxy with proxy=""        (account removes proxy)
//  2. account_witness_proxy with proxy=B,old=A   (account overwrites proxy)
//  3. decline_voting_rights settlement           (30-day window elapses)
//  4. governance vote expiration                 (365-day window post-HF25)
type ProxyClearedVOp struct {
	Account string
	Proxy   string // the OLD proxy that was cleared
}

// EmitProposalFee is a convenience that pushes the vop through
// ctx.EmitVOp if non-nil.
func (c *Context) EmitProposalFee(creator, treasury string, proposalID, fee int64) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(ProposalFeeVOp{
		Creator:    creator,
		Treasury:   treasury,
		ProposalID: proposalID,
		Fee:        fee,
	})
}

// EmitProxyCleared is a convenience that pushes the vop through
// ctx.EmitVOp if non-nil.
func (c *Context) EmitProxyCleared(account, oldProxy string) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(ProxyClearedVOp{Account: account, Proxy: oldProxy})
}

// EffectiveCommentVoteVOp mirrors hived's effective_comment_vote_operation,
// pushed at the END of both vote evaluators (hive_evaluator_social.cpp:604
// and :853) — the point where the vote's curation weight is final. A vote
// that was rejected, or skipped because the comment is already paid, returns
// before it, which is what makes the op mean "this vote counted".
//
// Weight is the curation weight actually STORED on the comment_vote row, not
// the operation's percentage: on a pre-HF26 recast that is zero (the edit
// penalty), and a consumer reading o.weight from the transaction instead
// would credit curation to a vote that earns none.
//
// PendingPayout is deliberately absent. hived declares it on the op but the
// consensus node always emits zero — the field is "supplemented by AH
// RocksDB plugin" (hive_virtual_operations.hpp:429), an account-history
// concern, and computing it here would put a different number on the wire
// than hived does.
type EffectiveCommentVoteVOp struct {
	Voter           string
	Author          string
	Permlink        string
	Weight          uint64
	Rshares         int64
	TotalVoteWeight uint64
}

// EmitEffectiveCommentVote is a convenience that pushes the vop through
// ctx.EmitVOp if non-nil.
func (c *Context) EmitEffectiveCommentVote(voter, author, permlink string, weight uint64, rshares int64, totalVoteWeight uint64) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(EffectiveCommentVoteVOp{
		Voter:           voter,
		Author:          author,
		Permlink:        permlink,
		Weight:          weight,
		Rshares:         rshares,
		TotalVoteWeight: totalVoteWeight,
	})
}
