package chain

import (
	"fmt"
	"os"
	"sort"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
	// dhf_processor::run() is remove_old_proposals → record_funding →
	// convert_funds → make_payments, and only the last two are gated on the
	// maintenance period: the removal sweep runs on EVERY block.
	removeOldProposals(s, blockTime)
	// Daily HIVE→HBD treasury conversion (HF24+) runs first; matches
	// the C++ order: convert_funds() → make_payments() inside
	// sps_processor::run().
	convertProposalFunds(s, blockTime, hf)
	// hived gates DHF maintenance on head_time >= next_maintenance_time
	// (dhf_processor::is_maintenance_period) and reschedules via
	// set_next_maintenance_time(head_time, HIVE_PROPOSAL_MAINTENANCE_PERIOD)
	// = head_time + 3600 — a SHIFTING schedule anchored to the last run, NOT
	// the absolute `blockTime % 3600` grid. The two diverge whenever a grid
	// block is missed (e.g. the irregular block timing after the HF21 stall
	// at block 35,922,615): the grid gate skips the missed tick then doubles
	// the next one against a larger (later) treasury, over-paying proposals
	// and over-draining the treasury (~152K HBD in 35.93M-36.04M → leaves
	// peak.proposals short at 36,351,128). next_maintenance_time defaults to
	// 0, so the first post-HF21 block is due (matches hived's genesis default).
	mdgpo, derr := objects.GetDGPO(s)
	if derr != nil {
		return
	}
	if blockTime < mdgpo.NextMaintenanceTime {
		return
	}
	objects.MutateDGPO(s, func(d *objects.DynamicGlobalProperty) {
		d.NextMaintenanceTime = blockTime + proposalMaintenancePeriodSec
		// hived's dhf_processor emits a dhf_funding_operation for the HBD
		// accumulated into the treasury since the last tick and resets
		// dhf_interval_ledger to 0 (sps_processor::run). The HBD itself
		// already lives in the treasury account + current_hbd_supply; the
		// ledger is only the per-interval reporting tally. Reset it here so
		// it mirrors hived's hourly cadence instead of growing unbounded.
		d.DHFIntervalLedger = 0
	})

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
			if p.EndDate < blockTime {
				return true, nil // expired — excluded from vote calc, NOT removed
			}
			// hived find_proposals (util/dhf_processor.cpp:73) treats a
			// proposal as ACTIVE while head_time <= end_date (INCLUSIVE of the
			// exact end_date second). Using `EndDate <= blockTime` here dropped
			// a proposal one maintenance tick early on the boundary, so its
			// receiver missed a period of pay (peak.proposals short 7900 HBD
			// at block 36,351,128).
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

	// Resolve treasury account by the HF24-gated name (pre-HF24 = "steem.dao",
	// HF24+ = "hive.fund"). MUST NOT probe "hive.fund" first: the account
	// "hive.fund" was registered by a user on 2020-03-18 (~block 41,759,371,
	// two days before the HF23 Hive fork) — years before HF24 renamed the
	// treasury to it. Probing "hive.fund" first made processProposals pay
	// proposals out of that tiny user account (~22,491 sat HBD) the moment it
	// was created, collapsing the maintenance budget to ~9 sat so every
	// proposal below rank 1 (including howo's #70 → 7.488 HBD/tick) went
	// unpaid — howo's HBD never replenished and its limit_order_create failed
	// at block 41,759,454. The real treasury (steem.dao, ~276.6M sat) stayed
	// full the whole time.
	treasuryName := currentTreasuryName(s, hf)
	treasury, err := objects.GetAccount(s, treasuryName)
	if err != nil {
		return
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
	dhfTrace := envGhivediffAll != "" && calcTraceWindow(blockNum)
	if dhfTrace {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF DHF-TICK block=%d treasury_hbd=%d dailyBudget=%d budget=%d passed=%d active=%d\n",
			blockNum, treasury.HBDBalance, dailyBudget, budget, passedTime, len(active))
	}
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
			if dhfTrace {
				fmt.Fprintf(os.Stderr, "GHIVEDIFF DHF-SKIP block=%d pid=%d recv=%s votes=0 zero-votes\n", blockNum, p.ID, p.Receiver)
			}
			continue
		}
		if budget <= 0 {
			if dhfTrace {
				fmt.Fprintf(os.Stderr, "GHIVEDIFF DHF-BREAK block=%d pid=%d recv=%s budget-exhausted\n", blockNum, p.ID, p.Receiver)
			}
			break
		}
		// Reload the treasury fresh for each payment. Holding a single
		// treasury copy across the whole loop and writing it back LAST
		// corrupted any payment whose receiver IS the treasury (the Hive
		// "return proposal", receiver = steem.dao / hive.fund, paid every
		// maintenance tick when it out-ranks other proposals): the stale
		// treasury copy clobbered the receiver-side credit on PutAccount,
		// and the freshly-loaded receiver re-accrued HBD interest a SECOND
		// time (its hbd_seconds_last_update was still pre-tick because the
		// treasury debit had not been persisted yet) — leaking that interest
		// into current_hbd_supply every tick. That was the +67,191 HBD
		// conservation break growing hourly from HF21 (block 35,921,786) on.
		// hived mutates a single live db object, so treasury→treasury nets
		// to zero with interest paid exactly once.
		treasury, terr := objects.GetAccount(s, treasuryName)
		if terr != nil || treasury.HBDBalance <= 0 {
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
		// Debit the treasury and persist it BEFORE loading the receiver, so
		// that a receiver which == treasury observes the debited balance and
		// the post-tick hbd_seconds_last_update — the net effect is zero and
		// interest is paid exactly once (matching hived's single-object
		// semantics).
		objects.AdjustHBDBalance(s, &treasury, blockTime, -periodPay)
		objects.PutAccount(s, &treasury)
		recv, err := objects.GetAccount(s, p.Receiver)
		if err != nil {
			continue
		}
		objects.AdjustHBDBalance(s, &recv, blockTime, periodPay)
		objects.PutAccount(s, &recv)
		budget -= periodPay
		if dhfTrace {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF DHF-PAY block=%d pid=%d recv=%s votes=%d dailyPay=%d periodPay=%d budgetLeft=%d\n",
				blockNum, p.ID, p.Receiver, p.TotalVotes, p.DailyPay, periodPay, budget)
		}
	}
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

// removeOldProposals mirrors dhf_processor::remove_proposals
// (libraries/chain/util/dhf_processor.cpp:44), which runs once per block.
//
// Expired proposals are deliberately NOT swept: they stay in state so
// list_proposals can still report them. The only thing this erases is what a
// remove_proposal_operation flagged after running out of its removal budget —
// normally nothing at all, occasionally the tail of a proposal whose vote
// count ran past HIVE_GLOBAL_REMOVE_THRESHOLD, spread over as many blocks as
// it takes at 200 objects each.
//
// hived reaches those rows by walking by_end_date over every proposal whose
// DELAYED end date has passed and skipping the ones that are not flagged.
// gopherd walks an index that holds only the flagged ones, which visits the
// same proposals in the same order at a fraction of the cost — the skipped
// entries are no-ops in hived too.
func removeOldProposals(s *state.Session, blockTime uint32) {
	// upper_bound(head_time) on get_end_date_with_delay(): a proposal is in
	// range once end_date + HIVE_PROPOSAL_MAINTENANCE_CLEANUP <= head_time.
	if blockTime < objects.ProposalMaintenanceCleanupSec {
		return
	}
	maxEndDate := blockTime - objects.ProposalMaintenanceCleanupSec

	// Collect before mutating: Range shares one pooled iterator per session.
	type flagged struct {
		endDate uint32
		id      int64
	}
	var due []flagged
	_ = s.Range(objects.ProposalTable, objects.ProposalRemovedIndex, nil,
		func(key, _ []byte) (bool, error) {
			endDate, id, ok := objects.ProposalIDFromEndDateKey(key)
			if !ok {
				return true, nil
			}
			if endDate > maxEndDate {
				return false, nil // index is end-date ordered; nothing later qualifies
			}
			due = append(due, flagged{endDate: endDate, id: id})
			return true, nil
		})
	if len(due) == 0 {
		return
	}

	guard := objects.NewRemoveGuard(network.GlobalRemoveThreshold)
	for _, f := range due {
		// hived checks the guard AFTER processing each proposal and breaks;
		// checking before the next one is the same sequence, and it keeps the
		// unlimited case (a negative budget is never Done) intact.
		if guard.Done() {
			break
		}
		p, err := objects.GetProposal(s, f.id)
		if err != nil {
			// The row is gone but the index entry survived. Nothing can ever
			// clear it through the normal path, so it would be rescanned every
			// block forever and hold the early exit open. Drop it here.
			objects.DeleteProposalRemovedIndexEntry(s, f.endDate, f.id)
			continue
		}
		objects.RemoveProposalWithVotes(s, &p, guard)
	}
}

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

	// Resolve treasury account by the HF24-gated name. convertProposalFunds
	// only runs at HF24+ (gated at the top of this function), where the
	// treasury is "hive.fund"; use currentTreasuryName for consistency rather
	// than hard-coding it.
	treasuryName := currentTreasuryName(s, hf)
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
