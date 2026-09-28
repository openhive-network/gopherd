package op

import (
	"encoding/binary"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
)

// Virtual operations.
//
// These never appear inside a transaction — evaluators and per-block hooks
// construct them, and they are emitted into the block's virtual-op stream.
// They share the `operation` static_variant space with regular ops, continuing
// its numbering at TagFirstVirtual (50), so they serialize through exactly the
// same varint(tag) || body framing as everything else. That is what lets a
// block+vop stream be read by anything that already speaks hived's operation
// encoding.
//
// Tag order is hived's, from the static_variant declaration in
// libraries/protocol/include/hive/protocol/operations.hpp. It is not
// alphabetical and not grouped by subject; the numbering is the wire contract,
// so the order here is the order there.
//
// Field order within each body is likewise the FC_REFLECT order from
// hive_virtual_operations.hpp — asset members carry their own symbol, so a
// reordered field is not a decode error, just silently wrong data.

const (
	TagFillConvertRequest Tag = TagFirstVirtual + iota
	TagAuthorReward
	TagCurationReward
	TagCommentReward
	TagLiquidityReward
	TagInterest
	TagFillVestingWithdraw
	TagFillOrder
	TagShutdownWitness
	TagFillTransferFromSavings
	TagHardfork
	TagCommentPayoutUpdate
	TagReturnVestingDelegation
	TagCommentBenefactorReward
	TagProducerReward
	TagClearNullAccountBalance
	TagProposalPay
	TagDhfFunding
	TagHardforkHive
	TagHardforkHiveRestore
	TagDelayedVoting
	TagConsolidateTreasuryBalance
	TagEffectiveCommentVote
	TagIneffectiveDeleteComment
	TagDhfConversion
	TagExpiredAccountNotification
	TagChangedRecoveryAccount
	TagTransferToVestingCompleted
	TagPowReward
	TagVestingSharesSplit
	TagAccountCreated
	TagFillCollateralizedConvertRequest
	TagSystemWarning
	TagFillRecurrentTransfer
	TagFailedRecurrentTransfer
	TagLimitOrderCancelled
	TagProducerMissed
	TagProposalFee
	TagCollateralizedConvertImmediateConversion
	TagEscrowApproved
	TagEscrowRejected
	TagProxyCleared
	TagDeclinedVotingRights

	// TagLastVirtual is one past the final virtual tag.
	TagLastVirtual
)

// FillConvertRequest — a convert_operation maturing.
type FillConvertRequest struct {
	Owner     auth.AccountName
	RequestID uint32
	AmountIn  asset.Asset // HBD
	AmountOut asset.Asset // HIVE
}

func (FillConvertRequest) OpTag() Tag { return TagFillConvertRequest }
func (o FillConvertRequest) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = appendVopU32(dst, o.RequestID)
	dst = asset.AppendAsset(dst, o.AmountIn)
	return asset.AppendAsset(dst, o.AmountOut)
}

// AuthorReward — the author's share of a comment payout.
//
// CuratorsVestingPayout is the exact sum actually paid to curators, not a
// conversion of the allotted amount at the post-payout price (hive e3b8a39d5).
type AuthorReward struct {
	Author                auth.AccountName
	Permlink              string
	HbdPayout             asset.Asset
	HivePayout            asset.Asset
	VestingPayout         asset.Asset
	CuratorsVestingPayout asset.Asset
	PayoutMustBeClaimed   bool
}

func (AuthorReward) OpTag() Tag { return TagAuthorReward }
func (o AuthorReward) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	dst = asset.AppendAsset(dst, o.HbdPayout)
	dst = asset.AppendAsset(dst, o.HivePayout)
	dst = asset.AppendAsset(dst, o.VestingPayout)
	dst = asset.AppendAsset(dst, o.CuratorsVestingPayout)
	return appendVopBool(dst, o.PayoutMustBeClaimed)
}

// CurationReward — one curator's share, emitted per curator.
type CurationReward struct {
	Curator             auth.AccountName
	Reward              asset.Asset // VESTS
	Author              auth.AccountName
	Permlink            string
	PayoutMustBeClaimed bool
}

func (CurationReward) OpTag() Tag { return TagCurationReward }
func (o CurationReward) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Curator)
	dst = asset.AppendAsset(dst, o.Reward)
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	return appendVopBool(dst, o.PayoutMustBeClaimed)
}

// CommentReward — the comment-level summary, emitted once per cashout.
type CommentReward struct {
	Author                 auth.AccountName
	Permlink               string
	Payout                 asset.Asset // HBD
	AuthorRewards          int64       // HIVE satoshi, raw
	TotalPayoutValue       asset.Asset
	CuratorPayoutValue     asset.Asset
	BeneficiaryPayoutValue asset.Asset
}

func (CommentReward) OpTag() Tag { return TagCommentReward }
func (o CommentReward) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	dst = asset.AppendAsset(dst, o.Payout)
	dst = appendVopI64(dst, o.AuthorRewards)
	dst = asset.AppendAsset(dst, o.TotalPayoutValue)
	dst = asset.AppendAsset(dst, o.CuratorPayoutValue)
	return asset.AppendAsset(dst, o.BeneficiaryPayoutValue)
}

// Interest — HBD interest paid.
//
// IsSavedIntoHbdBalance records whether the LIQUID balance was modified, which
// stops happening at HF25.
type Interest struct {
	Owner                 auth.AccountName
	Interest              asset.Asset // HBD
	IsSavedIntoHbdBalance bool
}

func (Interest) OpTag() Tag { return TagInterest }
func (o Interest) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = asset.AppendAsset(dst, o.Interest)
	return appendVopBool(dst, o.IsSavedIntoHbdBalance)
}

// FillVestingWithdraw — one power-down instalment, emitted per destination
// route (including the implied route back to the owner).
type FillVestingWithdraw struct {
	FromAccount auth.AccountName
	ToAccount   auth.AccountName
	Withdrawn   asset.Asset // VESTS
	Deposited   asset.Asset // HIVE or VESTS depending on the route
}

func (FillVestingWithdraw) OpTag() Tag { return TagFillVestingWithdraw }
func (o FillVestingWithdraw) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.FromAccount)
	dst = appendString(dst, o.ToAccount)
	dst = asset.AppendAsset(dst, o.Withdrawn)
	return asset.AppendAsset(dst, o.Deposited)
}

// ProducerReward — the block producer's reward.
//
// VestingShares is VESTS except during the first 30 days after genesis, when
// it is HIVE; the asset carries its own symbol either way.
type ProducerReward struct {
	Producer      auth.AccountName
	VestingShares asset.Asset
}

func (ProducerReward) OpTag() Tag { return TagProducerReward }
func (o ProducerReward) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Producer)
	return asset.AppendAsset(dst, o.VestingShares)
}

// CommentPayoutUpdate — emitted once per comment when its payout state is
// finalised. hived is explicit that this must appear exactly once per comment.
type CommentPayoutUpdate struct {
	Author   auth.AccountName
	Permlink string
}

func (CommentPayoutUpdate) OpTag() Tag { return TagCommentPayoutUpdate }
func (o CommentPayoutUpdate) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Author)
	return appendString(dst, o.Permlink)
}

// CommentBenefactorReward — a beneficiary's share of a comment payout.
type CommentBenefactorReward struct {
	Benefactor          auth.AccountName
	Author              auth.AccountName
	Permlink            string
	HbdPayout           asset.Asset
	HivePayout          asset.Asset
	VestingPayout       asset.Asset
	PayoutMustBeClaimed bool
}

func (CommentBenefactorReward) OpTag() Tag { return TagCommentBenefactorReward }
func (o CommentBenefactorReward) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Benefactor)
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	dst = asset.AppendAsset(dst, o.HbdPayout)
	dst = asset.AppendAsset(dst, o.HivePayout)
	dst = asset.AppendAsset(dst, o.VestingPayout)
	return appendVopBool(dst, o.PayoutMustBeClaimed)
}

// Hardfork — emitted when a hardfork activates.
type Hardfork struct {
	HardforkID uint32
}

func (Hardfork) OpTag() Tag                       { return TagHardfork }
func (o Hardfork) AppendFCBody(dst []byte) []byte { return appendVopU32(dst, o.HardforkID) }

// ProducerMissed — a scheduled witness that failed to produce.
type ProducerMissed struct {
	Producer auth.AccountName
}

func (ProducerMissed) OpTag() Tag { return TagProducerMissed }
func (o ProducerMissed) AppendFCBody(dst []byte) []byte {
	return appendString(dst, o.Producer)
}

// ShutdownWitness — a witness disabled for missing too many blocks.
type ShutdownWitness struct {
	Owner auth.AccountName
}

func (ShutdownWitness) OpTag() Tag { return TagShutdownWitness }
func (o ShutdownWitness) AppendFCBody(dst []byte) []byte {
	return appendString(dst, o.Owner)
}

// ExpiredAccountNotification — governance votes expired for this account.
type ExpiredAccountNotification struct {
	Account auth.AccountName
}

func (ExpiredAccountNotification) OpTag() Tag { return TagExpiredAccountNotification }
func (o ExpiredAccountNotification) AppendFCBody(dst []byte) []byte {
	return appendString(dst, o.Account)
}

// DeclinedVotingRights — a decline_voting_rights request taking effect.
type DeclinedVotingRights struct {
	Account auth.AccountName
}

func (DeclinedVotingRights) OpTag() Tag { return TagDeclinedVotingRights }
func (o DeclinedVotingRights) AppendFCBody(dst []byte) []byte {
	return appendString(dst, o.Account)
}

// ProxyCleared — an account's proxy removed.
type ProxyCleared struct {
	Account auth.AccountName
	Proxy   auth.AccountName
}

func (ProxyCleared) OpTag() Tag { return TagProxyCleared }
func (o ProxyCleared) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	return appendString(dst, o.Proxy)
}

// ReturnVestingDelegation — a delegation's cooldown expiring.
type ReturnVestingDelegation struct {
	Account       auth.AccountName
	VestingShares asset.Asset
}

func (ReturnVestingDelegation) OpTag() Tag { return TagReturnVestingDelegation }
func (o ReturnVestingDelegation) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	return asset.AppendAsset(dst, o.VestingShares)
}

// FillTransferFromSavings — a savings withdrawal maturing.
type FillTransferFromSavings struct {
	From      auth.AccountName
	To        auth.AccountName
	Amount    asset.Asset
	RequestID uint32
	Memo      string
}

func (FillTransferFromSavings) OpTag() Tag { return TagFillTransferFromSavings }
func (o FillTransferFromSavings) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.Amount)
	dst = appendVopU32(dst, o.RequestID)
	return appendString(dst, o.Memo)
}

// Small encoders local to the virtual ops. The regular-op files each inline
// these; naming them here keeps 43 bodies readable.
func appendVopBool(dst []byte, b bool) []byte {
	if b {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func appendVopU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendVopI64(dst []byte, v int64) []byte {
	u := uint64(v)
	return append(dst, byte(u), byte(u>>8), byte(u>>16), byte(u>>24),
		byte(u>>32), byte(u>>40), byte(u>>48), byte(u>>56))
}

// FillRecurrentTransfer — one instalment of a recurrent transfer.
//
// Extensions carries the raw recurrent_transfer_extensions_type (only
// recurrent_transfer_pair_id is defined), encoded exactly as the regular
// RecurrentTransfer does it.
type FillRecurrentTransfer struct {
	From                auth.AccountName
	To                  auth.AccountName
	Amount              asset.Asset
	Memo                string
	RemainingExecutions uint16
	Extensions          []byte
}

func (FillRecurrentTransfer) OpTag() Tag { return TagFillRecurrentTransfer }
func (o FillRecurrentTransfer) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.Amount)
	dst = appendString(dst, o.Memo)
	dst = binary.LittleEndian.AppendUint16(dst, o.RemainingExecutions)
	return appendRawExtensions(dst, o.Extensions)
}

// FailedRecurrentTransfer — an instalment that could not be paid.
//
// Field order is the FC_REFLECT order, which is NOT the constructor's: memo
// comes BEFORE consecutive_failures on the wire while the constructor takes
// them the other way round. Following the constructor here would produce a
// body that decodes without error and means something else.
type FailedRecurrentTransfer struct {
	From                auth.AccountName
	To                  auth.AccountName
	Amount              asset.Asset
	Memo                string
	ConsecutiveFailures uint8
	RemainingExecutions uint16
	Deleted             bool
	Extensions          []byte
}

func (FailedRecurrentTransfer) OpTag() Tag { return TagFailedRecurrentTransfer }
func (o FailedRecurrentTransfer) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.Amount)
	dst = appendString(dst, o.Memo)
	dst = append(dst, o.ConsecutiveFailures)
	dst = binary.LittleEndian.AppendUint16(dst, o.RemainingExecutions)
	dst = appendVopBool(dst, o.Deleted)
	return appendRawExtensions(dst, o.Extensions)
}

// ProposalFee — the fee charged for creating a proposal.
type ProposalFee struct {
	Creator    auth.AccountName
	Treasury   auth.AccountName
	ProposalID uint32
	Fee        asset.Asset
}

func (ProposalFee) OpTag() Tag { return TagProposalFee }
func (o ProposalFee) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Creator)
	dst = appendString(dst, o.Treasury)
	dst = appendVopU32(dst, o.ProposalID)
	return asset.AppendAsset(dst, o.Fee)
}

// ProposalPay — a proposal's periodic payment from the treasury.
type ProposalPay struct {
	ProposalID uint32
	Receiver   auth.AccountName
	Payer      auth.AccountName
	Payment    asset.Asset
}

func (ProposalPay) OpTag() Tag { return TagProposalPay }
func (o ProposalPay) AppendFCBody(dst []byte) []byte {
	dst = appendVopU32(dst, o.ProposalID)
	dst = appendString(dst, o.Receiver)
	dst = appendString(dst, o.Payer)
	return asset.AppendAsset(dst, o.Payment)
}

// LiquidityReward — the market-maker reward (dead since HF12).
type LiquidityReward struct {
	Owner  auth.AccountName
	Payout asset.Asset
}

func (LiquidityReward) OpTag() Tag { return TagLiquidityReward }
func (o LiquidityReward) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	return asset.AppendAsset(dst, o.Payout)
}

// ClearNullAccountBalance — the per-block sweep of whatever was sent to @null.
type ClearNullAccountBalance struct {
	TotalCleared []asset.Asset
}

func (ClearNullAccountBalance) OpTag() Tag { return TagClearNullAccountBalance }
func (o ClearNullAccountBalance) AppendFCBody(dst []byte) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(o.TotalCleared)))
	for _, a := range o.TotalCleared {
		dst = asset.AppendAsset(dst, a)
	}
	return dst
}

// LimitOrderCancelled — an order removed with its unfilled remainder returned.
type LimitOrderCancelled struct {
	Seller     auth.AccountName
	OrderID    uint32
	AmountBack asset.Asset
}

func (LimitOrderCancelled) OpTag() Tag { return TagLimitOrderCancelled }
func (o LimitOrderCancelled) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Seller)
	dst = appendVopU32(dst, o.OrderID)
	return asset.AppendAsset(dst, o.AmountBack)
}

// RecurrentTransferPairIDExtension builds the raw
// recurrent_transfer_extensions_type carrying a pair id, or an empty set when
// the id is 0.
//
// hived omits the extension entirely for pair id 0 rather than encoding a
// zero: the two forms are different bytes, and a consumer diffing against
// hived's stream sees every such op as a mismatch.
func RecurrentTransferPairIDExtension(pairID uint8) []byte {
	if pairID == 0 {
		return nil
	}
	// One entry: varint(count=1) || varint(tag=1) || uint8 pair_id.
	out := fcraw.AppendVarUint(nil, 1)
	out = fcraw.AppendVarUint(out, 1)
	return append(out, pairID)
}

// ---- the remaining virtual ops -------------------------------------------
//
// Field order below is the FC_REFLECT order in every case. Where a struct's
// C++ constructor takes its arguments in a different order — as
// failed_recurrent_transfer's does — the reflect order is the one on the wire.

// FillOrder — a limit order matching. Emitted once per match, naming both
// sides: `current` is the incoming order, `open` the resting one.
type FillOrder struct {
	CurrentOwner   auth.AccountName
	CurrentOrderID uint32
	CurrentPays    asset.Asset
	OpenOwner      auth.AccountName
	OpenOrderID    uint32
	OpenPays       asset.Asset
}

func (FillOrder) OpTag() Tag { return TagFillOrder }
func (o FillOrder) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.CurrentOwner)
	dst = appendVopU32(dst, o.CurrentOrderID)
	dst = asset.AppendAsset(dst, o.CurrentPays)
	dst = appendString(dst, o.OpenOwner)
	dst = appendVopU32(dst, o.OpenOrderID)
	return asset.AppendAsset(dst, o.OpenPays)
}

// EffectiveCommentVote — what a vote actually counted for, after the curation
// curve and the rshares cap. PendingPayout is the comment's projected payout
// at this moment, not a settled amount.
type EffectiveCommentVote struct {
	Voter           auth.AccountName
	Author          auth.AccountName
	Permlink        string
	Weight          uint64
	Rshares         int64
	TotalVoteWeight uint64
	PendingPayout   asset.Asset
}

func (EffectiveCommentVote) OpTag() Tag { return TagEffectiveCommentVote }
func (o EffectiveCommentVote) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Voter)
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	dst = appendVopU64(dst, o.Weight)
	dst = appendVopI64(dst, o.Rshares)
	dst = appendVopU64(dst, o.TotalVoteWeight)
	return asset.AppendAsset(dst, o.PendingPayout)
}

// IneffectiveDeleteComment — a delete_comment that could not take effect.
type IneffectiveDeleteComment struct {
	Author   auth.AccountName
	Permlink string
}

func (IneffectiveDeleteComment) OpTag() Tag { return TagIneffectiveDeleteComment }
func (o IneffectiveDeleteComment) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Author)
	return appendString(dst, o.Permlink)
}

// AccountCreated — a new account, with whatever VESTS it started with.
type AccountCreated struct {
	NewAccountName       auth.AccountName
	Creator              auth.AccountName
	InitialVestingShares asset.Asset
	InitialDelegation    asset.Asset
}

func (AccountCreated) OpTag() Tag { return TagAccountCreated }
func (o AccountCreated) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.NewAccountName)
	dst = appendString(dst, o.Creator)
	dst = asset.AppendAsset(dst, o.InitialVestingShares)
	return asset.AppendAsset(dst, o.InitialDelegation)
}

// TransferToVestingCompleted — a power-up settling, naming both the HIVE spent
// and the VESTS received.
type TransferToVestingCompleted struct {
	FromAccount           auth.AccountName
	ToAccount             auth.AccountName
	HiveVested            asset.Asset
	VestingSharesReceived asset.Asset
}

func (TransferToVestingCompleted) OpTag() Tag { return TagTransferToVestingCompleted }
func (o TransferToVestingCompleted) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.FromAccount)
	dst = appendString(dst, o.ToAccount)
	dst = asset.AppendAsset(dst, o.HiveVested)
	return asset.AppendAsset(dst, o.VestingSharesReceived)
}

// PowReward — a mining reward.
type PowReward struct {
	Worker auth.AccountName
	Reward asset.Asset
}

func (PowReward) OpTag() Tag { return TagPowReward }
func (o PowReward) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Worker)
	return asset.AppendAsset(dst, o.Reward)
}

// VestingSharesSplit — the HF1 VESTS redenomination, emitted per account.
type VestingSharesSplit struct {
	Owner                    auth.AccountName
	VestingSharesBeforeSplit asset.Asset
	VestingSharesAfterSplit  asset.Asset
}

func (VestingSharesSplit) OpTag() Tag { return TagVestingSharesSplit }
func (o VestingSharesSplit) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = asset.AppendAsset(dst, o.VestingSharesBeforeSplit)
	return asset.AppendAsset(dst, o.VestingSharesAfterSplit)
}

// ChangedRecoveryAccount — a recovery-account change taking effect after its
// 30-day delay.
type ChangedRecoveryAccount struct {
	Account            auth.AccountName
	OldRecoveryAccount auth.AccountName
	NewRecoveryAccount auth.AccountName
}

func (ChangedRecoveryAccount) OpTag() Tag { return TagChangedRecoveryAccount }
func (o ChangedRecoveryAccount) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = appendString(dst, o.OldRecoveryAccount)
	return appendString(dst, o.NewRecoveryAccount)
}

// DelayedVoting — VESTS entering or leaving the delayed-voting pool (HF24+).
type DelayedVoting struct {
	Voter auth.AccountName
	Votes uint64
}

func (DelayedVoting) OpTag() Tag { return TagDelayedVoting }
func (o DelayedVoting) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Voter)
	return appendVopU64(dst, o.Votes)
}

// DhfFunding — HBD moved into the treasury for the interval.
type DhfFunding struct {
	Treasury        auth.AccountName
	AdditionalFunds asset.Asset
}

func (DhfFunding) OpTag() Tag { return TagDhfFunding }
func (o DhfFunding) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Treasury)
	return asset.AppendAsset(dst, o.AdditionalFunds)
}

// DhfConversion — the treasury's daily HIVE→HBD conversion (HF24+).
type DhfConversion struct {
	Treasury     auth.AccountName
	HiveAmountIn asset.Asset
	HbdAmountOut asset.Asset
}

func (DhfConversion) OpTag() Tag { return TagDhfConversion }
func (o DhfConversion) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Treasury)
	dst = asset.AppendAsset(dst, o.HiveAmountIn)
	return asset.AppendAsset(dst, o.HbdAmountOut)
}

// ConsolidateTreasuryBalance — the HF24 move of the old treasury's balances.
type ConsolidateTreasuryBalance struct {
	TotalMoved []asset.Asset
}

func (ConsolidateTreasuryBalance) OpTag() Tag { return TagConsolidateTreasuryBalance }
func (o ConsolidateTreasuryBalance) AppendFCBody(dst []byte) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(o.TotalMoved)))
	for _, a := range o.TotalMoved {
		dst = asset.AppendAsset(dst, a)
	}
	return dst
}

// HardforkHive — the HF23 confiscation, per affected account.
type HardforkHive struct {
	Account               auth.AccountName
	Treasury              auth.AccountName
	OtherAffectedAccounts []auth.AccountName
	HbdTransferred        asset.Asset
	HiveTransferred       asset.Asset
	VestsConverted        asset.Asset
	TotalHiveFromVests    asset.Asset
}

func (HardforkHive) OpTag() Tag { return TagHardforkHive }
func (o HardforkHive) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = appendString(dst, o.Treasury)
	dst = fcraw.AppendVarUint(dst, uint32(len(o.OtherAffectedAccounts)))
	for _, a := range o.OtherAffectedAccounts {
		dst = appendString(dst, a)
	}
	dst = asset.AppendAsset(dst, o.HbdTransferred)
	dst = asset.AppendAsset(dst, o.HiveTransferred)
	dst = asset.AppendAsset(dst, o.VestsConverted)
	return asset.AppendAsset(dst, o.TotalHiveFromVests)
}

// HardforkHiveRestore — the HF24 return of confiscated funds.
type HardforkHiveRestore struct {
	Account         auth.AccountName
	Treasury        auth.AccountName
	HbdTransferred  asset.Asset
	HiveTransferred asset.Asset
}

func (HardforkHiveRestore) OpTag() Tag { return TagHardforkHiveRestore }
func (o HardforkHiveRestore) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = appendString(dst, o.Treasury)
	dst = asset.AppendAsset(dst, o.HbdTransferred)
	return asset.AppendAsset(dst, o.HiveTransferred)
}

// FillCollateralizedConvertRequest — a collateralized convert maturing, with
// whatever collateral was not needed returned.
type FillCollateralizedConvertRequest struct {
	Owner            auth.AccountName
	RequestID        uint32
	AmountIn         asset.Asset
	AmountOut        asset.Asset
	ExcessCollateral asset.Asset
}

func (FillCollateralizedConvertRequest) OpTag() Tag {
	return TagFillCollateralizedConvertRequest
}
func (o FillCollateralizedConvertRequest) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = appendVopU32(dst, o.RequestID)
	dst = asset.AppendAsset(dst, o.AmountIn)
	dst = asset.AppendAsset(dst, o.AmountOut)
	return asset.AppendAsset(dst, o.ExcessCollateral)
}

// CollateralizedConvertImmediateConversion — the HBD handed over up front when
// a collateralized convert is created.
type CollateralizedConvertImmediateConversion struct {
	Owner     auth.AccountName
	RequestID uint32
	HbdOut    asset.Asset
}

func (CollateralizedConvertImmediateConversion) OpTag() Tag {
	return TagCollateralizedConvertImmediateConversion
}
func (o CollateralizedConvertImmediateConversion) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = appendVopU32(dst, o.RequestID)
	return asset.AppendAsset(dst, o.HbdOut)
}

// EscrowApproved — both agent and recipient having approved; the fee is paid.
type EscrowApproved struct {
	From     auth.AccountName
	To       auth.AccountName
	Agent    auth.AccountName
	EscrowID uint32
	Fee      asset.Asset
}

func (EscrowApproved) OpTag() Tag { return TagEscrowApproved }
func (o EscrowApproved) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = appendString(dst, o.Agent)
	dst = appendVopU32(dst, o.EscrowID)
	return asset.AppendAsset(dst, o.Fee)
}

// EscrowRejected — an escrow refused or expired unapproved; everything goes
// back, which is why this one names the amounts and EscrowApproved does not.
type EscrowRejected struct {
	From       auth.AccountName
	To         auth.AccountName
	Agent      auth.AccountName
	EscrowID   uint32
	HbdAmount  asset.Asset
	HiveAmount asset.Asset
	Fee        asset.Asset
}

func (EscrowRejected) OpTag() Tag { return TagEscrowRejected }
func (o EscrowRejected) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = appendString(dst, o.Agent)
	dst = appendVopU32(dst, o.EscrowID)
	dst = asset.AppendAsset(dst, o.HbdAmount)
	dst = asset.AppendAsset(dst, o.HiveAmount)
	return asset.AppendAsset(dst, o.Fee)
}

// SystemWarning — a node-level notice carried in the stream (used for the RC
// and block-size warnings hived surfaces).
type SystemWarning struct {
	Message string
}

func (SystemWarning) OpTag() Tag { return TagSystemWarning }
func (o SystemWarning) AppendFCBody(dst []byte) []byte {
	return appendString(dst, o.Message)
}

func appendVopU64(dst []byte, v uint64) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}
