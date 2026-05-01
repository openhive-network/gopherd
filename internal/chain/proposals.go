package chain

import (
	"sort"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_PROPOSAL_MAINTENANCE_PERIOD — 1 hour. process_proposals only
// runs at this cadence; in between blocks we accumulate dhf_interval_ledger.
const proposalMaintenancePeriodSec uint32 = 60 * 60

// totalAmountDivider — proposal daily budget = treasury / this. Matches
// hived's dhf_processor::total_amount_divider = 100, i.e. treasury can
// pay out at most 1%/day to proposals collectively.
const proposalTotalAmountDivider int64 = 100

// dailySeconds = 86400. Used to scale daily_pay → period_pay
// (period_pay = daily_pay × elapsed / day).
const dailySecondsConst int64 = 24 * 60 * 60

// processProposals mirrors database::process_proposals
// (libraries/chain/database.cpp:1645) → dhf_processor::run.
//
// Once per HIVE_PROPOSAL_MAINTENANCE_PERIOD (1h):
//
//  1. Walk all proposals; remove any whose `Removed` flag is set
//     (set by remove_proposal_operation).
//  2. Find active proposals (start_date ≤ head_time ≤ end_date).
//  3. Recalculate total_votes for each — sum of voter
//     governance_vote_power = vesting_shares - sum_delayed_votes
//     (post-HF24) for non-proxied voters.
//  4. Sort active proposals by total_votes desc (id asc tiebreak).
//  5. Compute the per-period budget:
//        budget = treasury_hbd / 100 × elapsed / day
//     where elapsed = head_time - last_budget_time (capped at the
//     maintenance period for our simpler model).
//  6. Pay each proposal in rank order:
//        pay = daily_pay × elapsed / day, capped by remaining budget.
//     Skip proposals with total_votes == 0.
//
// Pre-HF21 this hook is a no-op.
func processProposals(s *state.Session, blockTime uint32, blockNum uint32) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF21) {
		return
	}
	if blockTime%proposalMaintenancePeriodSec != 0 {
		return
	}

	// Snapshot proposals into in-memory lists.
	var active []objects.Proposal
	var expired []objects.Proposal

	_ = s.Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err != nil {
				return true, nil
			}
			if p.EndDate <= blockTime {
				expired = append(expired, p)
				return true, nil
			}
			if p.StartDate <= blockTime {
				active = append(active, p)
			}
			return true, nil
		})

	// Drop expired proposals (and their votes).
	for i := range expired {
		objects.DeleteProposalFull(s, &expired[i])
	}

	if len(active) == 0 {
		return
	}

	// Recalculate total_votes for each active proposal: sum of
	// governance_vote_power across non-proxied voters. Mirrors
	// dhf_processor::calculate_votes (dhf_processor.cpp:88).
	for i := range active {
		active[i].TotalVotes = recalculateProposalVotes(s, active[i].ID)
		objects.PutProposal(s, &active[i])
	}

	// Sort by votes desc, id asc tiebreak.
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].TotalVotes != active[j].TotalVotes {
			return active[i].TotalVotes > active[j].TotalVotes
		}
		return active[i].ID < active[j].ID
	})

	// Resolve treasury account (HF24+ = "hive.fund", pre-HF24 = "steem.dao").
	treasuryName := "hive.fund"
	treasury, err := objects.GetAccount(s, treasuryName)
	if err != nil {
		treasuryName = "steem.dao"
		treasury, err = objects.GetAccount(s, treasuryName)
		if err != nil {
			return
		}
	}

	// Maintenance budget = treasury × period / day / 100.
	periodSec := int64(proposalMaintenancePeriodSec)
	dailyBudget := treasury.HBDBalance / proposalTotalAmountDivider
	budget := dailyBudget * periodSec / dailySecondsConst
	if budget <= 0 {
		return
	}

	for _, p := range active {
		// Proposals with zero votes are not paid.
		if p.TotalVotes == 0 {
			continue
		}
		if budget <= 0 || treasury.HBDBalance <= 0 {
			break
		}
		periodPay := p.DailyPay * periodSec / dailySecondsConst
		if periodPay > budget {
			periodPay = budget
		}
		if periodPay > treasury.HBDBalance {
			periodPay = treasury.HBDBalance
		}
		if periodPay <= 0 {
			continue
		}
		recv, err := objects.GetAccount(s, p.Receiver)
		if err != nil {
			continue
		}
		// Treasury → receiver: HBD transfer. Use AdjustHBDBalance so
		// interest accrual happens correctly on both sides.
		objects.AdjustHBDBalance(s, &treasury, blockTime, -periodPay)
		objects.AdjustHBDBalance(s, &recv, blockTime, periodPay)
		objects.PutAccount(s, &recv)
		budget -= periodPay
	}
	objects.PutAccount(s, &treasury)
}

// recalculateProposalVotes walks every proposal_vote row for `pid` and
// sums each voter's governance vote power (vesting_shares minus
// sum_delayed_votes). Mirrors dhf_processor::calculate_votes(uint32_t).
func recalculateProposalVotes(s *state.Session, pid int64) int64 {
	var total int64
	prefix := state.AppendInt64BE(nil, pid)
	_ = s.Range(objects.ProposalVoteTable, state.PrimaryIndex, prefix,
		func(body, _ []byte) (bool, error) {
			if len(body) < 8+state.AccountNameKeyLen {
				return true, nil
			}
			voterBytes := body[8 : 8+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && voterBytes[n-1] == 0 {
				n--
			}
			voter := string(voterBytes[:n])
			a, err := objects.GetAccount(s, voter)
			if err != nil {
				return true, nil
			}
			// Proxied voters' votes don't count for the proposal — the
			// proxy target's votes already include the proxied weight.
			if a.Proxy != "" {
				return true, nil
			}
			power := a.VestingShares - int64(a.SumDelayedVotes)
			if power > 0 {
				total += power
			}
			return true, nil
		})
	return total
}
