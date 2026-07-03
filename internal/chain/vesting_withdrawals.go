package chain

import (
	"fmt"
	"math/big"
	"math/bits"
	"os"
	"sort"
	"strings"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// envBalanceTraceWithdraw + balanceTraceMatchAccount gate the WITHDRAW
// trace emitted from processOneWithdrawal. Reuses the ACCOUNT_TRACE env
// list so callers only set one variable.
var (
	envBalanceTraceList2 = func() []string {
		v := os.Getenv("ACCOUNT_TRACE")
		if v == "" {
			return nil
		}
		out := strings.Split(v, ",")
		for i := range out {
			out[i] = strings.TrimSpace(out[i])
		}
		return out
	}()
	envBalanceTraceWithdraw = len(envBalanceTraceList2) > 0

	// WITHDRAW_TRACE_FROM/TO: dump EVERY fill_vesting_withdraw (and each
	// route processed) for blocks in [from,to], regardless of account.
	// Diagnostic for localizing pool drift in a narrow block window.
	wdTraceFrom = func() uint32 { v := os.Getenv("WITHDRAW_TRACE_FROM"); n := uint32(0); fmt.Sscan(v, &n); return n }()
	wdTraceTo   = func() uint32 { v := os.Getenv("WITHDRAW_TRACE_TO"); n := uint32(0); fmt.Sscan(v, &n); return n }()
)

func wdTraceOn(blk uint32) bool { return wdTraceTo != 0 && blk >= wdTraceFrom && blk <= wdTraceTo }

func balanceTraceMatchAccount(name string) bool {
	for _, n := range envBalanceTraceList2 {
		if n == name {
			return true
		}
	}
	return false
}

// HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS — 1 week between payouts at
// every hardfork. See evaluator/vesting.go for the C++ rationale; only
// the payout count differs across HF16 (104 vs 13).
const vestingWithdrawIntervalSec uint32 = 60 * 60 * 24 * 7

func vestingWithdrawIntervalForBlock(blockNum uint32) uint32 {
	return vestingWithdrawIntervalSec
}

// vestsToHive converts a VESTS-satoshi amount to HIVE-satoshi at the
// vesting fund's current dynamic price (fund / shares). Pre-genesis (or
// when either side is zero) it falls back to the inverse of the 1:1000
// initial CreateVesting ratio.
//
// hived's expression is `to_convert * cprops.get_vesting_share_price()`
// with VEST_price as `vesting_shares / vesting_fund_hive`; the reverse
// VESTS→HIVE multiply is therefore `vests * fund / shares`.
//
// The intermediate `vests * fund` overflows int64 well before reaching
// realistic chain state (vests ≈ 1e9, fund ≈ 1e10 already wraps). FC's
// asset.cpp uses a fc::uint128 for this; we mirror with math/big.
func vestsToHive(vests int64, dgpo *objects.DynamicGlobalProperty) int64 {
	if dgpo.TotalVestingFundHive == 0 || dgpo.TotalVestingShares == 0 {
		return vests / 1000
	}
	prod := new(big.Int).Mul(big.NewInt(vests), big.NewInt(dgpo.TotalVestingFundHive))
	prod.Quo(prod, big.NewInt(dgpo.TotalVestingShares))
	if !prod.IsInt64() {
		return 0
	}
	return prod.Int64()
}

// processVestingWithdrawals mirrors database::process_vesting_withdrawals
// (libraries/chain/database_vesting.cpp:111).
//
// Each block, every account whose next_vesting_withdrawal time has passed
// gets a chunk of its scheduled VESTS converted back to HIVE (or routed
// to other accounts at the configured percentages). The schedule advances
// by HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS until ToWithdraw is exhausted
// (or the account has run out of VESTS to back the schedule).
//
// We iterate the VestingWithdrawalSchedule secondary index in time order
// and stop at the first entry whose time is in the future. Each processed
// entry updates the account, the dgpo totals, and the schedule index.
//
// The full hived flow also generates a fill_vesting_withdraw virtual op
// (for history) and applies HF20+ RC mana regeneration / HF24+ delayed
// voting tracking; those land with the corresponding subsystems.
func processVestingWithdrawals(s *state.Session, blockTime uint32, hooks *blockHookScratch) {
	store := objects.EnsureVestingWithdrawalScheduleStore(s)
	// Short-circuit when the earliest scheduled withdrawal is in the
	// future. Killed the per-block Session.Range cost (~250 bytes/block,
	// ~1 GB cum) for the typical empty-queue case.
	if earliest, has := store.EarliestDue(); !has || earliest > blockTime {
		return
	}
	hooks.withdrawalPending = hooks.withdrawalPending[:0]
	ready := &hooks.withdrawalPending
	// Snapshot ready entries first to avoid invalidating the iterator
	// while we mutate the same table.
	store.RangeDueBefore(blockTime, func(t uint32, account auth.AccountName) bool {
		*ready = append(*ready, blockHookWithdrawalPending{nextWithdrawal: t, account: string(account)})
		return true
	})

	for _, p := range *ready {
		processOneWithdrawal(s, p.account, p.nextWithdrawal, blockTime)
	}
}

// processOneWithdrawal pays out the next chunk of an account's scheduled
// vesting withdrawal — convert min(rate, remaining) VESTS to HIVE, credit
// from_account.balance (and any routed accounts), update DGPO totals,
// advance or clear the schedule.
func processOneWithdrawal(s *state.Session, accountName string, scheduledTime, blockTime uint32) {
	objects.DeleteVestingWithdrawalSchedule(s, scheduledTime, accountName)

	a, err := objects.GetAccount(s, accountName)
	if err != nil {
		return
	}
	if a.VestingWithdrawRate == 0 || a.NextVestingWithdrawal == 0 {
		return
	}
	// Stale schedule-store entry guard. hived has NO separate withdrawal
	// schedule — process_vesting_withdrawals iterates accounts by the
	// by_next_vesting_withdrawal index, so an account is filled iff its
	// OWN next_vesting_withdrawal is due. Go keeps a separate
	// VestingWithdrawalScheduleStore as a fast candidate index; when an
	// account restarts/cancels a power-down (e.g. justyy @41,914,035) the
	// old entry can survive in that store while the account field is
	// rescheduled, causing an early fill that hived never performs (drains
	// the vesting pool ahead of hived → reward_price drift → create_vesting
	// floor flips → claim_reward_balance failures downstream, e.g. kimzwarch
	// @41,984,169). Treat the account field as authoritative: only fill when
	// the firing entry matches the account's current schedule.
	if a.NextVestingWithdrawal != scheduledTime {
		return
	}

	remaining := a.ToWithdraw - a.Withdrawn
	if remaining <= 0 {
		a.VestingWithdrawRate = 0
		a.NextVestingWithdrawal = 0
		a.ToWithdraw = 0
		a.Withdrawn = 0
		objects.PutAccount(s, &a)
		return
	}
	toWithdraw := a.VestingWithdrawRate
	if toWithdraw > remaining {
		toWithdraw = remaining
	}
	// Pre-HF28 final-chunk modulo quirk (database_vesting.cpp:176-177).
	// When remaining < rate, C++ replaces `remaining` with
	// `to_withdraw_total % rate`. For most accounts this collapses to
	// the same value, but for HF1-affected accounts whose rate changed
	// mid-power-down ("gil" is the canonical example) the residual
	// differs by 0.769230 VESTS vs 0.000040 VESTS — leaks/locks VESTS
	// on dgpo.TotalVestingShares which then propagates into witness
	// vote totals → schedule → median feed.
	dgpoHF, _ := objects.GetDGPO(s)
	hfWithdraw := hardfork.SetForBlock(dgpoHF.HeadBlockNumber)
	if !hfWithdraw.Has(hardfork.HF28) && toWithdraw < a.VestingWithdrawRate && a.VestingWithdrawRate > 0 {
		toWithdraw = a.ToWithdraw % a.VestingWithdrawRate
	}
	if toWithdraw > a.VestingShares {
		toWithdraw = a.VestingShares
	}
	if toWithdraw <= 0 {
		a.VestingWithdrawRate = 0
		a.NextVestingWithdrawal = 0
		a.ToWithdraw = 0
		a.Withdrawn = 0
		objects.PutAccount(s, &a)
		return
	}

	// Routes (set_withdraw_vesting_route). For each route, divert
	// (percent/HIVE_100_PERCENT) of toWithdraw — first as VESTS for
	// auto_vest=true entries (VESTS stay in the vesting pool), then as
	// HIVE for auto_vest=false entries (priced via the dynamic ratio).
	// The remainder is paid to the account itself in HIVE.
	// HF24+ auto_vest route deposits count toward the recipient's witness
	// votes only after delayed-vote maturation (database_vesting.cpp:213,
	// dv->add_votes → add_delayed_value). Accumulate per-recipient here;
	// flushed after the withdrawal removal so a self-route nets against the
	// withdrawal exactly like hived's votes_update_data set (whose entries
	// are keyed by account).
	var routeDelayedVotes map[string]int64
	if hfWithdraw.Has(hardfork.HF24) {
		routeDelayedVotes = map[string]int64{}
	}
	var depositedVests int64
	depositedVests += processWithdrawRoutes(s, accountName, toWithdraw, true, scheduledTime, routeDelayedVotes)
	depositedVests += processWithdrawRoutes(s, accountName, toWithdraw, false, scheduledTime, routeDelayedVotes)

	// Re-read account in case routing modified it (specifically, if a
	// route loops back to the source account or shares the dgpo).
	a, err = objects.GetAccount(s, accountName)
	if err != nil {
		return
	}

	dgpo, err := objects.GetDGPO(s)
	if err != nil && err != state.ErrNotFound {
		return
	}
	toConvertToHive := toWithdraw - depositedVests
	convertedHive := vestsToHive(toConvertToHive, &dgpo)
	if envBalanceTraceWithdraw && balanceTraceMatchAccount(accountName) {
		fmt.Fprintf(os.Stderr,
			"GHIVEDIFF WITHDRAW name=%s block=%d toWithdraw=%d depositedVests=%d toConvert=%d F=%d S=%d converted=%d acct_vests=%d acct_to_withdraw=%d acct_withdrawn=%d acct_rate=%d\n",
			accountName, dgpo.HeadBlockNumber, toWithdraw, depositedVests, toConvertToHive,
			dgpo.TotalVestingFundHive, dgpo.TotalVestingShares, convertedHive,
			a.VestingShares, a.ToWithdraw, a.Withdrawn, a.VestingWithdrawRate,
		)
	}

	if wdTraceOn(dgpo.HeadBlockNumber) {
		fmt.Fprintf(os.Stderr,
			"GHIVEDIFF WDFILL block=%d name=%s toWithdraw=%d depositedVests=%d toConvert=%d convertedHive=%d acct_vests=%d rate=%d to_withdraw=%d withdrawn=%d\n",
			dgpo.HeadBlockNumber, accountName, toWithdraw, depositedVests, toConvertToHive, convertedHive,
			a.VestingShares, a.VestingWithdrawRate, a.ToWithdraw, a.Withdrawn)
	}
	a.VestingShares -= toWithdraw
	a.Balance += convertedHive
	a.Withdrawn += toWithdraw
	// adjust_proxied_witness_votes for the withdrawn-out VESTS.
	//
	// Pre-HF24: full toWithdraw decrements witness votes immediately.
	// Post-HF24: erase from the back of DelayedVotes first (those vests
	// were never voting); only the leftover beyond SumDelayedVotes
	// decrements witness votes (those vests had matured + were
	// counting). Mirrors database_vesting.cpp:288.
	if toWithdraw > 0 {
		dgpoNow, _ := objects.GetDGPO(s)
		hfNow := hardfork.SetForBlock(dgpoNow.HeadBlockNumber)
		if hfNow.Has(hardfork.HF24) {
			// hived batches the from-account's -to_withdraw with any
			// self-route auto_vest deposit into ONE votes_update_data
			// entry (net <= 0 since routed_vests <= to_withdraw), then
			// update_votes erases it from the back of DelayedVotes (the
			// vests that just left / re-entered); only the leftover beyond
			// sum_delayed_votes hits witness totals immediately.
			netWithdraw := toWithdraw
			if routeDelayedVotes != nil {
				if selfDep, ok := routeDelayedVotes[accountName]; ok {
					netWithdraw -= selfDep
					delete(routeDelayedVotes, accountName)
				}
			}
			if netWithdraw > 0 {
				withdrawAmt := uint64(netWithdraw)
				eraseAmt := withdrawAmt
				if eraseAmt > a.SumDelayedVotes {
					eraseAmt = a.SumDelayedVotes
				}
				if eraseAmt > 0 {
					_ = objects.DelayedVotingEraseOnAccount(&a, eraseAmt)
				}
				leftover := withdrawAmt - eraseAmt
				if leftover > 0 {
					objects.AdjustProxiedWitnessVotes(s, &a, -int64(leftover))
				}
			}
		} else {
			objects.AdjustProxiedWitnessVotes(s, &a, -toWithdraw)
		}
	}

	if a.Withdrawn >= a.ToWithdraw || a.VestingShares == 0 {
		a.VestingWithdrawRate = 0
		a.NextVestingWithdrawal = 0
		a.ToWithdraw = 0
		a.Withdrawn = 0
	} else {
		dgpoNow, _ := objects.GetDGPO(s)
		a.NextVestingWithdrawal = scheduledTime + vestingWithdrawIntervalForBlock(dgpoNow.HeadBlockNumber)
		objects.PutVestingWithdrawalSchedule(s, a.NextVestingWithdrawal, accountName)
	}
	// HF20+: power-down decreased effective_vests → refresh RC cap +
	// clip any RC delegations now exceeding the new max. This mirrors
	// rc_plugin.cpp post_apply_operation_visitor for
	// fill_vesting_withdraw_operation, which calls
	// update_after_vest_change(from_account, fill_new_mana=true,
	// check_for_rc_delegation_overflow=true).
	if dgpoNow, _ := objects.GetDGPO(s); hardfork.SetForBlock(dgpoNow.HeadBlockNumber).Has(hardfork.HF20) {
		handleVestingWithdrawalRCImpact(s, &a, scheduledTime)
	}
	objects.PutAccount(s, &a)

	// Flush HF24 auto_vest route deposits to NON-self recipients as
	// delayed-vote buckets (hived update_votes → add_delayed_value): the
	// vest only reaches the recipient's witness totals after the 30-day
	// maturation. The self-route entry (if any) was netted into the
	// withdrawal above and removed from the map. Operations on distinct
	// recipients are independent, but iterate in sorted order for a
	// deterministic, replayable sequence.
	if len(routeDelayedVotes) > 0 {
		targets := make([]string, 0, len(routeDelayedVotes))
		for t := range routeDelayedVotes {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		for _, t := range targets {
			val := routeDelayedVotes[t]
			if val <= 0 {
				continue
			}
			ta, gerr := objects.GetAccount(s, t)
			if gerr != nil {
				continue
			}
			_ = objects.DelayedVotingAddOnAccount(&ta, blockTime, uint64(val))
			objects.PutAccount(s, &ta)
		}
	}

	dgpo.TotalVestingFundHive -= convertedHive
	dgpo.TotalVestingShares -= toConvertToHive
	objects.PutDGPO(s, &dgpo)
}

// processWithdrawRoutes handles the set_withdraw_vesting_route diversions
// for one account's withdrawal chunk. autoVest controls which pass we're
// running — VESTS routes first (preserve precision), HIVE routes second.
// Returns the total VESTS-satoshi diverted in this pass.
func processWithdrawRoutes(s *state.Session, fromAccount string, toWithdraw int64, autoVest bool, now uint32, delayedVotes map[string]int64) int64 {
	var deposited int64
	prefix := state.AppendAccountName(nil, fromAccount)
	_ = s.Range(objects.WithdrawRouteTable, state.PrimaryIndex, prefix,
		func(_, value []byte) (bool, error) {
			route, rerr := objects.ReadWithdrawRoute(value)
			if rerr != nil {
				return true, nil
			}
			if route.AutoVest != autoVest {
				return true, nil
			}
			// toWithdraw × percent / HIVE_100_PERCENT. C++ computes this in
			// uint128 (database_vesting.cpp:174); a plain int64 multiply
			// overflows once toWithdraw × percent > 2^63-1 — which a
			// mega-whale power-down hits (steemit's 68.3e15-VESTS power-down
			// at block 29,408,299 has a weekly chunk ~5.25e15; ×2500 =
			// 1.31e19 > int64 max → wraps negative → the route credit was
			// silently skipped, leaving steemit's 25% route recipients
			// (e.g. backup-bittrex) uncredited → transfer-out failed at
			// 29,612,472). Use 128-bit math.
			hiD, loD := bits.Mul64(uint64(toWithdraw), uint64(route.Percent))
			qD, _ := bits.Div64(hiD, loD, 10000)
			toDeposit := int64(qD)
			if toDeposit <= 0 {
				return true, nil
			}
			to, gerr := objects.GetAccount(s, route.To)
			if gerr != nil {
				return true, nil
			}
			if dgpoT, _ := objects.GetDGPO(s); wdTraceOn(dgpoT.HeadBlockNumber) {
				fmt.Fprintf(os.Stderr, "GHIVEDIFF WDROUTE block=%d from=%s to=%s pct=%d autoVest=%v toDeposit=%d\n",
					dgpoT.HeadBlockNumber, fromAccount, route.To, route.Percent, route.AutoVest, toDeposit)
			}
			if autoVest {
				to.VestingShares += toDeposit
				objects.PutAccount(s, &to)
				// C++ database_vesting.cpp:208-217 — auto_vest deposit
				// propagates the new vest into the recipient's witness
				// vote totals. Pre-HF24: immediately. HF24+: via delayed
				// voting (dv->add_votes → add_delayed_value at end-of-
				// withdrawal), so it only counts after the 30-day
				// maturation. Accumulate per-recipient; the caller flushes
				// the buckets (netting any self-route). Skipping this
				// entirely (the old behavior) permanently under-counted
				// every auto_vest-route recipient's witnesses post-HF24.
				dgpoNow, _ := objects.GetDGPO(s)
				if hardfork.SetForBlock(dgpoNow.HeadBlockNumber).Has(hardfork.HF24) {
					if delayedVotes != nil {
						delayedVotes[route.To] += toDeposit
					}
				} else if fresh, ferr := objects.GetAccount(s, route.To); ferr == nil {
					objects.AdjustProxiedWitnessVotes(s, &fresh, toDeposit)
				}
				// HF20+: route recipient's effective_vests increased →
				// refresh their RC cap. C++ post_apply on
				// fill_vesting_withdraw calls update_after_vest_change(
				// op.to_account) with fill_new_mana=true, no overflow
				// check (receiver gained vests, can't be over-delegated).
				if hardfork.SetForBlock(dgpoNow.HeadBlockNumber).Has(hardfork.HF20) {
					if fresh, ferr := objects.GetAccount(s, route.To); ferr == nil {
						freshNow := fresh
						eff := freshNow.VestingShares - freshNow.DelegatedVestingShares + freshNow.ReceivedVestingShares
						if eff < 0 {
							eff = 0
						}
						freshNow.LastMaxRC = eff - freshNow.DelegatedRCOut + freshNow.DelegatedRCIn
						if freshNow.LastMaxRC < 0 {
							freshNow.LastMaxRC = 0
						}
						objects.RegenerateRCMana(&freshNow, now)
						objects.PutAccount(s, &freshNow)
					}
				}
			} else {
				dgpo, derr := objects.GetDGPO(s)
				if derr == nil {
					hive := vestsToHive(toDeposit, &dgpo)
					to.Balance += hive
					dgpo.TotalVestingFundHive -= hive
					dgpo.TotalVestingShares -= toDeposit
					objects.PutDGPO(s, &dgpo)
				}
				objects.PutAccount(s, &to)
			}
			deposited += toDeposit
			return true, nil
		})
	return deposited
}
