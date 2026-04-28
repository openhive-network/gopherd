package evaluator

// stubs.go registers a minimal "validate-only" evaluator for every
// regular op tag that doesn't yet have a state-mutating implementation.
// Stubs perform basic shape validation (positive amounts, supported
// asset symbols, etc.) and return nil, leaving state untouched.
//
// IMPORTANT: a stub-applied block produces consensus-INCORRECT state.
// The point of registering stubs is to let the apply pipeline run end-
// to-end so we can drive the user's block_log through the full
// dispatch path and shake out coverage holes / parse issues at scale.
// Each stub here is a P1 follow-up to replace with real semantics
// alongside the state objects it needs (comment_object, witness_object,
// limit_order_object, escrow_object, …).
//
// Stubs are grouped by the state object family they're waiting on.

import (
	"fmt"

	"hived-go/internal/protocol/op"
)

// stub returns an evaluator that runs validate (a closure) and otherwise
// returns nil — leaving state unchanged.
func stub(name string, validate func(op.Operation) error) Evaluator {
	return EvaluatorFunc(func(ctx *Context, o op.Operation) error {
		if validate != nil {
			if err := validate(o); err != nil {
				return fmt.Errorf("%s (stub): %w", name, err)
			}
		}
		return nil
	})
}

// noValidate is the default validate-pass closure for stubs that have no
// shape rules to enforce yet.
func noValidate(_ op.Operation) error { return nil }

func init() {
	// (vote, comment, delete_comment, comment_options are implemented in
	// comment.go.)

	// (witness_update, witness_set_properties, account_witness_vote, and
	// account_witness_proxy are implemented in witness.go.)
	register(op.TagWitnessBlockApprove, stub("witness_block_approve", noValidate))
	register(op.TagDeclineVotingRights, stub("decline_voting_rights", noValidate))

	// (pow/pow2 are implemented in pow.go — they create the worker account
	// so subsequent transfers don't fail; reward payout and witness object
	// updates land later.)

	// Custom data ops — by themselves they don't mutate state; downstream
	// custom-operation interpreters do (we don't run those yet).
	register(op.TagCustom, stub("custom", noValidate))
	register(op.TagCustomJSON, stub("custom_json", noValidate))
	register(op.TagCustomBinary, stub("custom_binary", noValidate))

	// Market (need limit_order_object).
	register(op.TagLimitOrderCreate, stub("limit_order_create", noValidate))
	register(op.TagLimitOrderCreate2, stub("limit_order_create2", noValidate))
	register(op.TagLimitOrderCancel, stub("limit_order_cancel", noValidate))

	// Feed (need feed_history_object + price tracking).
	register(op.TagFeedPublish, stub("feed_publish", noValidate))

	// Convert (need convert_request_object).
	register(op.TagConvert, stub("convert", noValidate))
	register(op.TagCollateralizedConvert, stub("collateralized_convert", noValidate))

	// Escrow (need escrow_object).
	register(op.TagEscrowTransfer, stub("escrow_transfer", noValidate))
	register(op.TagEscrowApprove, stub("escrow_approve", noValidate))
	register(op.TagEscrowDispute, stub("escrow_dispute", noValidate))
	register(op.TagEscrowRelease, stub("escrow_release", noValidate))

	// Savings (need savings_withdraw_object).
	register(op.TagTransferToSavings, stub("transfer_to_savings", noValidate))
	register(op.TagTransferFromSavings, stub("transfer_from_savings", noValidate))
	register(op.TagCancelTransferFromSavings, stub("cancel_transfer_from_savings", noValidate))

	// Vesting (need vesting_withdraw schedule + vesting_delegation_object).
	register(op.TagWithdrawVesting, stub("withdraw_vesting", noValidate))
	register(op.TagSetWithdrawVestingRoute, stub("set_withdraw_vesting_route", noValidate))
	register(op.TagDelegateVestingShares, stub("delegate_vesting_shares", noValidate))

	// Recovery (need account_recovery_request_object + history).
	register(op.TagRequestAccountRecovery, stub("request_account_recovery", noValidate))
	register(op.TagRecoverAccount, stub("recover_account", noValidate))
	register(op.TagChangeRecoveryAccount, stub("change_recovery_account", noValidate))
	register(op.TagResetAccount, stub("reset_account", noValidate))
	register(op.TagSetResetAccount, stub("set_reset_account", noValidate))

	// Account creation variants (need pending_claimed_account counter +
	// authority objects).
	register(op.TagClaimAccount, stub("claim_account", noValidate))
	register(op.TagCreateClaimedAccount, stub("create_claimed_account", noValidate))
	register(op.TagAccountCreateWithDelegation, stub("account_create_with_delegation", noValidate))

	// DHF / proposals (post-HF24, need proposal_object + proposal_vote_object).
	register(op.TagCreateProposal, stub("create_proposal", noValidate))
	register(op.TagUpdateProposalVotes, stub("update_proposal_votes", noValidate))
	register(op.TagRemoveProposal, stub("remove_proposal", noValidate))
	register(op.TagUpdateProposal, stub("update_proposal", noValidate))

	// Recurrent transfers (post-HF26, need recurrent_transfer_object).
	register(op.TagRecurrentTransfer, stub("recurrent_transfer", noValidate))
}
