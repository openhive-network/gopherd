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
