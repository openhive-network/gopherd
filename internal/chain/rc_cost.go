package chain

import (
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// HIVE_RC_NUM_RESOURCE_TYPES — 5 resource types: history_bytes,
// new_accounts, market_bytes, state_bytes, execution_time.
const rcNumResourceTypes = 5

// State-byte lifetime multipliers from hived's resource_sizes.cpp:
//
//	TEMPORARY_STATE_BYTE  = 1                   (1 hour-byte)
//	LASTING_STATE_BYTE    = 1 × 5 × 365 × 12   (~2.5 years lifetime)
//	PERSISTENT_STATE_BYTE = 1 × 5 × 365 × 24   (~5 years lifetime)
const (
	temporaryStateByte  int64 = 1
	lastingStateByte    int64 = 1 * 5 * 365 * 12 // 21,900
	persistentStateByte int64 = 1 * 5 * 365 * 24 // 43,800
)

// Multi-index node sizes in bytes (from resource_sizes.cpp's SIZE
// macros). These are the per-row state object sizes as measured by
// hived's chainbase introspection.
const (
	accountIndexNodeSize           int64 = 616 + 144 // = 760
	commentIndexNodeSize           int64 = 224
	convertIndexNodeSize           int64 = 96
	collateralConvertNodeSize      int64 = 128
	declineVotingRightsNodeSize    int64 = 128
	escrowIndexNodeSize            int64 = 216
	limitOrderIndexNodeSize        int64 = 208
	savingsWithdrawIndexNodeSize   int64 = 232
	transactionIndexNodeSize       int64 = 128
	vestingDelegationNodeSize      int64 = 88
	vestingDelegationExpNodeSize   int64 = 120
	withdrawVestingRouteNodeSize   int64 = 144
	witnessIndexNodeSize           int64 = 544
	witnessVoteIndexNodeSize       int64 = 136
	proposalIndexNodeSize          int64 = 336
	proposalVoteIndexNodeSize      int64 = 128
	recurrentTransferIndexNodeSize int64 = 200
	rcDirectDelegationNodeSize     int64 = 88
	ownerAuthHistoryObjectSize     int64 = 200
)

// Lifetime windows (in HOURS) for each TEMPORARY object. From
// resource_sizes.cpp.
const (
	limitOrderLifetimeHours        int64 = 28 * 24       // HIVE_MAX_LIMIT_ORDER_EXPIRATION (28 days)
	collateralConvertLifetimeHours int64 = 7 * 12        // 3.5 days
	declineVotingLifetimeHours     int64 = 30 * 24       // HIVE_OWNER_AUTH_RECOVERY_PERIOD
	savingsWithdrawLifetimeHours   int64 = 3 * 24        // HIVE_SAVINGS_WITHDRAW_TIME
	vestingDelegationExpHours      int64 = 5 * 24        // HIVE_DELEGATION_RETURN_PERIOD_HF20
	witnessVoteLifetimeHours       int64 = 365 * 24      // HIVE_GOVERNANCE_VOTE_EXPIRATION_PERIOD
	proposalVoteLifetimeHours      int64 = 365 * 24      // same
)

// Per-byte state costs for variable-length fields.
const (
	commentPermlinkCharSize       int64 = temporaryStateByte // dynamic
	witnessUpdateURLCharSize      int64 = persistentStateByte
	authorityAccountMemberSize    int64 = persistentStateByte * 80   // approx
	authorityKeyMemberSize        int64 = persistentStateByte * 80
	commentBeneficiariesMemberSize int64 = temporaryStateByte * 16
	createProposalSubjectCharSize  int64 = temporaryStateByte // × lifetime
	recurrentTransferMemoCharSize  int64 = temporaryStateByte // × lifetime
)

// Execution times (in nanoseconds) from resource_sizes.cpp's
// operation_exec_info constructor. These are calibrated against a
// reference machine — they mostly serve to scale across op types.
var execTimeNS = map[op.Tag]int64{
	op.TagAccountCreate:                 498 + 31575 + 236 + 6401,
	op.TagAccountCreateWithDelegation:   770 + 39616 + 139 + 66,
	op.TagAccountWitnessVote:            516 + 13907 + 173 + 145,
	op.TagComment:                       2419 + 20408 + 221 + 166 + 42964,
	op.TagCommentOptions:                413 + 5530 + 142 + 117,
	op.TagConvert:                       442 + 11491 + 207 + 143 + 8280,
	op.TagCollateralizedConvert:         613 + 13325 + 332 + 239 + 9154,
	op.TagCreateClaimedAccount:          964 + 46331 + 342 + 10778,
	op.TagDeclineVotingRights:           530 + 3894 + 179 + 187 + 24301,
	op.TagDelegateVestingShares:         411 + 11998 + 3110 + 1208,
	op.TagEscrowTransfer:                2943 + 16130 + 309 + 204,
	op.TagLimitOrderCreate:              468 + 13924 + 187 + 124,
	op.TagLimitOrderCreate2:             607 + 23356 + 310 + 118,
	op.TagRequestAccountRecovery:        613 + 7058 + 149 + 127,
	op.TagSetWithdrawVestingRoute:       501 + 9003 + 632 + 121,
	op.TagVote:                          479 + 17568 + 187 + 78,
	op.TagWitnessUpdate:                 675 + 3287 + 154 + 121,
	op.TagTransfer:                      638 + 5023 + 204 + 134,
	op.TagTransferToVesting:             484 + 15856 + 2174 + 721 + 23010,
	op.TagTransferToSavings:             771 + 5392 + 205 + 155,
	op.TagTransferFromSavings:           519 + 7840 + 111 + 106 + 8974,
	op.TagClaimRewardBalance:            427 + 23366 + 2798 + 970,
	op.TagWithdrawVesting:               376 + 6524 + 2068 + 567 + 16307*13, // HIVE_VESTING_WITHDRAW_INTERVALS
	op.TagAccountUpdate:                 1531 + 11433 + 206 + 152,
	op.TagAccountUpdate2:                2182 + 10804 + 405 + 257,
	op.TagAccountWitnessProxy:           684 + 57106 + 170 + 166,
	op.TagCancelTransferFromSavings:     135 + 2881 + 72 + 66,
	op.TagChangeRecoveryAccount:         580 + 9391 + 297 + 202,
	op.TagClaimAccount:                  336 + 8028 + 267 + 150,
	op.TagCustom:                        132 + 183 + 238 + 121,
	op.TagCustomJSON:                    859 + 162 + 321 + 167,
	op.TagCustomBinary:                  100000,
	op.TagDeleteComment:                 521 + 17208 + 175 + 146,
	op.TagEscrowApprove:                 814 + 8408 + 260 + 181,
	op.TagEscrowDispute:                 918 + 5219 + 261 + 176,
	op.TagEscrowRelease:                 1313 + 13586 + 252 + 189,
	op.TagFeedPublish:                   469 + 2233 + 213 + 144,
	op.TagLimitOrderCancel:              407 + 5986 + 204 + 142,
	op.TagWitnessSetProperties:          3986 + 4920 + 310 + 234,
	op.TagCreateProposal:                831 + 24323 + 5666 + 2168,
	op.TagUpdateProposal:                820 + 11193 + 4501 + 267,
	op.TagUpdateProposalVotes:           503 + 12602 + 2993 + 1206,
	op.TagRemoveProposal:                527 + 129323 + 3886 + 1692,
	op.TagRecurrentTransfer:             914 + 12426 + 330 + 238,
	op.TagRecoverAccount:                727 + 17934 + 149 + 145,
	op.TagPow:                           177321 + 23458 + 165 + 57,
	op.TagPow2:                          86349 + 17226 + 160 + 92,
}

// transactionExecTimeNS — per-tx overhead from operation_exec_info::
// transaction_time. Added once per tx regardless of op count.
const transactionExecTimeNS int64 = 2821 + 386 + 3415

// verifyAuthorityTimeNS — per-signature exec time. We don't count
// signatures during replay, but the constant is needed for live mode.
const verifyAuthorityTimeNS int64 = 94165

// stateBytesForOp returns the state-bytes resource cost for op `o`
// (in hour-bytes). For ops that create new chain state objects (orders,
// withdrawals, comments...), this is `node_size × lifetime_hours ×
// byte_cost_class`. For pure transfers (no state object), this is 0.
//
// Mirrors the count_operation_visitor methods in
// libraries/chain/rc/resource_count.cpp.
func stateBytesForOp(o op.Operation) int64 {
	switch v := o.(type) {
	case op.AccountCreate:
		// account_create_base_size ≈ 760 × persistent + authority_dynamic
		base := accountIndexNodeSize * persistentStateByte
		base += authorityDynamicSize(v.Owner)
		base += authorityDynamicSize(v.Active)
		base += authorityDynamicSize(v.Posting)
		return base
	case op.AccountCreateWithDelegation:
		base := accountIndexNodeSize * persistentStateByte
		base += authorityDynamicSize(v.Owner)
		base += authorityDynamicSize(v.Active)
		base += authorityDynamicSize(v.Posting)
		base += vestingDelegationNodeSize * lastingStateByte
		return base
	case op.CreateClaimedAccount:
		base := accountIndexNodeSize * persistentStateByte
		base += authorityDynamicSize(v.Owner)
		base += authorityDynamicSize(v.Active)
		base += authorityDynamicSize(v.Posting)
		return base
	case op.AccountUpdate:
		var s int64
		if v.Owner != nil {
			s += authorityDynamicSize(*v.Owner)
			s += ownerAuthHistoryObjectSize * persistentStateByte
		}
		if v.Active != nil {
			s += authorityDynamicSize(*v.Active)
		}
		if v.Posting != nil {
			s += authorityDynamicSize(*v.Posting)
		}
		return s
	case op.AccountWitnessVote:
		if !v.Approve {
			return 0
		}
		return witnessVoteIndexNodeSize * temporaryStateByte * witnessVoteLifetimeHours
	case op.Comment:
		return commentIndexNodeSize*temporaryStateByte +
			commentPermlinkCharSize*int64(len(v.Permlink))
	case op.CommentOptions:
		return commentBeneficiariesMemberSize * int64(len(v.Beneficiaries))
	case op.Convert:
		return convertIndexNodeSize * temporaryStateByte * 7 * 12 * 24 // 3.5 days
	case op.CollateralizedConvert:
		return collateralConvertNodeSize * temporaryStateByte * collateralConvertLifetimeHours
	case op.DeclineVotingRights:
		if !v.Decline {
			return 0
		}
		return declineVotingRightsNodeSize * temporaryStateByte * declineVotingLifetimeHours
	case op.DelegateVestingShares:
		// max(vesting_delegation_object, vesting_delegation_expiration_object)
		a := vestingDelegationNodeSize * lastingStateByte
		b := vestingDelegationExpNodeSize * temporaryStateByte * vestingDelegationExpHours
		if a > b {
			return a
		}
		return b
	case op.EscrowTransfer:
		return escrowIndexNodeSize * lastingStateByte
	case op.LimitOrderCreate:
		// FillOrKill orders don't persist (they fill or cancel immediately).
		if v.FillOrKill {
			return 0
		}
		return limitOrderIndexNodeSize * temporaryStateByte * limitOrderLifetimeHours
	case op.LimitOrderCreate2:
		if v.FillOrKill {
			return 0
		}
		return limitOrderIndexNodeSize * temporaryStateByte * limitOrderLifetimeHours
	case op.SetWithdrawVestingRoute:
		if v.Percent == 0 {
			return 0
		}
		return withdrawVestingRouteNodeSize * lastingStateByte
	case op.WitnessUpdate:
		return witnessIndexNodeSize*persistentStateByte +
			witnessUpdateURLCharSize*int64(len(v.URL))
	case op.TransferToVesting:
		// transfer_to_vesting_size — noted as 0 in resource_sizes.cpp
		// (no new state created; only existing balances mutated).
		return 0
	case op.TransferFromSavings:
		return savingsWithdrawIndexNodeSize * temporaryStateByte * savingsWithdrawLifetimeHours
	case op.RequestAccountRecovery:
		// new_owner_authority weight_threshold == 0 cancels — no state.
		// We don't have the field on our op type yet; conservative
		// estimate the full size.
		return 200 * temporaryStateByte * declineVotingLifetimeHours
	case op.Vote:
		return commentIndexNodeSize * temporaryStateByte // approx vote_size
	case op.RecurrentTransfer:
		base := recurrentTransferIndexNodeSize * temporaryStateByte
		base += recurrentTransferMemoCharSize * int64(len(v.Memo))
		// hived multiplies by actual lifetime; we approximate.
		return base * 24 * int64(v.Recurrence)
	case op.CreateProposal:
		base := proposalIndexNodeSize * temporaryStateByte
		base += createProposalSubjectCharSize * int64(len(v.Subject)+len(v.Permlink))
		// Multiply by actual lifetime: end - start, in hours.
		lifetimeHours := int64(v.EndDate-v.StartDate) / 3600
		if lifetimeHours <= 0 {
			lifetimeHours = 1
		}
		return base * lifetimeHours
	case op.UpdateProposalVotes:
		return proposalVoteIndexNodeSize * temporaryStateByte * proposalVoteLifetimeHours *
			int64(len(v.ProposalIDs))
	}
	return 0
}

// authorityDynamicSize approximates authority dynamic size (counting
// account_auths + key_auths members). authorityAccountMemberSize and
// authorityKeyMemberSize are PERSISTENT_STATE_BYTE × 80 in hived.
func authorityDynamicSize(a interface{}) int64 {
	// We don't have a direct way to walk auth.Authority members
	// from this package without adding an import. Use a flat estimate
	// of 200 bytes per authority — typical hived authorities have
	// 1-2 key_auths and 0 account_auths, so 80 bytes × 1-2 ≈ 80-160.
	_ = a
	return 200
}

// ComputeRCCost returns the total RC cost of `signedTx` at `blockNum`,
// summing state_bytes + execution_time + tx-base costs. Returns 0
// pre-HF20 (RC subsystem inactive).
//
// Hived's actual compute_cost (rc_utility.cpp:286) divides by per-resource
// pool regen rates. We approximate by SUMMING the resource costs
// directly — this gives order-of-magnitude parity but not exact match.
// Full parity requires tracking the rc_pool_object across blocks.
func ComputeRCCost(signedTx *tx.SignedTransaction, blockNum uint32) int64 {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) {
		return 0
	}
	cost := transactionExecTimeNS
	for _, o := range signedTx.Operations {
		cost += stateBytesForOp(o.Operation())
		if t, ok := execTimeNS[o.Tag]; ok {
			cost += t
		}
	}
	return cost
}

// PayerOf returns the account that pays the RC for the given tx.
// Mirrors hived's logic: the first required-active or required-posting
// authority. We use the canonical "from / account / owner / voter /
// creator" field of the op.
func PayerOf(o op.Operation) string {
	switch v := o.(type) {
	case op.Transfer:
		return string(v.From)
	case op.TransferToVesting:
		return string(v.From)
	case op.WithdrawVesting:
		return string(v.Account)
	case op.Vote:
		return string(v.Voter)
	case op.Comment:
		return string(v.Author)
	case op.LimitOrderCreate:
		return string(v.Owner)
	case op.LimitOrderCreate2:
		return string(v.Owner)
	case op.LimitOrderCancel:
		return string(v.Owner)
	case op.FeedPublish:
		return string(v.Publisher)
	case op.Convert:
		return string(v.Owner)
	case op.AccountCreate:
		return string(v.Creator)
	case op.AccountCreateWithDelegation:
		return string(v.Creator)
	case op.AccountUpdate:
		return string(v.Account)
	case op.WitnessUpdate:
		return string(v.Owner)
	case op.AccountWitnessVote:
		return string(v.Account)
	case op.AccountWitnessProxy:
		return string(v.Account)
	case op.DeleteComment:
		return string(v.Author)
	case op.CommentOptions:
		return string(v.Author)
	case op.SetWithdrawVestingRoute:
		return string(v.FromAccount)
	case op.ClaimAccount:
		return string(v.Creator)
	case op.CreateClaimedAccount:
		return string(v.Creator)
	case op.DelegateVestingShares:
		return string(v.Delegator)
	case op.TransferToSavings:
		return string(v.From)
	case op.TransferFromSavings:
		return string(v.From)
	case op.CancelTransferFromSavings:
		return string(v.From)
	case op.RecurrentTransfer:
		return string(v.From)
	case op.ClaimRewardBalance:
		return string(v.Account)
	case op.DeclineVotingRights:
		return string(v.Account)
	case op.CreateProposal:
		return string(v.Creator)
	case op.UpdateProposalVotes:
		return string(v.Voter)
	case op.RemoveProposal:
		return string(v.ProposalOwner)
	case op.UpdateProposal:
		return string(v.Creator)
	case op.CustomJSON:
		if len(v.RequiredPostingAuths) > 0 {
			return string(v.RequiredPostingAuths[0])
		}
		if len(v.RequiredAuths) > 0 {
			return string(v.RequiredAuths[0])
		}
	}
	return ""
}
