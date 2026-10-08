package objects

import (
	"testing"

	"gopherd/internal/state"
)

// allTableIDs lists every K/V table the objects package defines. Add new
// tables here: TestTableIDsUnique is the only thing standing between a copied
// `state.TableID = N` and two tables sharing one key space.
var allTableIDs = map[string]state.TableID{
	"dgpo":                          DynamicGlobalPropertyTable,
	"account":                       AccountTable,
	"witness":                       WitnessTable,
	"witness_vote":                  WitnessVoteTable,
	"comment":                       CommentTable,
	"comment_vote":                  CommentVoteTable,
	"limit_order":                   LimitOrderTable,
	"savings_withdraw":              SavingsWithdrawTable,
	"vesting_delegation":            VestingDelegationTable,
	"withdraw_route":                WithdrawRouteTable,
	"escrow":                        EscrowTable,
	"convert_request":               ConvertRequestTable,
	"collateralized_convert":        CollateralizedConvertTable,
	"recurrent_transfer":            RecurrentTransferTable,
	"account_recovery_request":      AccountRecoveryRequestTable,
	"change_recovery_account":       ChangeRecoveryAccountRequestTable,
	"proposal":                      ProposalTable,
	"proposal_vote":                 ProposalVoteTable,
	"vesting_withdrawal_schedule":   VestingWithdrawalScheduleTable,
	"comment_cashout_schedule":      CommentCashoutScheduleTable,
	"feed_history":                  FeedHistoryTable,
	"witness_schedule":              WitnessScheduleTable,
	"vesting_delegation_expiration": VestingDelegationExpirationTable,
	"decline_voting_rights":         DeclineVotingRightsRequestTable,
	"hardfork_property":             HardforkPropertyTable,
	"block_summary":                 BlockSummaryTable,
	"transaction":                   TransactionTable,
	"account_authority":             AccountAuthorityTable,
	"rc_pool":                       RCPoolTable,
	"rc_delegation_expiration":      RCDelegationExpirationTable,
	"rc_resource_param":             RCResourceParamTable,
	"rc_stats":                      RCStatsTable,
	"rc_expired_delegation":         RCExpiredDelegationTable,
	"rc_usage_bucket":               RCUsageBucketTable,
	"rc_pending_data":               RCPendingDataTable,
	"h23_balance":                   H23BalanceTable,
	"owner_authority_history":       OwnerAuthorityHistoryTable,
	"rc_direct_delegation":          RCDirectDelegationTable,
	"liquidity_reward_balance":      LiquidityRewardBalanceTable,
	"reward_fund":                   RewardFundTable,
}

// TestTableIDsUnique pins the fix for three id collisions found by diffing a
// replayed mainnet state against hived: owner_authority_history and
// vesting_delegation_expiration both sat at 22 (415 owner-key history rows
// surfaced as delegation returns), rc_direct_delegation shared 23 with
// decline_voting_rights, and reward_fund / liquidity_reward_balance shared 21
// with witness_schedule. Any prefix Range over one table then visits the
// other's rows.
func TestTableIDsUnique(t *testing.T) {
	seen := map[state.TableID]string{}
	for name, id := range allTableIDs {
		if other, dup := seen[id]; dup {
			t.Errorf("table id %d used by both %s and %s", id, name, other)
		}
		seen[id] = name
	}
}

// TestOwnerHistoryRangeIgnoresDelegationReturns is the user-visible symptom:
// a pending delegation return for alice must not read as owner-key history
// (recover_account walks that history to match the previous owner authority).
func TestOwnerHistoryRangeIgnoresDelegationReturns(t *testing.T) {
	s := state.NewSession(state.OpenMem())
	PutVestingDelegationExpiration(s, &VestingDelegationExpiration{
		ID: 7, Delegator: "alice", VestingShares: 1000, Expiration: 1_500_000_000,
	})
	if HasOwnerAuthorityHistoryFor(s, "alice") {
		t.Fatal("a vesting_delegation_expiration row was visible through the owner_authority_history table")
	}
	n := 0
	_ = s.Range(VestingDelegationExpirationTable, state.PrimaryIndex, nil, func(_, _ []byte) (bool, error) {
		n++
		return true, nil
	})
	if n != 1 {
		t.Fatalf("vesting_delegation_expiration primary rows = %d, want 1", n)
	}
}
