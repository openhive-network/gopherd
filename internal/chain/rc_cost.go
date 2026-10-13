package chain

import (
	"gopherd/internal/chain/evaluator"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
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

// State sizes, verbatim from state_object_size_info's constructor
// (libraries/chain/rc/resource_sizes.cpp:55-164). Each is
// SIZE(multiindex node) × byte-class × lifetime-hours, already multiplied
// out; the names follow hived's so the two tables can be diffed side by
// side. An earlier version of this table carried approximations ("≈ 760 ×
// persistent", a flat 200 per authority, comment = 224 × 1) that were off
// from hived by up to four orders of magnitude on the most common
// operations, which under HF29 (RC is consensus) would have been a split.
const (
	// account create (all versions): account node + account_authority node.
	rcAccountCreateBaseSize      = (616+144)*persistentStateByte + 312*persistentStateByte
	rcAuthorityAccountMemberSize = 24 * persistentStateByte
	rcAuthorityKeyMemberSize     = 36 * persistentStateByte
	// account update (both versions)
	rcOwnerAuthorityHistoryObjectSize = 168 * temporaryStateByte * 30 * 24 // HIVE_OWNER_AUTH_RECOVERY_PERIOD
	// transfer to vesting: delayed_votes_data for HIVE_DELAYED_VOTING_TOTAL_INTERVAL_SECONDS
	rcTransferToVestingSize = 16 * temporaryStateByte * 30 * 24
	// request account recovery: HIVE_ACCOUNT_RECOVERY_REQUEST_EXPIRATION_PERIOD
	rcRequestAccountRecoverySize = 192 * temporaryStateByte * 24
	// change recovery account: HIVE_OWNER_AUTH_RECOVERY_PERIOD
	rcChangeRecoveryAccountSize = 136 * temporaryStateByte * 30 * 24
	// comment: comment node (persistent) + comment_cashout node for HIVE_CASHOUT_WINDOW_SECONDS
	rcCommentBaseSize                = 96*persistentStateByte + 192*temporaryStateByte*7*24
	rcCommentPermlinkCharSize        = temporaryStateByte * 7 * 24
	rcCommentBeneficiariesMemberSize = 8 * temporaryStateByte * 7 * 24
	// vote: comment_vote node for HIVE_CASHOUT_WINDOW_SECONDS
	rcVoteSize = 144 * temporaryStateByte * 7 * 24
	// convert: HIVE_CONVERSION_DELAY (3.5 days)
	rcConvertSize = 120 * temporaryStateByte * 7 * 12
	// collateralized convert: HIVE_COLLATERALIZED_CONVERSION_DELAY
	rcCollateralizedConvertSize = 128 * temporaryStateByte * 7 * 12
	// decline voting rights: HIVE_OWNER_AUTH_RECOVERY_PERIOD
	rcDeclineVotingRightsSize = 128 * temporaryStateByte * 30 * 24
	// escrow transfer
	rcEscrowTransferSize = 216 * lastingStateByte
	// limit order create (both versions): HIVE_MAX_LIMIT_ORDER_EXPIRATION
	rcLimitOrderCreateSize = 208 * temporaryStateByte * 28 * 24
	// transfer from savings: HIVE_SAVINGS_WITHDRAW_TIME
	rcTransferFromSavingsSize = 232 * temporaryStateByte * 3 * 24
	// delegate vesting shares
	rcVestingDelegationObjectSize           = 88 * lastingStateByte
	rcVestingDelegationExpirationObjectSize = 120 * temporaryStateByte * 5 * 24 // HIVE_DELEGATION_RETURN_PERIOD_HF20
	// set withdraw vesting route
	rcSetWithdrawVestingRouteSize = 144 * lastingStateByte
	// witness update
	rcWitnessUpdateBaseSize    = 544 * persistentStateByte
	rcWitnessUpdateURLCharSize = persistentStateByte
	// account witness vote: HIVE_GOVERNANCE_VOTE_EXPIRATION_PERIOD
	rcAccountWitnessVoteSize = 136 * temporaryStateByte * 365 * 24
	// create proposal: × actual lifetime (hours, rounded up)
	rcCreateProposalBaseSize                = 336 * temporaryStateByte
	rcCreateProposalSubjectPermlinkCharSize = temporaryStateByte
	// update proposal votes: HIVE_GOVERNANCE_VOTE_EXPIRATION_PERIOD, × ids
	rcUpdateProposalVotesMemberSize = 128 * temporaryStateByte * 365 * 24
	// recurrent transfer: × actual lifetime (recurrence × executions hours)
	rcRecurrentTransferBaseSize     = 200 * temporaryStateByte
	rcRecurrentTransferMemoCharSize = temporaryStateByte
	// direct rc delegation: × number of delegatees
	rcDelegateRCBaseSize = 88 * lastingStateByte

	// Node sizes the differential visitor (rc_differential.go) refers to by
	// name.
	witnessIndexNodeSize       int64 = 544
	rcDirectDelegationNodeSize int64 = 88
	ownerAuthHistoryObjectSize int64 = 168
	witnessUpdateURLCharSize         = rcWitnessUpdateURLCharSize
	authorityAccountMemberSize       = rcAuthorityAccountMemberSize
	authorityKeyMemberSize           = rcAuthorityKeyMemberSize
)

// Execution-time pieces hived keeps outside the per-op table
// (resource_sizes.cpp:225-227).
const (
	rcRecurrentTransferProcessingTimeNS int64 = 14347 // × executions
	rcDelegateRCTimeNS                  int64 = 75000 // on top of custom_json_time
)

// Execution times (in nanoseconds) from resource_sizes.cpp's
// operation_exec_info constructor. These are calibrated against a
// reference machine — they mostly serve to scale across op types.
var execTimeNS = map[op.Tag]int64{
	op.TagAccountCreate:               498 + 31575 + 236 + 6401,
	op.TagAccountCreateWithDelegation: 770 + 39616 + 139 + 66,
	op.TagAccountWitnessVote:          516 + 13907 + 173 + 145,
	op.TagComment:                     2419 + 20408 + 221 + 166 + 42964,
	op.TagCommentOptions:              413 + 5530 + 142 + 117,
	op.TagConvert:                     442 + 11491 + 207 + 143 + 8280,
	op.TagCollateralizedConvert:       613 + 13325 + 332 + 239 + 9154,
	op.TagCreateClaimedAccount:        964 + 46331 + 342 + 10778,
	op.TagDeclineVotingRights:         530 + 3894 + 179 + 187 + 24301,
	op.TagDelegateVestingShares:       411 + 11998 + 3110 + 1208,
	op.TagEscrowTransfer:              2943 + 16130 + 309 + 204,
	op.TagLimitOrderCreate:            468 + 13924 + 187 + 124,
	op.TagLimitOrderCreate2:           607 + 23356 + 310 + 118,
	op.TagRequestAccountRecovery:      613 + 7058 + 149 + 127,
	op.TagSetWithdrawVestingRoute:     501 + 9003 + 632 + 121,
	op.TagVote:                        479 + 17568 + 187 + 78,
	op.TagWitnessUpdate:               675 + 3287 + 154 + 121,
	op.TagTransfer:                    638 + 5023 + 204 + 134,
	op.TagTransferToVesting:           484 + 15856 + 2174 + 721 + 23010,
	op.TagTransferToSavings:           771 + 5392 + 205 + 155,
	op.TagTransferFromSavings:         519 + 7840 + 111 + 106 + 8974,
	op.TagClaimRewardBalance:          427 + 23366 + 2798 + 970,
	op.TagWithdrawVesting:             376 + 6524 + 2068 + 567 + 16307*13, // HIVE_VESTING_WITHDRAW_INTERVALS
	op.TagAccountUpdate:               1531 + 11433 + 206 + 152,
	op.TagAccountUpdate2:              2182 + 10804 + 405 + 257,
	op.TagAccountWitnessProxy:         684 + 57106 + 170 + 166,
	op.TagCancelTransferFromSavings:   135 + 2881 + 72 + 66,
	op.TagChangeRecoveryAccount:       580 + 9391 + 297 + 202,
	op.TagClaimAccount:                336 + 8028 + 267 + 150,
	op.TagCustom:                      132 + 183 + 238 + 121,
	op.TagCustomJSON:                  859 + 162 + 321 + 167,
	op.TagCustomBinary:                100000,
	op.TagDeleteComment:               521 + 17208 + 175 + 146,
	op.TagEscrowApprove:               814 + 8408 + 260 + 181,
	op.TagEscrowDispute:               918 + 5219 + 261 + 176,
	op.TagEscrowRelease:               1313 + 13586 + 252 + 189,
	op.TagFeedPublish:                 469 + 2233 + 213 + 144,
	op.TagLimitOrderCancel:            407 + 5986 + 204 + 142,
	op.TagWitnessSetProperties:        3986 + 4920 + 310 + 234,
	op.TagCreateProposal:              831 + 24323 + 5666 + 2168,
	op.TagUpdateProposal:              820 + 11193 + 4501 + 267,
	op.TagUpdateProposalVotes:         503 + 12602 + 2993 + 1206,
	op.TagRemoveProposal:              527 + 129323 + 3886 + 1692,
	op.TagRecurrentTransfer:           914 + 12426 + 330 + 238,
	op.TagRecoverAccount:              727 + 17934 + 149 + 145,
}

// transactionExecTimeNS — per-tx overhead from operation_exec_info::
// transaction_time. Added once per tx regardless of op count.
const transactionExecTimeNS int64 = 2821 + 386 + 3415

// verifyAuthorityTimeNS — per-signature exec time. We don't count
// signatures during replay, but the constant is needed for live mode.
const verifyAuthorityTimeNS int64 = 94165

// stateBytesForOp returns the state_bytes resource usage of one operation,
// mirroring count_operation_visitor in rc/resource_count.cpp:196-553 case
// by case. `now` is head_block_time, used for create_proposal's lifetime.
// Operations not listed consume no state bytes in hived either.
func stateBytesForOp(o op.Op, now uint32) int64 {
	switch o.Tag {
	case op.TagAccountCreate:
		a := o.AsAccountCreate()
		return rcAccountCreateBaseSize +
			authorityDynamicSize(&a.Owner) + authorityDynamicSize(&a.Active) + authorityDynamicSize(&a.Posting)
	case op.TagAccountCreateWithDelegation:
		a := o.AsAccountCreateWithDelegation()
		return rcAccountCreateBaseSize +
			authorityDynamicSize(&a.Owner) + authorityDynamicSize(&a.Active) + authorityDynamicSize(&a.Posting) +
			rcVestingDelegationObjectSize
	case op.TagCreateClaimedAccount:
		a := o.AsCreateClaimedAccount()
		return rcAccountCreateBaseSize +
			authorityDynamicSize(&a.Owner) + authorityDynamicSize(&a.Active) + authorityDynamicSize(&a.Posting)
	case op.TagAccountUpdate:
		a := o.AsAccountUpdate()
		var s int64
		if a.Owner != nil {
			s += authorityDynamicSize(a.Owner) + rcOwnerAuthorityHistoryObjectSize
		}
		if a.Active != nil {
			s += authorityDynamicSize(a.Active)
		}
		if a.Posting != nil {
			s += authorityDynamicSize(a.Posting)
		}
		return s
	case op.TagAccountUpdate2:
		a := o.AsAccountUpdate2()
		var s int64
		if a.Owner != nil {
			s += authorityDynamicSize(a.Owner) + rcOwnerAuthorityHistoryObjectSize
		}
		if a.Active != nil {
			s += authorityDynamicSize(a.Active)
		}
		if a.Posting != nil {
			s += authorityDynamicSize(a.Posting)
		}
		return s
	case op.TagAccountWitnessVote:
		if o.AsAccountWitnessVote().Approve {
			return rcAccountWitnessVoteSize
		}
		return 0
	case op.TagComment:
		return rcCommentBaseSize + rcCommentPermlinkCharSize*int64(len(o.AsComment().Permlink))
	case op.TagCommentOptions:
		// comment_payout_beneficiaries extension only.
		return rcCommentBeneficiariesMemberSize * int64(len(o.AsCommentOptions().Beneficiaries))
	case op.TagConvert:
		return rcConvertSize
	case op.TagCollateralizedConvert:
		return rcCollateralizedConvertSize
	case op.TagDeclineVotingRights:
		if o.AsDeclineVotingRights().Decline {
			return rcDeclineVotingRightsSize
		}
		return 0
	case op.TagDelegateVestingShares:
		if o.AsDelegateVestingShares().VestingShares.Amount > 0 {
			// delegate_vesting_shares_size = max(object, expiration object)
			if rcVestingDelegationObjectSize > rcVestingDelegationExpirationObjectSize {
				return rcVestingDelegationObjectSize
			}
			return rcVestingDelegationExpirationObjectSize
		}
		return rcVestingDelegationExpirationObjectSize
	case op.TagEscrowTransfer:
		return rcEscrowTransferSize
	case op.TagLimitOrderCreate:
		if o.AsLimitOrderCreate().FillOrKill {
			return 0
		}
		return rcLimitOrderCreateSize
	case op.TagLimitOrderCreate2:
		if o.AsLimitOrderCreate2().FillOrKill {
			return 0
		}
		return rcLimitOrderCreateSize
	case op.TagRequestAccountRecovery:
		if o.AsRequestAccountRecovery().NewOwnerAuthority.WeightThreshold != 0 {
			return rcRequestAccountRecoverySize
		}
		return 0
	case op.TagSetWithdrawVestingRoute:
		if o.AsSetWithdrawVestingRoute().Percent != 0 {
			return rcSetWithdrawVestingRouteSize
		}
		return 0
	case op.TagVote:
		return rcVoteSize
	case op.TagWitnessUpdate:
		return rcWitnessUpdateBaseSize + rcWitnessUpdateURLCharSize*int64(len(o.AsWitnessUpdate().URL))
	case op.TagTransferToVesting:
		return rcTransferToVestingSize
	case op.TagTransferFromSavings:
		return rcTransferFromSavingsSize
	case op.TagChangeRecoveryAccount:
		return rcChangeRecoveryAccountSize
	case op.TagCreateProposal:
		cp := o.AsCreateProposal()
		// FC_ASSERT( op.end_date > _now ); lifetime rounded up to whole hours
		// from NOW (not from start_date). The evaluator rejects end_date <= now
		// before any charge, so 0 here is unreachable in practice.
		if cp.EndDate <= now {
			return 0
		}
		lifetime := (int64(cp.EndDate) - int64(now) + 3600 - 1) / 3600
		size := rcCreateProposalBaseSize +
			rcCreateProposalSubjectPermlinkCharSize*int64(len(cp.Subject)+len(cp.Permlink))
		return size * lifetime
	case op.TagUpdateProposalVotes:
		uv := o.AsUpdateProposalVotes()
		if uv.Approve {
			return rcUpdateProposalVotesMemberSize * int64(len(uv.ProposalIDs))
		}
		return 0
	case op.TagRecurrentTransfer:
		rt := o.AsRecurrentTransfer()
		if rt.Amount.Amount > 0 {
			lifetime := int64(rt.Recurrence) * int64(rt.Executions) // hours
			return (rcRecurrentTransferBaseSize + rcRecurrentTransferMemoCharSize*int64(len(rt.Memo))) * lifetime
		}
		return 0
	case op.TagRecoverAccount:
		// Charged even when the subsidy applies; the subsidy zeroes the whole
		// transaction afterwards (resource_count.cpp:534-546).
		return authorityDynamicSize(&o.AsRecoverAccount().NewOwnerAuthority) + rcOwnerAuthorityHistoryObjectSize
	case op.TagCustomJSON:
		// delegate_rc rides in custom_json id "rc"; hived adds its state usage
		// in handle_custom_op_usage (resource_count.cpp:548-553) once the
		// payload was interpreted.
		cj := o.AsCustomJSON()
		if cj.ID == "rc" {
			// Per interpreted operation, as handle_custom_op_usage runs from
			// each delegate_rc's post-apply notification.
			ops, err := evaluator.ParseRCCustomJSON(cj.JSON, false)
			if err != nil {
				return 0
			}
			var usage int64
			for i := range ops {
				if ops[i].MaxRC != 0 {
					usage += rcDelegateRCBaseSize * int64(len(ops[i].Delegatees))
				}
			}
			return usage
		}
		return 0
	}
	return 0
}

// authorityDynamicSize is get_authority_dynamic_size (resource_count.cpp):
// per-member cost of the account_auths and key_auths maps.
func authorityDynamicSize(a *auth.Authority) int64 {
	return rcAuthorityAccountMemberSize*int64(len(a.AccountAuths)) +
		rcAuthorityKeyMemberSize*int64(len(a.KeyAuths))
}

// extraExecTimeForOp is the execution time hived adds outside the per-op
// table: recurrent_transfer_processing_time × executions
// (resource_count.cpp:524-525) and delegate_rc_time for an interpreted
// delegate_rc custom_json (rc_utility.cpp:121 → resource_count.cpp:672-678).
func extraExecTimeForOp(o op.Op) int64 {
	switch o.Tag {
	case op.TagRecurrentTransfer:
		return rcRecurrentTransferProcessingTimeNS * int64(o.AsRecurrentTransfer().Executions)
	case op.TagCustomJSON:
		cj := o.AsCustomJSON()
		if cj.ID == "rc" {
			if ops, err := evaluator.ParseRCCustomJSON(cj.JSON, false); err == nil {
				return rcDelegateRCTimeNS * int64(len(ops))
			}
		}
	}
	return 0
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
		cost += stateBytesForOp(o, 0) // approximation helper: no head time available
		if t, ok := execTimeNS[o.Tag]; ok {
			cost += t
		}
	}
	return cost
}

// PayerOf returns the account that pays RC for ONE operation, or "" when
// the operation names no resource user. Mirrors get_resource_user_visitor
// (rc/resource_user.cpp:10-47): the first required ACTIVE authority, else
// the first required OWNER authority, else the first required POSTING
// authority (each a sorted flat_set, so "first" is the lexically smallest
// name), with three explicit overrides:
//
//	witness_set_properties → owner
//	recover_account        → account_to_recover
//	witness_block_approve  → witness
//
// Every real operation here has at most one account per level, so the
// canonical "from / account / owner / voter / creator" field IS the first
// element. custom_json is the exception: hived checks required_auths
// (active) BEFORE required_posting_auths. Use ResourceUserOf for a whole
// transaction: hived walks every operation until one yields a payer
// (resource_user.cpp:54-62), it does not stop at operations[0].
func PayerOf(o op.Op) string {
	switch o.Tag {
	case op.TagTransfer:
		return string(o.AsTransfer().From)
	case op.TagTransferToVesting:
		return string(o.AsTransferToVesting().From)
	case op.TagWithdrawVesting:
		return string(o.AsWithdrawVesting().Account)
	case op.TagVote:
		return string(o.AsVote().Voter)
	case op.TagComment:
		return string(o.AsComment().Author)
	case op.TagLimitOrderCreate:
		return string(o.AsLimitOrderCreate().Owner)
	case op.TagLimitOrderCreate2:
		return string(o.AsLimitOrderCreate2().Owner)
	case op.TagLimitOrderCancel:
		return string(o.AsLimitOrderCancel().Owner)
	case op.TagFeedPublish:
		return string(o.AsFeedPublish().Publisher)
	case op.TagConvert:
		return string(o.AsConvert().Owner)
	case op.TagAccountCreate:
		return string(o.AsAccountCreate().Creator)
	case op.TagAccountCreateWithDelegation:
		return string(o.AsAccountCreateWithDelegation().Creator)
	case op.TagAccountUpdate:
		return string(o.AsAccountUpdate().Account)
	case op.TagWitnessUpdate:
		return string(o.AsWitnessUpdate().Owner)
	case op.TagAccountWitnessVote:
		return string(o.AsAccountWitnessVote().Account)
	case op.TagAccountWitnessProxy:
		return string(o.AsAccountWitnessProxy().Account)
	case op.TagDeleteComment:
		return string(o.AsDeleteComment().Author)
	case op.TagCommentOptions:
		return string(o.AsCommentOptions().Author)
	case op.TagSetWithdrawVestingRoute:
		return string(o.AsSetWithdrawVestingRoute().FromAccount)
	case op.TagClaimAccount:
		return string(o.AsClaimAccount().Creator)
	case op.TagCreateClaimedAccount:
		return string(o.AsCreateClaimedAccount().Creator)
	case op.TagDelegateVestingShares:
		return string(o.AsDelegateVestingShares().Delegator)
	case op.TagTransferToSavings:
		return string(o.AsTransferToSavings().From)
	case op.TagTransferFromSavings:
		return string(o.AsTransferFromSavings().From)
	case op.TagCancelTransferFromSavings:
		return string(o.AsCancelTransferFromSavings().From)
	case op.TagRecurrentTransfer:
		return string(o.AsRecurrentTransfer().From)
	case op.TagClaimRewardBalance:
		return string(o.AsClaimRewardBalance().Account)
	case op.TagDeclineVotingRights:
		return string(o.AsDeclineVotingRights().Account)
	case op.TagCreateProposal:
		return string(o.AsCreateProposal().Creator)
	case op.TagUpdateProposalVotes:
		return string(o.AsUpdateProposalVotes().Voter)
	case op.TagRemoveProposal:
		return string(o.AsRemoveProposal().ProposalOwner)
	case op.TagUpdateProposal:
		return string(o.AsUpdateProposal().Creator)
	case op.TagCustomJSON:
		// active before posting, like the generic visitor.
		if len(o.AsCustomJSON().RequiredAuths) > 0 {
			return string(o.AsCustomJSON().RequiredAuths[0])
		}
		if len(o.AsCustomJSON().RequiredPostingAuths) > 0 {
			return string(o.AsCustomJSON().RequiredPostingAuths[0])
		}
	case op.TagCustom:
		if len(o.AsCustom().RequiredAuths) > 0 {
			return string(o.AsCustom().RequiredAuths[0])
		}
	case op.TagCustomBinary:
		cb := o.AsCustomBinary()
		if len(cb.RequiredActiveAuths) > 0 {
			return string(cb.RequiredActiveAuths[0])
		}
		if len(cb.RequiredOwnerAuths) > 0 {
			return string(cb.RequiredOwnerAuths[0])
		}
		if len(cb.RequiredPostingAuths) > 0 {
			return string(cb.RequiredPostingAuths[0])
		}
	case op.TagAccountUpdate2:
		// owner / active / posting — whichever tier the op requires, the
		// account is the same.
		return string(o.AsAccountUpdate2().Account)
	case op.TagWitnessSetProperties:
		return string(o.AsWitnessSetProperties().Owner)
	case op.TagWitnessBlockApprove:
		return string(o.AsWitnessBlockApprove().Witness)
	case op.TagCollateralizedConvert:
		return string(o.AsCollateralizedConvert().Owner)
	case op.TagRequestAccountRecovery:
		return string(o.AsRequestAccountRecovery().RecoveryAccount)
	case op.TagEscrowTransfer:
		return string(o.AsEscrowTransfer().From)
	case op.TagEscrowApprove:
		return string(o.AsEscrowApprove().Who)
	case op.TagEscrowDispute:
		return string(o.AsEscrowDispute().Who)
	case op.TagEscrowRelease:
		return string(o.AsEscrowRelease().Who)
	case op.TagRecoverAccount:
		return string(o.AsRecoverAccount().AccountToRecover)
	case op.TagChangeRecoveryAccount:
		return string(o.AsChangeRecoveryAccount().AccountToRecover)
	}
	return ""
}
