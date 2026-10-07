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

// AccountCreatedVOp mirrors hived's account_created_operation. It is the op
// that tells a consumer an account now exists and what it started with, so
// every creation path pushes one — account_create, account_create_with_
// delegation, create_claimed_account and the two pow paths.
//
// InitialVestingShares is only non-zero on the paths that actually vested
// the fee: from HF20 the creation fee is burned to the null account instead
// (hive_evaluator_account.cpp:208-212), and create_claimed_account pays no
// fee at all, so zero there is the true value rather than a missing one.
type AccountCreatedVOp struct {
	NewAccountName       string
	Creator              string
	InitialVestingShares int64 // VESTS satoshi
	InitialDelegation    int64 // VESTS satoshi
}

// TransferToVestingCompletedVOp mirrors hived's
// transfer_to_vesting_completed_operation. hived emits it unconditionally
// and says why in a comment at hive_evaluator_transfer.cpp:334: the VESTS
// balance moved immediately, independent of whatever delayed_voting does to
// the voting power afterwards. So it must not be gated on the delayed-voting
// branch, nor skipped when the vest conversion rounds to zero.
type TransferToVestingCompletedVOp struct {
	FromAccount           string
	ToAccount             string
	HiveVested            int64 // HIVE satoshi
	VestingSharesReceived int64 // VESTS satoshi
}

// PowRewardVOp mirrors hived's pow_reward_operation. The reward's CURRENCY
// changes with the chain's age: liquid HIVE before HIVE_START_MINER_VOTING_
// BLOCK, VESTS after (hive_evaluator.cpp:485-494). RewardIsVests says which,
// because an op carrying VESTS labelled HIVE is off by the share price.
//
// The worker is the witness that INCLUDED the proof of work
// (dgp.current_witness), not the miner who solved it.
type PowRewardVOp struct {
	Worker        string
	Reward        int64
	RewardIsVests bool
}

// EmitAccountCreated is a convenience that pushes the vop through
// ctx.EmitVOp if non-nil.
func (c *Context) EmitAccountCreated(newAccount, creator string, initialVestingShares, initialDelegation int64) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(AccountCreatedVOp{
		NewAccountName:       newAccount,
		Creator:              creator,
		InitialVestingShares: initialVestingShares,
		InitialDelegation:    initialDelegation,
	})
}

// EmitTransferToVestingCompleted is a convenience that pushes the vop
// through ctx.EmitVOp if non-nil.
func (c *Context) EmitTransferToVestingCompleted(from, to string, hiveVested, vestingSharesReceived int64) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(TransferToVestingCompletedVOp{
		FromAccount:           from,
		ToAccount:             to,
		HiveVested:            hiveVested,
		VestingSharesReceived: vestingSharesReceived,
	})
}

// EmitPowReward is a convenience that pushes the vop through ctx.EmitVOp
// if non-nil.
func (c *Context) EmitPowReward(worker string, reward int64, isVests bool) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(PowRewardVOp{Worker: worker, Reward: reward, RewardIsVests: isVests})
}

// EscrowApprovedVOp mirrors hived's escrow_approved_operation, pushed the
// moment BOTH the recipient and the agent have ratified and the agent's fee
// is released (hive_evaluator_transfer.cpp:175).
//
// hived does not gate it on a non-zero fee — the op reports that the escrow
// became active, and a zero-fee escrow becoming active is exactly as much
// news as any other.
type EscrowApprovedVOp struct {
	From     string
	To       string
	Agent    string
	EscrowID uint32
	Fee      int64
	FeeIsHBD bool
}

// EscrowRejectedVOp mirrors hived's escrow_rejected_operation. It reports
// every asset returned to the originator: the HBD, the HIVE and the agent
// fee, which may itself be denominated in either currency — hence FeeIsHBD.
//
// hived emits it from remove_escrow on two of three call sites. The third,
// remove_pending_escrows, passes emit_rejected_vop = false: that is the
// HF23 account clear, where the escrow is not being rejected by anyone and
// the funds are on their way to the treasury rather than back to the
// originator. See the corresponding silence in hardfork_accounts.go.
type EscrowRejectedVOp struct {
	From       string
	To         string
	Agent      string
	EscrowID   uint32
	HbdAmount  int64
	HiveAmount int64
	Fee        int64
	FeeIsHBD   bool
}

// EmitEscrowApproved is a convenience that pushes the vop through
// ctx.EmitVOp if non-nil.
func (c *Context) EmitEscrowApproved(from, to, agent string, escrowID uint32, fee int64, feeIsHBD bool) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(EscrowApprovedVOp{
		From: from, To: to, Agent: agent,
		EscrowID: escrowID, Fee: fee, FeeIsHBD: feeIsHBD,
	})
}

// EmitEscrowRejected is a convenience that pushes the vop through
// ctx.EmitVOp if non-nil.
func (c *Context) EmitEscrowRejected(from, to, agent string, escrowID uint32, hbd, hive, fee int64, feeIsHBD bool) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(EscrowRejectedVOp{
		From: from, To: to, Agent: agent, EscrowID: escrowID,
		HbdAmount: hbd, HiveAmount: hive, Fee: fee, FeeIsHBD: feeIsHBD,
	})
}

// LimitOrderCancelledVOp mirrors hived's limit_order_cancelled_operation,
// pushed by cancel_order (database_market.cpp:81) with the amount refunded
// to the seller, in the order's sell asset.
type LimitOrderCancelledVOp struct {
	Seller          string
	OrderID         uint32
	AmountBack      int64
	AmountBackIsHBD bool
}

// EmitLimitOrderCancelled pushes the vop through ctx.EmitVOp if non-nil.
func (c *Context) EmitLimitOrderCancelled(seller string, orderID uint32, amountBack int64, isHBD bool) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(LimitOrderCancelledVOp{Seller: seller, OrderID: orderID, AmountBack: amountBack, AmountBackIsHBD: isHBD})
}

// FillOrderVOp mirrors hived's fill_order_operation, pushed once per match
// (database_market.cpp:342) BEFORE either side is filled: the incoming
// ("current") order and the resting ("open") one, each with what it pays.
type FillOrderVOp struct {
	CurrentOwner     string
	CurrentOrderID   uint32
	CurrentPays      int64
	CurrentPaysIsHBD bool
	OpenOwner        string
	OpenOrderID      uint32
	OpenPays         int64
	OpenPaysIsHBD    bool
}

// EmitFillOrder pushes the vop through ctx.EmitVOp if non-nil.
func (c *Context) EmitFillOrder(v FillOrderVOp) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(v)
}

// CollateralizedConvertImmediateConversionVOp mirrors
// collateralized_convert_immediate_conversion_operation
// (hive_evaluator_transfer.cpp:532): the HBD handed out right away against
// the collateral, before the request itself settles 3.5 days later.
type CollateralizedConvertImmediateConversionVOp struct {
	Owner     string
	RequestID uint32
	HbdOut    int64
}

// EmitCollateralizedConvertImmediateConversion pushes the vop through
// ctx.EmitVOp if non-nil.
func (c *Context) EmitCollateralizedConvertImmediateConversion(owner string, requestID uint32, hbdOut int64) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(CollateralizedConvertImmediateConversionVOp{Owner: owner, RequestID: requestID, HbdOut: hbdOut})
}

// IneffectiveDeleteCommentVOp mirrors ineffective_delete_comment_operation
// (hive_evaluator_social.cpp:74): pre-HF19 a delete_comment on a comment
// with net positive votes is a silent no-op, and this is how a consumer
// learns the delete did nothing.
type IneffectiveDeleteCommentVOp struct {
	Author   string
	Permlink string
}

// EmitIneffectiveDeleteComment pushes the vop through ctx.EmitVOp if non-nil.
func (c *Context) EmitIneffectiveDeleteComment(author, permlink string) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(IneffectiveDeleteCommentVOp{Author: author, Permlink: permlink})
}

// DhfConversionVOp mirrors dhf_conversion_operation as pushed by the
// transfer evaluator (hive_evaluator_transfer.cpp:289) when HIVE sent to
// the treasury is converted to HBD on the way in. The daily DHF hook has
// its own emission in chain/proposals.go.
type DhfConversionVOp struct {
	Treasury string
	HiveIn   int64
	HbdOut   int64
}

// EmitDhfConversion pushes the vop through ctx.EmitVOp if non-nil.
func (c *Context) EmitDhfConversion(treasury string, hiveIn, hbdOut int64) {
	if c.EmitVOp == nil {
		return
	}
	c.EmitVOp(DhfConversionVOp{Treasury: treasury, HiveIn: hiveIn, HbdOut: hbdOut})
}
