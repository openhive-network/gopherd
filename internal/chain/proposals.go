package chain

import (
	"sort"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_PROPOSAL_MAINTENANCE_PERIOD — 1 hour. process_proposals only
// runs at this cadence; in between blocks we accumulate dhf_interval_ledger.
const proposalMaintenancePeriodSec uint32 = 60 * 60

// HIVE_DAILY_PROPOSAL_MAINTENANCE_PERIOD — 24 hours. The HIVE→HBD
// conversion in the treasury fires on this cadence (HF24+ only).
// Mirrors libraries/protocol/include/hive/protocol/config.hpp:128.
const dailyMaintenancePeriodSec uint32 = 24 * 60 * 60

// HIVE_PROPOSAL_CONVERSION_RATE — 5 basis points (0.05%) of the
// treasury's HIVE balance is converted to HBD every daily tick (HF24+).
// HIVE_100_PERCENT = 10000; HIVE_1_BASIS_POINT = 1 → conversion rate is
// 5/10000. Mirrors config.hpp:440.
const proposalConversionRateBP int64 = 5
const proposal100PercentBP int64 = 10000

// HIVE_MIN_PAYOUT_HBD — 0.020 HBD (20 satoshi). Conversions resulting
// in less than this amount are skipped (sps_processor::convert_funds).
const proposalMinPayoutHbdSats int64 = 20

// totalAmountDivider — proposal daily budget = treasury / this. Matches
// hived's dhf_processor::total_amount_divider = 100, i.e. treasury can
// pay out at most 1%/day to proposals collectively.
const proposalTotalAmountDivider int64 = 100

// currentTreasuryName returns the canonical treasury account name at the
// given hardfork set. HF24+ = "hive.fund"; pre-HF24 = "steem.dao".
// Mirrors hived's database::get_treasury_name (database.cpp).
func currentTreasuryName(s *state.Session, hf hardfork.Set) string {
	if hf.Has(hardfork.HF24) {
		return "hive.fund"
	}
	return "steem.dao"
}

// dailySeconds = 86400. Used to scale daily_pay → period_pay
// (period_pay = daily_pay × elapsed / day).
const dailySecondsConst int64 = 24 * 60 * 60

// processProposals mirrors database::process_proposals
// (libraries/chain/database.cpp:1645) → dhf_processor::run.
//
// Two cadences run here:
//
//  1. Hourly maintenance — process_proposals payout loop
//     (sps_processor::make_payments).
//  2. Daily maintenance — HIVE→HBD treasury conversion
//     (sps_processor::convert_funds, HF24+ only).
//
// Pre-HF21 this hook is a no-op.
func processProposals(s *state.Session, blockTime uint32, blockNum uint32) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF21) {
		return
	}
	// Daily HIVE→HBD treasury conversion (HF24+) runs first; matches
	// the C++ order: convert_funds() → make_payments() inside
	// sps_processor::run().
	convertProposalFunds(s, blockTime, hf)
	if blockTime%proposalMaintenancePeriodSec != 0 {
		return
	}

	// Snapshot ACTIVE proposals (start<=now<=end) for the vote recalc.
	// hived does NOT auto-remove expired proposals: dhf_processor::
	// remove_proposals (util/dhf_processor.cpp:53-68) only deletes
	// proposals already flagged `removed` by an explicit
	// remove_proposal_operation — "automatic removing of old proposals is
	// to be blocked in order to allow looking for expired proposals (by
	// API call list_proposals with expired flag)". Expired proposals linger
	// in state forever. Go previously auto-deleted every proposal past its
	// end_date here, which orphaned later unapprove votes: an
	// update_proposal_votes(approve=false) on the expired-but-still-alive
	// proposal 16 hit "proposal not found" at block 36,196,594.
	var active []objects.Proposal

	_ = s.Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err != nil {
				return true, nil
			}
			if p.EndDate <= blockTime {
				return true, nil // expired — excluded from vote calc, NOT removed
			}
			if p.StartDate <= blockTime {
				active = append(active, p)
			}
			return true, nil
		})

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

	// Maintenance budget per hived dhf_processor::calculate_maintenance_budget
	// (dhf_processor.cpp:145-159):
	//
	//	passed_time_seconds = head_time - dgpo.last_budget_time
	//	daily_budget_limit  = treasury.hbd / total_amount_divider
	//	daily_budget_limit  = passed_time_seconds * daily_budget_limit / 86400
	//
	// last_budget_time is set by set_next_maintenance_time(head_time, …) at
	// every maintenance tick. The first time hived enters this code path
	// (HF21 activation) last_budget_time has the genesis default — so on
	// first tick passed_time_seconds is potentially years. To match,
	// gopherd updates LastBudgetTime at HF21 activation (hardforks.go HF21
	// case) so the first DHF tick computes a one-hour passed_time.
	//
	// Fallback: if LastBudgetTime is 0 (never seeded — e.g., snapshot
	// that predates the field), treat the period as the canonical
	// HIVE_PROPOSAL_MAINTENANCE_PERIOD (3600s) to avoid spending the
	// entire treasury in one tick.
	var passedTime int64
	if dgpo, derr := objects.GetDGPO(s); derr == nil && dgpo.LastBudgetTime > 0 && blockTime > dgpo.LastBudgetTime {
		passedTime = int64(blockTime - dgpo.LastBudgetTime)
	} else {
		passedTime = int64(proposalMaintenancePeriodSec)
	}
	// Sanity clamp: a single tick budget shouldn't exceed a full day
	// (catches stale LastBudgetTime after a long node downtime).
	if passedTime > dailySecondsConst {
		passedTime = dailySecondsConst
	}
	dailyBudget := treasury.HBDBalance / proposalTotalAmountDivider
	budget := dailyBudget * passedTime / dailySecondsConst
	if budget <= 0 {
		// Even when budget == 0 we still need to refresh LastBudgetTime
		// so the next tick computes correctly.
		objects.MutateDGPO(s, func(d *objects.DynamicGlobalProperty) {
			d.LastBudgetTime = blockTime
		})
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
		// Per-proposal payment formula differs pre/post HF25
		// (dhf_processor.cpp:191-199):
		//   HF25+:  period_pay = passed_time * daily_pay / 86400
		//   pre-HF25: ratio = (passed_time * HIVE_100_PERCENT) / 86400
		//            period_pay = (ratio * daily_pay) / HIVE_100_PERCENT
		// The pre-HF25 form retains precision via the 100% intermediate.
		var periodPay int64
		if hf.Has(hardfork.HF25) {
			periodPay = passedTime * p.DailyPay / dailySecondsConst
		} else {
			const hivePct = int64(10000)
			ratio := (passedTime * hivePct) / dailySecondsConst
			periodPay = (ratio * p.DailyPay) / hivePct
		}
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
	// Bump LastBudgetTime so the next tick's passed_time is correct.
	// Mirrors dhf_processor::update_settings (dhf_processor.cpp:214-222).
	objects.MutateDGPO(s, func(d *objects.DynamicGlobalProperty) {
		d.LastBudgetTime = blockTime
	})
}

// convertProposalFunds mirrors sps_processor::convert_funds — once per
// HIVE_DAILY_PROPOSAL_MAINTENANCE_PERIOD (24h) post-HF24 it converts
// HIVE_PROPOSAL_CONVERSION_RATE (5 bps) of the treasury's HIVE balance
// into HBD at the current median feed.
//
// On first hit, DGPO.NextDailyMaintenanceTime is 0 — we initialize it
// to blockTime + 1 day and DON'T convert (matches the C++ behavior
// where dgpo.next_daily_maintenance_time starts at genesis_time + 1d
// from feed_history bootstrap). On subsequent ticks, we advance by
// 1 day and convert.
func convertProposalFunds(s *state.Session, blockTime uint32, hf hardfork.Set) {
	if !hf.Has(hardfork.HF24) {
		return
	}
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	// Mirror C++ database_dhf.cpp / dhf_processor: next_daily_maintenance_time
	// is seeded to HIVE_GENESIS_TIME (= 0 in unix terms) so the first
	// HF24 block satisfies `next <= head_time` → conversion fires
	// IMMEDIATELY at HF24 activation. Gopherd's prior bootstrap pattern
	// (set next = block + 86400 and return) skipped that first
	// conversion entirely — the activation block ended with treasury
	// HIVE not yet converted to HBD.
	if dgpo.NextDailyMaintenanceTime > blockTime {
		return
	}
	// Advance the schedule first (matches C++'s "shift from current
	// time" pattern in sps_processor::convert_funds).
	dgpo.NextDailyMaintenanceTime = blockTime + dailyMaintenancePeriodSec

	// Resolve treasury account.
	treasuryName := "hive.fund"
	treasury, terr := objects.GetAccount(s, treasuryName)
	if terr != nil {
		objects.PutDGPO(s, &dgpo)
		return
	}
	if treasury.Balance <= 0 {
		objects.PutDGPO(s, &dgpo)
		return
	}
	// to_convert = HIVE_PROPOSAL_CONVERSION_RATE × balance / HIVE_100_PERCENT.
	toConvert := proposalConversionRateBP * treasury.Balance / proposal100PercentBP
	if toConvert <= 0 {
		objects.PutDGPO(s, &dgpo)
		return
	}
	// Apply the median feed (HBD/HIVE).
	fh, ferr := objects.GetFeedHistory(s)
	if ferr != nil || priceIsZero(fh.CurrentMedianHistory) {
		objects.PutDGPO(s, &dgpo)
		return
	}
	convertedHbd := convertHiveToHBD(toConvert, fh.CurrentMedianHistory)
	if convertedHbd < proposalMinPayoutHbdSats {
		objects.PutDGPO(s, &dgpo)
		return
	}
	// Apply to treasury: -HIVE +HBD.
	treasury.Balance -= toConvert
	objects.AdjustHBDBalance(s, &treasury, blockTime, convertedHbd)
	objects.PutAccount(s, &treasury)
	// Adjust supplies: HIVE goes down, HBD goes up. adjustSupplyHbd
	// performs the virtual_supply recompute internally.
	adjustSupplyHive(s, &dgpo, -toConvert)
	adjustSupplyHbd(s, &dgpo, convertedHbd)
	objects.PutDGPO(s, &dgpo)
	_ = asset.HiveSymbol // keep asset import in case future expansion needs it
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
			// Mirror C++ account_object::get_governance_vote_power
			// (account_object.hpp:329-333): direct = vesting_shares -
			// sum_delayed_votes; total = direct + sum(proxied_vsf_votes).
			// Omitting the proxied component systematically under-counts
			// proxy-receiving witnesses' proposal vote weights, which
			// shifts the desc-by-votes ordering and the budget-exhaustion
			// break point.
			power := a.VestingShares - int64(a.SumDelayedVotes)
			if power < 0 {
				power = 0
			}
			power += objects.SumProxiedVotes(&a)
			if power > 0 {
				total += power
			}
			return true, nil
		})
	return total
}
