package op

import "unsafe"

// Per-tag accessors for Op.ptr. Each AsX() returns a typed pointer into
// the underlying concrete-op storage. The Tag should match before use;
// helpers do not validate the tag for hot-path speed.
//
// This file is mechanical — one accessor per op tag, in tag order.

func (o Op) AsVote() *Vote                                 { return (*Vote)(o.ptr) }
func (o Op) AsComment() *Comment                           { return (*Comment)(o.ptr) }
func (o Op) AsTransfer() *Transfer                         { return (*Transfer)(o.ptr) }
func (o Op) AsTransferToVesting() *TransferToVesting       { return (*TransferToVesting)(o.ptr) }
func (o Op) AsWithdrawVesting() *WithdrawVesting           { return (*WithdrawVesting)(o.ptr) }
func (o Op) AsLimitOrderCreate() *LimitOrderCreate         { return (*LimitOrderCreate)(o.ptr) }
func (o Op) AsLimitOrderCancel() *LimitOrderCancel         { return (*LimitOrderCancel)(o.ptr) }
func (o Op) AsFeedPublish() *FeedPublish                   { return (*FeedPublish)(o.ptr) }
func (o Op) AsConvert() *Convert                           { return (*Convert)(o.ptr) }
func (o Op) AsAccountCreate() *AccountCreate               { return (*AccountCreate)(o.ptr) }
func (o Op) AsAccountUpdate() *AccountUpdate               { return (*AccountUpdate)(o.ptr) }
func (o Op) AsWitnessUpdate() *WitnessUpdate               { return (*WitnessUpdate)(o.ptr) }
func (o Op) AsAccountWitnessVote() *AccountWitnessVote     { return (*AccountWitnessVote)(o.ptr) }
func (o Op) AsAccountWitnessProxy() *AccountWitnessProxy   { return (*AccountWitnessProxy)(o.ptr) }
func (o Op) AsPow() *Pow                                   { return (*Pow)(o.ptr) }
func (o Op) AsCustom() *Custom                             { return (*Custom)(o.ptr) }
func (o Op) AsWitnessBlockApprove() *WitnessBlockApprove   { return (*WitnessBlockApprove)(o.ptr) }
func (o Op) AsDeleteComment() *DeleteComment               { return (*DeleteComment)(o.ptr) }
func (o Op) AsCustomJSON() *CustomJSON                     { return (*CustomJSON)(o.ptr) }
func (o Op) AsCommentOptions() *CommentOptions             { return (*CommentOptions)(o.ptr) }
func (o Op) AsSetWithdrawVestingRoute() *SetWithdrawVestingRoute {
	return (*SetWithdrawVestingRoute)(o.ptr)
}
func (o Op) AsLimitOrderCreate2() *LimitOrderCreate2 { return (*LimitOrderCreate2)(o.ptr) }
func (o Op) AsClaimAccount() *ClaimAccount           { return (*ClaimAccount)(o.ptr) }
func (o Op) AsCreateClaimedAccount() *CreateClaimedAccount {
	return (*CreateClaimedAccount)(o.ptr)
}
func (o Op) AsRequestAccountRecovery() *RequestAccountRecovery {
	return (*RequestAccountRecovery)(o.ptr)
}
func (o Op) AsRecoverAccount() *RecoverAccount               { return (*RecoverAccount)(o.ptr) }
func (o Op) AsChangeRecoveryAccount() *ChangeRecoveryAccount { return (*ChangeRecoveryAccount)(o.ptr) }
func (o Op) AsEscrowTransfer() *EscrowTransfer               { return (*EscrowTransfer)(o.ptr) }
func (o Op) AsEscrowDispute() *EscrowDispute                 { return (*EscrowDispute)(o.ptr) }
func (o Op) AsEscrowRelease() *EscrowRelease                 { return (*EscrowRelease)(o.ptr) }
func (o Op) AsPow2() *Pow2                                   { return (*Pow2)(o.ptr) }
func (o Op) AsEscrowApprove() *EscrowApprove                 { return (*EscrowApprove)(o.ptr) }
func (o Op) AsTransferToSavings() *TransferToSavings         { return (*TransferToSavings)(o.ptr) }
func (o Op) AsTransferFromSavings() *TransferFromSavings     { return (*TransferFromSavings)(o.ptr) }
func (o Op) AsCancelTransferFromSavings() *CancelTransferFromSavings {
	return (*CancelTransferFromSavings)(o.ptr)
}
func (o Op) AsCustomBinary() *CustomBinary             { return (*CustomBinary)(o.ptr) }
func (o Op) AsDeclineVotingRights() *DeclineVotingRights {
	return (*DeclineVotingRights)(o.ptr)
}
func (o Op) AsResetAccount() *ResetAccount             { return (*ResetAccount)(o.ptr) }
func (o Op) AsSetResetAccount() *SetResetAccount       { return (*SetResetAccount)(o.ptr) }
func (o Op) AsClaimRewardBalance() *ClaimRewardBalance { return (*ClaimRewardBalance)(o.ptr) }
func (o Op) AsDelegateVestingShares() *DelegateVestingShares {
	return (*DelegateVestingShares)(o.ptr)
}
func (o Op) AsAccountCreateWithDelegation() *AccountCreateWithDelegation {
	return (*AccountCreateWithDelegation)(o.ptr)
}
func (o Op) AsWitnessSetProperties() *WitnessSetProperties { return (*WitnessSetProperties)(o.ptr) }
func (o Op) AsAccountUpdate2() *AccountUpdate2             { return (*AccountUpdate2)(o.ptr) }
func (o Op) AsCreateProposal() *CreateProposal             { return (*CreateProposal)(o.ptr) }
func (o Op) AsUpdateProposalVotes() *UpdateProposalVotes   { return (*UpdateProposalVotes)(o.ptr) }
func (o Op) AsRemoveProposal() *RemoveProposal             { return (*RemoveProposal)(o.ptr) }
func (o Op) AsUpdateProposal() *UpdateProposal             { return (*UpdateProposal)(o.ptr) }
func (o Op) AsCollateralizedConvert() *CollateralizedConvert {
	return (*CollateralizedConvert)(o.ptr)
}
func (o Op) AsRecurrentTransfer() *RecurrentTransfer { return (*RecurrentTransfer)(o.ptr) }

// initOpAsIface populates the per-tag conversion table. Each entry takes
// the raw ptr and returns the boxed Operation iface. Used by Op.Operation()
// during the migration.
func init() {
	opAsIface[TagVote] = func(p unsafe.Pointer) Operation { return *(*Vote)(p) }
	opAsIface[TagComment] = func(p unsafe.Pointer) Operation { return *(*Comment)(p) }
	opAsIface[TagTransfer] = func(p unsafe.Pointer) Operation { return *(*Transfer)(p) }
	opAsIface[TagTransferToVesting] = func(p unsafe.Pointer) Operation { return *(*TransferToVesting)(p) }
	opAsIface[TagWithdrawVesting] = func(p unsafe.Pointer) Operation { return *(*WithdrawVesting)(p) }
	opAsIface[TagLimitOrderCreate] = func(p unsafe.Pointer) Operation { return *(*LimitOrderCreate)(p) }
	opAsIface[TagLimitOrderCancel] = func(p unsafe.Pointer) Operation { return *(*LimitOrderCancel)(p) }
	opAsIface[TagFeedPublish] = func(p unsafe.Pointer) Operation { return *(*FeedPublish)(p) }
	opAsIface[TagConvert] = func(p unsafe.Pointer) Operation { return *(*Convert)(p) }
	opAsIface[TagAccountCreate] = func(p unsafe.Pointer) Operation { return *(*AccountCreate)(p) }
	opAsIface[TagAccountUpdate] = func(p unsafe.Pointer) Operation { return *(*AccountUpdate)(p) }
	opAsIface[TagWitnessUpdate] = func(p unsafe.Pointer) Operation { return *(*WitnessUpdate)(p) }
	opAsIface[TagAccountWitnessVote] = func(p unsafe.Pointer) Operation { return *(*AccountWitnessVote)(p) }
	opAsIface[TagAccountWitnessProxy] = func(p unsafe.Pointer) Operation { return *(*AccountWitnessProxy)(p) }
	opAsIface[TagPow] = func(p unsafe.Pointer) Operation { return *(*Pow)(p) }
	opAsIface[TagCustom] = func(p unsafe.Pointer) Operation { return *(*Custom)(p) }
	opAsIface[TagWitnessBlockApprove] = func(p unsafe.Pointer) Operation { return *(*WitnessBlockApprove)(p) }
	opAsIface[TagDeleteComment] = func(p unsafe.Pointer) Operation { return *(*DeleteComment)(p) }
	opAsIface[TagCustomJSON] = func(p unsafe.Pointer) Operation { return *(*CustomJSON)(p) }
	opAsIface[TagCommentOptions] = func(p unsafe.Pointer) Operation { return *(*CommentOptions)(p) }
	opAsIface[TagSetWithdrawVestingRoute] = func(p unsafe.Pointer) Operation { return *(*SetWithdrawVestingRoute)(p) }
	opAsIface[TagLimitOrderCreate2] = func(p unsafe.Pointer) Operation { return *(*LimitOrderCreate2)(p) }
	opAsIface[TagClaimAccount] = func(p unsafe.Pointer) Operation { return *(*ClaimAccount)(p) }
	opAsIface[TagCreateClaimedAccount] = func(p unsafe.Pointer) Operation { return *(*CreateClaimedAccount)(p) }
	opAsIface[TagRequestAccountRecovery] = func(p unsafe.Pointer) Operation { return *(*RequestAccountRecovery)(p) }
	opAsIface[TagRecoverAccount] = func(p unsafe.Pointer) Operation { return *(*RecoverAccount)(p) }
	opAsIface[TagChangeRecoveryAccount] = func(p unsafe.Pointer) Operation { return *(*ChangeRecoveryAccount)(p) }
	opAsIface[TagEscrowTransfer] = func(p unsafe.Pointer) Operation { return *(*EscrowTransfer)(p) }
	opAsIface[TagEscrowDispute] = func(p unsafe.Pointer) Operation { return *(*EscrowDispute)(p) }
	opAsIface[TagEscrowRelease] = func(p unsafe.Pointer) Operation { return *(*EscrowRelease)(p) }
	opAsIface[TagPow2] = func(p unsafe.Pointer) Operation { return *(*Pow2)(p) }
	opAsIface[TagEscrowApprove] = func(p unsafe.Pointer) Operation { return *(*EscrowApprove)(p) }
	opAsIface[TagTransferToSavings] = func(p unsafe.Pointer) Operation { return *(*TransferToSavings)(p) }
	opAsIface[TagTransferFromSavings] = func(p unsafe.Pointer) Operation { return *(*TransferFromSavings)(p) }
	opAsIface[TagCancelTransferFromSavings] = func(p unsafe.Pointer) Operation { return *(*CancelTransferFromSavings)(p) }
	opAsIface[TagCustomBinary] = func(p unsafe.Pointer) Operation { return *(*CustomBinary)(p) }
	opAsIface[TagDeclineVotingRights] = func(p unsafe.Pointer) Operation { return *(*DeclineVotingRights)(p) }
	opAsIface[TagResetAccount] = func(p unsafe.Pointer) Operation { return *(*ResetAccount)(p) }
	opAsIface[TagSetResetAccount] = func(p unsafe.Pointer) Operation { return *(*SetResetAccount)(p) }
	opAsIface[TagClaimRewardBalance] = func(p unsafe.Pointer) Operation { return *(*ClaimRewardBalance)(p) }
	opAsIface[TagDelegateVestingShares] = func(p unsafe.Pointer) Operation { return *(*DelegateVestingShares)(p) }
	opAsIface[TagAccountCreateWithDelegation] = func(p unsafe.Pointer) Operation { return *(*AccountCreateWithDelegation)(p) }
	opAsIface[TagWitnessSetProperties] = func(p unsafe.Pointer) Operation { return *(*WitnessSetProperties)(p) }
	opAsIface[TagAccountUpdate2] = func(p unsafe.Pointer) Operation { return *(*AccountUpdate2)(p) }
	opAsIface[TagCreateProposal] = func(p unsafe.Pointer) Operation { return *(*CreateProposal)(p) }
	opAsIface[TagUpdateProposalVotes] = func(p unsafe.Pointer) Operation { return *(*UpdateProposalVotes)(p) }
	opAsIface[TagRemoveProposal] = func(p unsafe.Pointer) Operation { return *(*RemoveProposal)(p) }
	opAsIface[TagUpdateProposal] = func(p unsafe.Pointer) Operation { return *(*UpdateProposal)(p) }
	opAsIface[TagCollateralizedConvert] = func(p unsafe.Pointer) Operation { return *(*CollateralizedConvert)(p) }
	opAsIface[TagRecurrentTransfer] = func(p unsafe.Pointer) Operation { return *(*RecurrentTransfer)(p) }
}
