package chain

import (
	"math/big"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/reward"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// processHardforks fires apply_hardfork(N) once for each hardfork whose
// activation block is THIS block. Mirror of database::process_hardforks
// (libraries/chain/database_hardfork.cpp:114) — we just compare the
// HardforkSet at blockNum vs blockNum-1 and trigger any newly-set bits.
//
// Apply must run BEFORE the per-block payout hooks at the activation
// block: HF1's vesting share split rescales every account's VESTS, and
// any cashout / withdraw that runs after the split must see the new
// values. Hived runs apply_hardfork inside process_hardforks at the
// END of apply_block, but the relevant invariant is "all post-apply
// hooks observe HF state matching the head block" — which is fine
// either way since the block whose HF activates can't reference the
// HF's effects in its own evaluator dispatch.
func processHardforks(s *state.Session, blockNum uint32) {
	if blockNum == 0 {
		return
	}
	// Fire applyHardfork at the END of the activation block (matches C++
	// process_hardforks at database.cpp:2368, after all per-block hooks).
	// State effects (e.g. HF1's 1M× vesting share split) become visible to
	// the NEXT block's evaluators — which is exactly when SetForBlock first
	// flags the HF as active (strict `>`, see hardfork.go:SetForBlock).
	for id := hardfork.ID(1); id <= hardfork.NumHardforks; id++ {
		if blockNum == hardfork.MainnetActivationBlock[id] {
			applyHardfork(s, id)
		}
	}
}

// RunHardforkActivation is an exported entry point for tests so the
// testfixture package can drive HF activation without walking blocks.
// Production callers go through processHardforks.
func RunHardforkActivation(s *state.Session, id hardfork.ID, blockNum uint32) {
	applyHardfork(s, id)
}

// applyHardfork dispatches the per-HF state-mutation logic, mirroring
// libraries/chain/database_hardfork.cpp:178. Most HFs are no-ops at the
// state level (their effects are encoded as gates on evaluators);
// the ones we model here are the ones whose state mutations are
// observable post-activation.
func applyHardfork(s *state.Session, id hardfork.ID) {
	// Mirror C++ apply_hardfork (database_hardfork.cpp:519-527): per-HF
	// effects run FIRST, then the hardfork_property_object is bumped.
	// This ordering matters when an effect handler reads
	// `hpo.current_hardfork_version`/`next_hardfork` — at that moment it
	// must reflect the PRE-bump state. (For HF1-HF12 the dispatch is
	// neutral because none of the effect handlers read hpo, but
	// HF22→HF23 forces a bump regardless of vote tally and a future
	// effect handler that reads hpo could trip on the wrong order.)
	switch id {
	case hardfork.HF1:
		performVestingShareSplit(s, 1_000_000)
	case hardfork.HF2, hardfork.HF3:
		// retally_witness_votes — pre-HF2 vote-tally accounting was
		// out of sync with the post-HF1 share split (and similar pre-
		// HF3 issues), so hived re-walks every witness_vote row to
		// reset witness.votes / TotalVotesVests from scratch.
		// libraries/chain/database_hardfork.cpp:192,196.
		retallyWitnessVotes(s)
	case hardfork.HF4:
		// HF4 (database_hardfork.cpp:199) calls
		// reset_virtual_schedule_time which zeros ws.current_virtual_time
		// and re-seeds every witness's VST = LAP_LENGTH2 / (votes+1).
		// Without this, Go's pre-HF4 VST evolution drifts permanently
		// from C++'s post-HF4 baseline.
		if ws, err := objects.GetWitnessSchedule(s); err == nil {
			resetVirtualScheduleTime(s, &ws)
			objects.PutWitnessSchedule(s, &ws)
		}
	case hardfork.HF6:
		// HF6 calls retally_witness_vote_counts(force=false) which only
		// repairs proxy-holding accounts. We don't model proxy chains
		// faithfully, so we conservatively retally every account here.
		// This may reset a few accounts whose pre-HF6 buggy counts
		// matched C++; the alternative is letting our cap-checks
		// disagree with C++ on accounts that proxied pre-HF6.
		retallyWitnessVoteCountsForProxiedOnly(s)
		if dgpo, err := objects.GetDGPO(s); err == nil {
			dgpo.ReverseAuctionSeconds = 30 * 60
			objects.PutDGPO(s, &dgpo)
		}
	case hardfork.HF8:
		// HF8 calls retally_witness_vote_counts(true) — repairs every
		// account by re-walking the witness_vote table.
		retallyWitnessVoteCounts(s)
	case hardfork.HF9:
		// HF9 mass-rotates 150 compromised-account owner/active/posting
		// authorities to a hived-controlled key and records the prior
		// authorities in owner_authority_history so subsequent
		// recover_account ops can succeed. Mirrors
		// database_hardfork.cpp:212-229 + hardfork.d/0_9.hf:19-169.
		applyHardfork9CompromisedAccounts(s)
	case hardfork.HF10:
		// HF10 calls retally_liquidity_weight() — recomputes every
		// liquidity_reward_balance row's weight using the
		// post-HF10 MIN(hive,hbd) formula (database_vesting.cpp:102,
		// database_hardfork.cpp:232). Pre-HF9 rows had been holding
		// stale PRODUCT weights that dwarfed post-HF9 MIN weights, so
		// pay_liquidity_reward kept paying out to accounts that hadn't
		// traded recently.
		retallyLiquidityWeight(s)
	case hardfork.HF12:
		// HF12__178 retires the liquidity-reward subsystem. Wipe every
		// row so the by_volume_weight scan in pay_liquidity_reward
		// returns nothing post-HF12 (pay_liquidity_reward also bails
		// post-HF12 as a guard).
		clearLiquidityRewardBalances(s)
		// apply_hardfork_12_comment_cashout_fix: walk every comment;
		// frozen-but-unpaid root posts get rescheduled for HF12_time+12h,
		// paid root posts get a second cashout window of creation+30d.
		// Replies stay frozen.
		applyHardfork12CommentCashoutFix(s)
	case hardfork.HF14:
		// HF14 enables the dynamic HBD print rate. Seed start/stop
		// percentages and the print rate at full (100% = 10000).
		if dgpo, err := objects.GetDGPO(s); err == nil {
			dgpo.HBDStartPercent = 200 // 2 %
			dgpo.HBDStopPercent = 500  // 5 %
			dgpo.HBDPrintRate = 10000  // initially 100 %; ramps each block
			objects.PutDGPO(s, &dgpo)
		}
	case hardfork.HF16:
		// Truncate the price-history window from 168 → 84 entries.
		truncateFeedHistoryWindow(s, 84)
		// Switch to the smooth inflation curve. Seed reward-split
		// percentages: 75 % content, 15 % vesting, 0 % proposal,
		// witness gets the remainder (10 %).
		if dgpo, err := objects.GetDGPO(s); err == nil {
			dgpo.ContentRewardPercent = 7500 // 75 %
			dgpo.VestingRewardPercent = 1500 // 15 %
			dgpo.ProposalFundPercent = 0
			objects.PutDGPO(s, &dgpo)
		}
	case hardfork.HF17:
		// Migrate total_reward_fund_hive into a "post" reward_fund_object
		// and zero out the dgpo aggregate. From here on, comment cashouts
		// draw from the fund's reward_balance and recent_claims tracks
		// the running curve sum.
		applyHardfork17(s)
		// apply_hardfork_17_comment_cashout_fix: re-extend every
		// scheduled root post and reply to the new 7-day window from
		// creation.
		applyHardfork17CommentCashoutFix(s)
	case hardfork.HF19:
		// Switch the post fund's curves: author=linear, curation=square_root.
		// HF19 also resets recent_claims to HIVE_HF_19_RECENT_CLAIMS
		// (140797515942543623) to recalibrate per-comment payouts under
		// the new linear curve.
		if fund, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
			fund.AuthorRewardCurve = objects.CurveLinear
			fund.CurationRewardCurve = objects.CurveSquareRoot
			var recent [16]byte
			low := uint64(140797515942543623)
			for i := 0; i < 8; i++ {
				recent[8+i] = byte(low >> (56 - i*8))
			}
			fund.RecentClaims = recent
			objects.PutRewardFund(s, &fund)
		}
		// Mirror database::remove_old_cashouts (database_comment.cpp:102).
		// At HF19 hived clears the entire comment_cashout_ex_index AND
		// removes every comment_cashout row whose cashout_time is at
		// maximum (= already paid / frozen). C++ keeps comment_object
		// itself; we collapse both into one Comment row, so we delete
		// the whole row for paid/frozen comments. The ~9 GiB CommentStore
		// growth on a full mainnet replay traces back to never having
		// run this cleanup — by block 21.5M the store held millions of
		// post-paid rows that hived had already dropped.
		removeOldCashouts(s)
		// Mirror C++ database_hardfork.cpp:304-320 — at HF19, drop every
		// vesting_delegation_object whose vesting_shares == 0. These
		// are leftover rows from HF17-era delegations that got reduced
		// to zero but never garbage-collected. Two-phase walk to avoid
		// mutating the index during iteration.
		var zeroDelegations []objects.VestingDelegation
		_ = s.Range(objects.VestingDelegationTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
			d, derr := objects.ReadVestingDelegation(value)
			if derr != nil {
				return false, derr
			}
			if d.VestingShares == 0 {
				zeroDelegations = append(zeroDelegations, d)
			}
			return true, nil
		})
		for _, d := range zeroDelegations {
			objects.DeleteVestingDelegation(s, d.Delegator, d.Delegatee)
		}
	case hardfork.HF20:
		if dgpo, err := objects.GetDGPO(s); err == nil {
			dgpo.HBDStartPercent = 900                     // 9 %
			dgpo.HBDStopPercent = 1000                     // 10 %
			dgpo.ReverseAuctionSeconds = 15 * 60           // 15 min
			dgpo.DelegationReturnPeriod = 5 * 24 * 60 * 60 // 5 days
			objects.PutDGPO(s, &dgpo)
		}
		// HF20 also multiplies witness account_creation_fee by
		// HIVE_CREATE_ACCOUNT_WITH_HIVE_MODIFIER (= 30) since accounts
		// were previously created at 1× fee but the new RC subsystem
		// makes that 30× too cheap. Mirrors database_hardfork.cpp:354.
		_ = objects.EnsureWitnessStore(s).RangePrimary(func(w *objects.Witness) (bool, error) {
			cp := *w
			cp.AccountCreationFee *= 30
			objects.PutWitness(s, &cp)
			return true, nil
		})
		// Mirror the rest of database_hardfork.cpp:325-353: bump the
		// median schedule's account_creation_fee by 30 (so existing
		// account_create_with_delegation ops cost the new minimum) and
		// reset dgpo.available_account_subsidies to 0 (the new RC pool
		// starts empty and fills via process_subsidy each block).
		if ws, werr := objects.GetWitnessSchedule(s); werr == nil {
			ws.MedianAccountCreationFee *= 30
			objects.PutWitnessSchedule(s, &ws)
		}
		if dgpo2, derr := objects.GetDGPO(s); derr == nil {
			dgpo2.AvailableAccountSubsidies = 0
			objects.PutDGPO(s, &dgpo2)
		}
		// Initialize voting manabar to full for every existing account
		// since pre-HF20 the field was a percentage; HF20 reinterprets
		// it as raw mana with a vesting-derived cap. We mirror by
		// keeping VotingManaCurrent at HIVE_100_PERCENT (10000) which
		// the vote evaluator clamps appropriately.
		//
		// HF20 RC subsystem bootstrap. Mirror C++ database_hardfork.cpp:
		// 392-403 — walk every account and seed:
		//   rc_adjustment = HIVE_RC_HISTORICAL_ACCOUNT_CREATION_ADJUSTMENT
		//                 = 2020748973 (= ~2020.748973 VESTS)
		//   rc_manabar.last_update_time = now
		//   rc_manabar.current_mana = max_rc (full pool)
		//   last_max_rc = max_rc
		//
		// Without this, every existing account at HF20 starts with
		// current_mana=0 instead of full. With EnforceRC=true (live
		// sync) almost every post-HF20 tx would fail ErrInsufficientRC
		// until the manabar regenerates over 5 days. Other RC bootstrap
		// pieces (rc_resource_param_object, rc_pool_object, 24 rc_usage
		// buckets, 2 rc_stats rows, witness rd-params) are still
		// missing — tracked separately. This is the highest-impact
		// piece since it affects EVERY account's max_rc and starting
		// mana.
		var hf20Time uint32
		if dgpoNow, derr := objects.GetDGPO(s); derr == nil {
			hf20Time = dgpoNow.Time
		}
		const rcHistoricalAdjustment int64 = 2020748973
		// HF20 voting manabar migration: pre-HF20 the voting_manabar
		// is in percent units (0..10000); HF20+ it switches to VESTS
		// units (0..effective_vesting_shares). Hived doesn't do an
		// explicit migration — it just changes how the field is
		// interpreted — but we need to convert the persisted value
		// because gopherd's HF20+ vote evaluator reads it as VESTS.
		//
		// Conversion:
		//
		//	current_mana_vests = effective_vesting * current_mana_pct / 10000
		//
		// Pre-HF21 the downvote_manabar doesn't exist (DownvotePoolPercent
		// was 0); HF21 activation seeds it. So at HF20 migration we only
		// touch voting_manabar.
		var hf20DowntvotePoolPct uint16 = 0
		_ = hf20DowntvotePoolPct
		_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
			a.RCAdjustment = rcHistoricalAdjustment
			a.RCManaLastUpdate = hf20Time
			maxRC := a.MaximumRC()
			a.RCManaCurrent = maxRC
			a.LastMaxRC = maxRC
			// Voting manabar migration. Pre-HF20 the field holds 0..10000.
			// Scale to VESTS = effective_vesting * pct / 10000.
			effVests := a.EffectiveVestingSharesExclWithdraw()
			if effVests > 0 && a.VotingManaCurrent > 0 {
				pct := a.VotingManaCurrent
				if pct > 10000 {
					pct = 10000
				}
				a.VotingManaCurrent = effVests * pct / 10000
			} else if effVests == 0 {
				a.VotingManaCurrent = 0
			}
			a.VotingManaLastUpdate = hf20Time
			a.DownvoteManaCurrent = 0
			a.DownvoteManaLastUpdate = hf20Time
			objects.PutAccount(s, a)
			return true, nil
		})
		// HF20 RC subsystem state-object bootstrap. Mirrors
		// database_hardfork.cpp:355-390 — creates rc_resource_param,
		// rc_pool seeded to pool_eq, 24 staggered rc_usage_buckets,
		// and the two rc_stats rows. Per-account rc_adjustment +
		// initial mana seed was the loop above; this is the rest of
		// the C++ HF20 init block.
		initRCBootstrapHF20(s, hf20Time)
	case hardfork.HF21:
		if dgpo, err := objects.GetDGPO(s); err == nil {
			dgpo.ContentRewardPercent = 6500    // 65 %
			dgpo.ProposalFundPercent = 1000     // 10 %
			dgpo.ReverseAuctionSeconds = 5 * 60 // 5 min
			// HIVE_DOWNVOTE_POOL_PERCENT_HF21 = 25 × HIVE_1_PERCENT
			// = 2500 (25 %). Mirrors database_hardfork.cpp HF21 init.
			dgpo.DownvotePoolPercent = 2500
			// DHF: don't seed LastBudgetTime here. hived leaves
			// last_budget_time at its HIVE_GENESIS_TIME default until
			// the first dhf_processor::run call (which calls
			// update_settings → sets last_budget_time = head_time).
			// On mainnet, that first call sees an empty treasury
			// (steem.dao was just created in this same HF21 hook with
			// 0 HBD), so the resulting huge passed_time × 0 = 0
			// payout is benign. From the second tick onward,
			// passed_time = 3600 in steady state. processProposals
			// has a fallback to proposalMaintenancePeriodSec when
			// LastBudgetTime == 0, so the first-tick math matches
			// the historical hardcoded-3600 behavior.
			objects.PutDGPO(s, &dgpo)
		}
		// HF21 also creates the treasury account ("steem.dao") if absent
		// and switches the post fund's curves to convergent_linear /
		// convergent_square_root with HIVE_CONTENT_CONSTANT_HF21
		// (= 2000000000000 still, same as HF0; only the curve changes).
		if has, _ := objects.HasAccount(s, "steem.dao"); !has {
			if dgpo, err := objects.GetDGPO(s); err == nil {
				objects.PutAccount(s, &objects.Account{
					Name:                 "steem.dao",
					Created:              dgpo.Time,
					VotingManaCurrent:    10000,
					VotingManaLastUpdate: dgpo.Time,
				})
			}
		}
		// Lock the treasury account so its authorities can never
		// authorize an op (no key, no account auth, threshold=1).
		// Mirrors C++ database_hardfork.cpp:427 lock_account(get_treasury())
		// at HF21 activation.
		if dgpo, err := objects.GetDGPO(s); err == nil {
			lockAccount(s, "steem.dao", dgpo.Time)
		}
		if fund, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
			fund.AuthorRewardCurve = objects.CurveConvergentLinear
			fund.CurationRewardCurve = objects.CurveConvergentSquareRoot
			fund.PercentCurationRewards = 5000 // 50 %
			// HIVE_HF21_CONVERGENT_LINEAR_RECENT_CLAIMS = uint128(0,
			// 503600561838938636ull). Reset recent_claims at HF21 to
			// recalibrate per-comment payouts under the new curve.
			var recent [16]byte
			low := uint64(503600561838938636)
			for i := 0; i < 8; i++ {
				recent[8+i] = byte(low >> (56 - i*8))
			}
			fund.RecentClaims = recent
			objects.PutRewardFund(s, &fund)
		}
	case hardfork.HF22:
		// HF22 (libraries/chain/database_hardfork.cpp:452) is a no-op
		// at the state level; new behavior is gated by HF22 in
		// individual evaluators (governance vote expiration).
	case hardfork.HF23:
		// Hive fork airdrop: zero the Steemit / Tron-puppet account
		// list (libraries/chain/database_hardfork.cpp:454). 328 names.
		clearAccounts(s, hf23ClearAccounts)
	case hardfork.HF24:
		// Restore a small set of accounts mistakenly cleared at HF23
		// (no-op in our model — see hardfork_accounts.go), then rename
		// the treasury from steem.dao to hive.fund and consolidate.
		restoreAccounts(s, hf24RestoreAccounts)
		if dgpo, err := objects.GetDGPO(s); err == nil {
			consolidateTreasuryBalance(s, dgpo.Time)
			// Lock the new treasury (hive.fund). Mirrors C++
			// database_hardfork.cpp:528 lock_account(get_treasury())
			// at HF24 activation, called AFTER consolidation so the
			// rotated treasury balance is preserved before the
			// authority freeze.
			lockAccount(s, "hive.fund", dgpo.Time)
		}
	case hardfork.HF25:
		// HF25 (libraries/chain/database_hardfork.cpp:470-483): switch
		// the post fund's curves to linear/linear and DISABLE the
		// reverse-auction discount in favor of the new
		// early_voting / mid_voting phase scheme. From the C++ comment
		// at hive_evaluator_social.cpp:797: "In HF25
		// dgpo.reverse_auction_seconds is set to zero. It's replaced
		// by dgpo.early_voting_seconds and dgpo.mid_voting_seconds."
		//
		// HF21 set the early/mid voting fields to 0 (struct default),
		// so the /=2 and /=8 discount branches in the curation weight
		// computation never fire pre-HF25 — hived gates them on
		// `dgpo.get_early_voting_seconds() > 0`. The gopherd vote
		// evaluator now mirrors that gate (comment.go:957+), keying
		// off hardfork.HF25 rather than HF21 to avoid double-discounting
		// the HF21→HF25 window (~33M-62M blocks).
		if fund, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
			fund.AuthorRewardCurve = objects.CurveLinear
			fund.CurationRewardCurve = objects.CurveLinear
			objects.PutRewardFund(s, &fund)
		}
		if dgpo, err := objects.GetDGPO(s); err == nil {
			dgpo.ReverseAuctionSeconds = 0 // disabled at HF25
			objects.PutDGPO(s, &dgpo)
		}
	case hardfork.HF26:
		if dgpo, err := objects.GetDGPO(s); err == nil {
			dgpo.HBDStartPercent = 2000 // 20 %
			dgpo.HBDStopPercent = 2000  // 20 %
			objects.PutDGPO(s, &dgpo)
		}
		// Seed the future_witness_schedule_object as a copy of the
		// current one. Mirrors C++ database_hardfork.cpp:492-496 — at
		// HF26 activation, hived creates a second witness_schedule_object
		// initialized via copy_values_from(get_witness_schedule_object()).
		// Thereafter each round-boundary update_witness_schedule swaps
		// future→current and recomputes the new future.
		if ws, err := objects.GetWitnessSchedule(s); err == nil {
			objects.PutFutureWitnessSchedule(s, &ws)
		}
	case hardfork.HF27:
		// HF27 (block 69,050,084) — no consensus-state mutation in
		// hived's hardfork hook beyond hardfork_property_object update.
	case hardfork.HF28:
		// HF28 removes proposal votes for accounts that have declined
		// voting rights. The set of such accounts is enumerated by
		// scanning DeclineVotingRightsRequest history; we approximate
		// by walking all accounts and checking VoteRevoked.
		removeProposalVotesForRevokedAccounts(s)
	}
	// Per-HF effects done — now bump processed_hardforks /
	// last_hardfork / current_hardfork_version. Mirrors C++ ordering
	// at database_hardfork.cpp:519-527.
	bumpHardforkPropertyObject(s, id)
}

// bumpHardforkPropertyObject mirrors the per-apply_hardfork housekeeping
// from C++ database::apply_hardfork — push the activation timestamp into
// processed_hardforks, bump last_hardfork by one, and update
// current_hardfork_version. Idempotent against double-fire: if last_hardfork
// already equals id, no-op.
func bumpHardforkPropertyObject(s *state.Session, id hardfork.ID) {
	hpo, err := objects.GetHardforkPropertyObject(s)
	if err != nil {
		return
	}
	if hpo.LastHardfork >= uint32(id) {
		return
	}
	// Mirror C++ database_hardfork.cpp:512-515 — processed_hardforks[id]
	// is the CANONICAL HIVE_HARDFORK_0_N_TIME constant, not the
	// activation block's timestamp. C++ asserts equality after push.
	// Gopherd previously stored dgpo.Time (the activation block's
	// wall-clock timestamp), which for mainnet doesn't match the
	// constant (e.g., HIVE_HARDFORK_0_1_TIME = 1461605400 vs the
	// actual mainnet block 905693 timestamp). State-dump diff only,
	// but the parity-fix is cheap.
	t := hardfork.MainnetActivationTime[id]
	hpo.ProcessedHardforks = append(hpo.ProcessedHardforks, t)
	hpo.LastHardfork = uint32(id)
	hpo.CurrentHardforkVersion = hardfork.PackedVersion(id)
	// next_hardfork tracks the latest applied version when no
	// vote-driven future fork is queued. Once update_witness_schedule_4
	// votes a future (version, time) tuple, those overwrite this.
	if hpo.NextHardfork < hpo.CurrentHardforkVersion {
		hpo.NextHardfork = hpo.CurrentHardforkVersion
		hpo.NextHardforkTime = t
	}
	objects.PutHardforkPropertyObject(s, &hpo)
}

// removeProposalVotesForRevokedAccounts walks the proposal_vote table
// and removes every entry whose voter has VoteRevoked = true. Mirrors
// database::remove_proposal_votes_for_accounts_without_voting_rights
// triggered at HF28.
func removeProposalVotesForRevokedAccounts(s *state.Session) {
	type pair struct {
		proposalID int64
		voter      string
	}
	var toDelete []pair
	_ = s.Range(objects.ProposalVoteTable, state.PrimaryIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 8+state.AccountNameKeyLen {
				return true, nil
			}
			// AppendInt64BE flips the sign bit to make sorted-int order
			// match byte-lex order; reverse that here.
			var u uint64
			for i := 0; i < 8; i++ {
				u = (u << 8) | uint64(body[i])
			}
			id := int64(u ^ (1 << 63))
			voterBytes := body[8 : 8+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && voterBytes[n-1] == 0 {
				n--
			}
			voter := string(voterBytes[:n])
			a, err := objects.GetAccount(s, voter)
			if err == nil && a.VoteRevoked {
				toDelete = append(toDelete, pair{id, voter})
			}
			return true, nil
		})
	for _, p := range toDelete {
		objects.DeleteProposalVote(s, p.proposalID, p.voter)
	}
}

// applyHardfork17 migrates the chain to the reward_fund_object model.
// Mirrors libraries/chain/database_hardfork.cpp:278. Critically the
// recent_claims field starts at HIVE_HF_17_RECENT_CLAIMS — a hard-coded
// uint128 (≈1.49e31) representing the prior chain-wide claim weight.
// This prevents the first post-HF17 cashouts from drawing massively
// outsized payouts from a near-empty denominator.
func applyHardfork17(s *state.Session) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	if has, _ := objects.HasRewardFund(s, objects.PostRewardFundName); has {
		return
	}
	// HIVE_HF_17_RECENT_CLAIMS = (808638359297 << 64) | 13744269167557038121.
	// Encode as 16-byte big-endian uint128.
	var recent [16]byte
	high := uint64(808638359297)
	low := uint64(13744269167557038121)
	for i := 0; i < 8; i++ {
		recent[i] = byte(high >> (56 - i*8))
	}
	for i := 0; i < 8; i++ {
		recent[8+i] = byte(low >> (56 - i*8))
	}
	// HF17 reward-fund transition. In hived the reward_fund_object is
	// created at genesis (database_init.cpp:258) with reward_balance = 0;
	// pre-HF17 process_funds (database.cpp:1794) routes content_reward
	// through pay_reward_funds, which adds to fund.reward_balance. The
	// HF17 handler (database_hardfork.cpp:276-281) only writes
	// recent_claims = HIVE_HF_17_RECENT_CLAIMS; reward_balance is not
	// touched. We don't have a fund pre-HF17 so we accumulate the same
	// content_reward stream in dgpo.TotalRewardFundHive and pre-HF17
	// cashouts draw from it.
	//
	// Migrate the pre-HF17 accumulator into the new fund. In hived the
	// reward_fund_object exists from genesis and grows continuously via
	// pay_reward_funds; the HF17 handler does NOT reset reward_balance.
	// gopherd accumulated the same stream in dgpo.TotalRewardFundHive
	// (drained by pre-HF17 cashouts at comment_cashout.go:478). At the
	// transition whatever's left in that accumulator becomes the seed
	// for fund.RewardBalance — without it, post-HF17 cashouts start
	// from 0 and systematically under-pay the head of the post-HF17
	// window. Zero the dgpo accumulator after migrating so subsequent
	// reads see the moved value.
	seedBalance := dgpo.TotalRewardFundHive
	dgpo.TotalRewardFundHive = 0
	objects.PutRewardFund(s, &objects.RewardFund{
		Name:                   objects.PostRewardFundName,
		RewardBalance:          seedBalance,
		RecentClaims:           recent,
		LastUpdate:             1458835200, // HIVE_GENESIS_TIME
		ContentConstant:        2_000_000_000_000,
		PercentCurationRewards: 2500,
		PercentContentRewards:  10000,
		AuthorRewardCurve:      objects.CurveQuadratic,
		CurationRewardCurve:    objects.CurveQuadratic,
	})
	objects.PutDGPO(s, &dgpo)

	// Witness slot reallocation. Hived's HF17 (database_hardfork.cpp:269-274)
	// drops the dedicated miner slot — moving its weight into the elected
	// pool — and keeps a single timeshare slot:
	//   HIVE_MAX_VOTED_WITNESSES_HF17  = 20
	//   HIVE_MAX_MINER_WITNESSES_HF17  =  0
	//   HIVE_MAX_RUNNER_WITNESSES_HF17 =  1
	// The pay_norm factor is computed from the actual ACTIVE counts at
	// schedule rebuild, so 1×20 + 1×0 + 5×1 = 25 — the same value as
	// pre-HF17's 1×19 + 1×1 + 5×1 = 25. But the witness IDENTITIES in
	// the active set change because the schedule fills max-voted slots
	// from the top-N elected pool, and a 20-slot pool pulls in one more
	// elected candidate than a 19-slot pool. Without this update, the
	// schedule rebuilder keeps the 19/1/1 split and the miner slot stays
	// populated with a pow-miner candidate that mainnet has retired.
	if wso, err := objects.GetWitnessSchedule(s); err == nil {
		wso.MaxVotedWitnesses = 20
		wso.MaxMinerWitnesses = 0
		wso.MaxRunnerWitnesses = 1
		objects.PutWitnessSchedule(s, &wso)
	}
}

// performVestingShareSplit mirrors database::perform_vesting_share_split
// (libraries/chain/database_comment.cpp:612). For HF1 magnitude is
// 1,000,000:
//
//	dgpo.total_vesting_shares    *= magnitude
//	dgpo.total_reward_shares2     = 0    (and recomputed below)
//	for each account:
//	    vesting_shares           *= magnitude
//	    withdrawn                *= magnitude
//	    to_withdraw              *= magnitude
//	    vesting_withdraw_rate     = to_withdraw / 104  (recompute)
//	    proxied_vsf_votes[*]     *= magnitude  (we don't model)
//	for each comment with cashout:
//	    net_rshares × magnitude   (effective)
//	then re-add evaluate_reward_curve(net_rshares × magnitude) for each
//	positive-rshares comment to total_reward_shares2.
//
// Applied as a single Range-and-batch since the underlying pebble overlay
// can absorb the per-account modifications without invalidating the
// outer iterator.
func performVestingShareSplit(s *state.Session, magnitude int64) {
	type acctMod struct {
		owner string
	}
	var acctNames []acctMod
	_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		acctNames = append(acctNames, acctMod{owner: a.Name})
		return true, nil
	})
	for _, m := range acctNames {
		a, err := objects.GetAccount(s, m.owner)
		if err != nil {
			continue
		}
		a.VestingShares *= magnitude
		a.Withdrawn *= magnitude
		a.ToWithdraw *= magnitude
		// Pre-HF16 rate divisor (HF16 hasn't fired yet at HF1).
		if a.ToWithdraw > 0 {
			a.VestingWithdrawRate = a.ToWithdraw / 104
			if a.VestingWithdrawRate == 0 {
				a.VestingWithdrawRate = 1
			}
		}
		a.DelegatedVestingShares *= magnitude
		a.ReceivedVestingShares *= magnitude
		// Mirror C++ database_comment.cpp:636 — proxied_vsf_votes are
		// also scaled by the share-split magnitude.
		for i := range a.ProxiedVSFVotes {
			a.ProxiedVSFVotes[i] *= magnitude
		}
		objects.PutAccount(s, &a)
	}

	dgpo, err := objects.GetDGPO(s)
	if err == nil {
		dgpo.TotalVestingShares *= magnitude
		// Reset total_reward_shares2; the matching engine + cashout
		// pipeline rebuilds it as comments cash out post-HF1. Hived
		// re-walks every comment_cashout here to add curve(rshares)
		// back; we approximate by leaving it zero-then-grow which is
		// equivalent for fresh post-HF1 votes.
		dgpo.TotalRewardShares2 = [16]byte{}
		objects.PutDGPO(s, &dgpo)
	}

	// Walk every Comment and scale net_rshares (we keep an explicit
	// field). For each positive-rshares comment, re-add its scaled
	// curve value to total_reward_shares2.
	rebuildCommentRshares(s, magnitude)
}

// rebuildCommentRshares scales every comment's NetRshares (and the
// AbsRshares + VoteRshares accumulators) by magnitude and re-adds
// curve(NetRshares × magnitude) to dgpo.TotalRewardShares2 for every
// positive-rshares comment. Mirrors the comments-and-rshares loop in
// perform_vesting_share_split (database_comment.cpp:681-701).
//
// Without the post-scale curve re-add, dgpo.total_reward_shares2 stays
// at 0 (it was zeroed at the top of the share split) and pre-HF1
// comments' contributions never come back. The first post-HF1 vote on
// any pre-HF1 comment then computes
//
//	delta = curve(old_net_post_scale + rshares) - curve(old_net_post_scale)
//
// against a shares2=0 baseline — i.e., adds only the *delta* of that
// vote rather than the full curve. Cumulative deficit ≈ sum of
// curve(pre_HF1_net × magnitude) over every comment that had positive
// rshares pre-HF1, which is exactly the constant 1.6% gap we saw at
// the HF7 cashout cascade vs hived.
func rebuildCommentRshares(s *state.Session, magnitude int64) {
	type pending struct {
		author, permlink string
	}
	var queue []pending
	_ = objects.RangeAllComments(s, func(c *objects.Comment) (bool, error) {
		queue = append(queue, pending{author: string(c.Author), permlink: c.Permlink})
		return true, nil
	})
	dgpo, dgpoErr := objects.GetDGPO(s)
	total := reward.Uint128FromBytes(dgpo.TotalRewardShares2)
	for _, p := range queue {
		c, err := objects.GetComment(s, p.author, p.permlink)
		if err != nil {
			continue
		}
		c.NetRshares *= magnitude
		// AbsRshares + VoteRshares are accumulators with the same
		// per-vote scale as NetRshares, so they multiply by magnitude
		// too (mirrors comment_cashout_ex.accumulate_abs_rshares at
		// database_comment.cpp:693).
		c.AbsRshares *= magnitude
		// VoteRshares is uint64; cast through math/big to avoid signed
		// overflow when magnitude × rshares stays below 2^64.
		if c.VoteRshares > 0 {
			vr := new(big.Int).Mul(new(big.Int).SetUint64(c.VoteRshares), big.NewInt(magnitude))
			if vr.IsUint64() {
				c.VoteRshares = vr.Uint64()
			} else {
				c.VoteRshares = ^uint64(0)
			}
		}
		// NOTE: children_abs_rshares is NOT scaled. C++'s
		// perform_vesting_share_split (database_comment.cpp:697-707)
		// scales c.net_rshares, c.vote_rshares, and c_ex.abs_rshares,
		// but leaves children_abs_rshares alone. Post-HF1 votes drown
		// out the pre-HF1 children_abs_rshares (they're 1M× larger),
		// so the weighted-avg cashout-time pulls toward the late
		// vote — matching how the chain ran. If we scale
		// children_abs_rshares × magnitude, the post-HF1 vote becomes
		// comparable to the historical cumulative pre-HF1 abs and
		// barely moves cashout_time, causing pre-HF7 abit posts (and
		// many others) to land in the cashout cascade in the wrong
		// order, which compounds into per-cashout fund/shares2
		// divergence and a ~10M VESTS shortfall on pfunk by the
		// withdraw_vesting at block 2,897,769.
		objects.PutComment(s, &c)
		if c.NetRshares > 0 {
			total = total.Add(reward.EvaluateRewardCurveQuadratic(c.NetRshares))
		}
	}
	if dgpoErr == nil {
		dgpo.TotalRewardShares2 = reward.Uint128ToBytes(total)
		objects.PutDGPO(s, &dgpo)
	}
}

// applyHardfork12CommentCashoutFix mirrors
// database::apply_hardfork_12_comment_cashout_fix
// (database_comment.cpp:579). At HF12 every root post in the cashout
// schedule gets rescheduled:
//   - already paid (cashout_time = 0 sentinel in our model): second
//     cashout = creation_time + HIVE_CASHOUT_WINDOW_SECONDS_PRE_HF17 (12h)
//     (hived uses 30d but we follow the simpler "+12h from now" branch
//     of the fix that applies to fresh posts)
//   - never paid (cashout_time = ?): set to HF12_time + 12h
//
// Replies stay frozen; HF17 will re-extend them later.
//
// Hived's exact fix (paraphrased):
//   - was_paid → cashout_time = head_block_time + 30d (second cashout)
//   - !was_paid AND no votes → cashout_time = head_block_time + 12h
//   - !was_paid AND with votes → cashout_time = max (frozen)
//
// We approximate by giving every paid root post a second cashout 12h
// from now (instead of 30d) — close enough for our test window since
// the cashouts ultimately drain the reward fund similarly.
func applyHardfork12CommentCashoutFix(s *state.Session) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	now := dgpo.Time
	type pending struct {
		author, permlink string
		oldCashout       uint32
		created          uint32
		netRshares       int64
		isPaidOut        bool
	}
	var queue []pending
	_ = objects.RangeAllComments(s, func(c *objects.Comment) (bool, error) {
		if c.ParentAuthor != "" {
			return true, nil // replies stay frozen
		}
		queue = append(queue, pending{
			author:     string(c.Author),
			permlink:   c.Permlink,
			oldCashout: c.CashoutTime,
			created:    c.Created,
			netRshares: c.NetRshares,
			isPaidOut:  c.CashoutTime == 0,
		})
		return true, nil
	})

	// Mirror C++ database_comment.cpp:579:
	//   - root + unpaid + frozen (cashout==max): cashout = head + 12h (HIVE_CASHOUT_WINDOW_SECONDS_PRE_HF17)
	//   - root + already paid: cashout = last_payout + HIVE_SECOND_CASHOUT_WINDOW (30d mainnet)
	//   - replies: untouched (stay frozen until HF17 fix lifts them)
	// We approximate `was_paid()` via LastPayout > 0 (set by cashout)
	// and "frozen" via CashoutTime == 0 (our maximum sentinel).
	const cashoutWindowPreHF17 uint32 = 60 * 60 * 12 // 12 hours
	const secondCashoutWindow uint32 = 60 * 60 * 24 * 30 // 30 days
	for _, p := range queue {
		c, err := objects.GetComment(s, p.author, p.permlink)
		if err != nil {
			continue
		}
		// Drop the old schedule entry first.
		if p.oldCashout != 0 {
			objects.DeleteCommentCashoutSchedule(s, p.oldCashout, c.ID)
		}
		var newCashout uint32
		if c.WasPaid && c.LastPayout > 0 {
			// Was paid at LastPayout; reschedule for the second cashout
			// window 30 days after that payout.
			newCashout = c.LastPayout + secondCashoutWindow
		} else if p.isPaidOut {
			// Frozen (cashout==max). Schedule fresh: head + 12h.
			newCashout = now + cashoutWindowPreHF17
		} else {
			// Unpaid, not yet frozen — leave alone with same cashout
			// (re-add the schedule entry).
			newCashout = p.oldCashout
		}
		c.CashoutTime = newCashout
		objects.PutComment(s, &c)
		if newCashout != 0 {
			objects.PutCommentCashoutSchedule(s, newCashout, c.ID, p.author, p.permlink)
		}
	}
}

// removeOldCashouts mirrors database::remove_old_cashouts
// (database_comment.cpp:102), invoked from the HF19 hardfork handler.
// Iterates every comment and drops the ones whose CashoutTime is the
// "frozen / paid" sentinel (0 in our model, time_point_sec::maximum in
// C++). C++ separately retains the long-lived comment_object; our
// schema is unified, so this is a stronger purge but consistent with
// post-HF19 reality (paid comments are unreachable via cashout-time
// lookups anyway).
func removeOldCashouts(s *state.Session) {
	type victim struct {
		author   string
		permlink string
	}
	var victims []victim
	_ = objects.RangeAllComments(s, func(c *objects.Comment) (bool, error) {
		if c.CashoutTime == 0 {
			victims = append(victims, victim{string(c.Author), c.Permlink})
		}
		return true, nil
	})
	for _, v := range victims {
		objects.DeleteComment(s, auth.AccountName(v.author), v.permlink)
	}
}

// applyHardfork17CommentCashoutFix mirrors
// database::apply_hardfork_17_comment_cashout_fix
// (database_comment.cpp:533-581). Two passes:
//
//	1. Root posts:  cashout = max(creation + 7d, old_cashout)
//	2. Replies:     cashout = max(root.cashout_AFTER_pass_1, creation + 7d)
//
// The C++ replies pass calls calculate_discussion_payout_time, which
// returns the ROOT's (just-updated) cashout_time — so replies share
// their thread root's cashout schedule. Earlier Go applied the root
// formula to replies too, which under-delays threaded posts and
// triggers `claim_reward_balance: insufficient balance` mismatches at
// the first post-HF17 author cashouts (e.g. block 10,635,691 "ocrdu").
func applyHardfork17CommentCashoutFix(s *state.Session) {
	const sevenDays uint32 = 60 * 60 * 24 * 7
	type pending struct {
		author, permlink string
		rootAuthor       string
		rootPermlink     string
		oldCashout       uint32
		created          uint32
		netRshares       int64
		id               uint64
		isRoot           bool
	}
	var roots []pending
	var replies []pending
	_ = objects.RangeAllComments(s, func(c *objects.Comment) (bool, error) {
		isRoot := string(c.RootAuthor) == string(c.Author) && c.RootPermlink == c.Permlink
		// Treat empty root fields (legacy rows) as root-itself, matching
		// the ReadAccount fallback path.
		if string(c.RootAuthor) == "" && c.RootPermlink == "" {
			isRoot = true
		}
		p := pending{
			author:       string(c.Author),
			permlink:     c.Permlink,
			rootAuthor:   string(c.RootAuthor),
			rootPermlink: c.RootPermlink,
			oldCashout:   c.CashoutTime,
			created:      c.Created,
			netRshares:   c.NetRshares,
			id:           c.ID,
			isRoot:       isRoot,
		}
		if isRoot {
			roots = append(roots, p)
		} else {
			replies = append(replies, p)
		}
		return true, nil
	})

	// Pass 1: root posts. cashout = max(creation + 7d, old_cashout).
	newRootCashout := make(map[string]uint32, len(roots))
	for _, p := range roots {
		c, err := objects.GetComment(s, p.author, p.permlink)
		if err != nil {
			continue
		}
		if p.oldCashout != 0 {
			objects.DeleteCommentCashoutSchedule(s, p.oldCashout, c.ID)
		}
		var newCashout uint32
		if p.oldCashout == 0 && c.NetRshares <= 0 {
			// Frozen / empty root — schedule fresh 7d.
			newCashout = p.created + sevenDays
		} else {
			newCashout = p.created + sevenDays
			if p.oldCashout > newCashout {
				newCashout = p.oldCashout
			}
		}
		c.CashoutTime = newCashout
		objects.PutComment(s, &c)
		if newCashout != 0 {
			objects.PutCommentCashoutSchedule(s, newCashout, c.ID, p.author, p.permlink)
		}
		newRootCashout[p.author+"\x00"+p.permlink] = newCashout
	}

	// Pass 2: replies. cashout = max(root.new_cashout, creation + 7d).
	// Replies whose root no longer exists fall back to creation+7d.
	for _, p := range replies {
		c, err := objects.GetComment(s, p.author, p.permlink)
		if err != nil {
			continue
		}
		if p.oldCashout != 0 {
			objects.DeleteCommentCashoutSchedule(s, p.oldCashout, c.ID)
		}
		floor := p.created + sevenDays
		rootCashout, ok := newRootCashout[p.rootAuthor+"\x00"+p.rootPermlink]
		if !ok {
			// Root missing from this pass — likely the reply has stale
			// Root* fields. Treat as orphan: floor at creation+7d.
			rootCashout = 0
		}
		newCashout := floor
		if rootCashout > newCashout {
			newCashout = rootCashout
		}
		c.CashoutTime = newCashout
		objects.PutComment(s, &c)
		if newCashout != 0 {
			objects.PutCommentCashoutSchedule(s, newCashout, c.ID, p.author, p.permlink)
		}
	}
}

// retallyLiquidityWeight recomputes every liquidity_reward_balance
// row's weight under the HF10+ MIN(hive,hbd) formula. Mirrors C++
// database::retally_liquidity_weight (database_vesting.cpp:102) which
// is invoked from the HF10 hardfork handler (database_hardfork.cpp:232).
func retallyLiquidityWeight(s *state.Session) {
	type row struct {
		owner       string
		hive, hbd   int64
		lastUpdate  uint32
		prevWeight  [16]byte
	}
	var rows []row
	_ = s.Range(objects.LiquidityRewardBalanceTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			b, err := objects.ReadLiquidityRewardBalance(value)
			if err != nil {
				return true, nil
			}
			rows = append(rows, row{
				owner:      b.Owner,
				hive:       b.HiveVolume,
				hbd:        b.HBDVolume,
				lastUpdate: b.LastUpdate,
				prevWeight: b.Weight,
			})
			return true, nil
		})
	for _, r := range rows {
		b := objects.LiquidityRewardBalance{
			Owner:      r.owner,
			HiveVolume: r.hive,
			HBDVolume:  r.hbd,
			LastUpdate: r.lastUpdate,
			Weight:     objects.ComputeLiquidityWeight(r.hive, r.hbd, true /* HF10+ → MIN */),
		}
		objects.PutLiquidityRewardBalanceUpdating(s, &b, r.prevWeight)
	}
}

// clearLiquidityRewardBalances drops every row in the
// liquidity_reward_balance table. Called from HF12.
func clearLiquidityRewardBalances(s *state.Session) {
	type row struct {
		owner  string
		weight [16]byte
	}
	var rows []row
	_ = s.Range(objects.LiquidityRewardBalanceTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			b, err := objects.ReadLiquidityRewardBalance(value)
			if err != nil {
				return true, nil
			}
			rows = append(rows, row{owner: b.Owner, weight: b.Weight})
			return true, nil
		})
	for _, r := range rows {
		b, err := objects.GetLiquidityRewardBalance(s, r.owner)
		if err == nil {
			objects.DeleteLiquidityRewardBalanceFull(s, &b)
		}
	}
}

// retallyWitnessVotes mirrors database::retally_witness_votes
// (libraries/chain/database_witness.cpp:113). Reset every witness's
// total_votes_vests to 0, then walk every account; for accounts WITHOUT
// a proxy, add their full governance_vote_power to each witness they
// vote for. Accounts WITH a proxy are skipped — their vest already
// propagated through the proxy chain into their proxy's
// proxied_vsf_votes, which is then folded into the proxy chain's
// terminal voter.
//
// Used by HF2 / HF3 to repair the per-witness totals after the HF1
// share split (and earlier proxy accounting bugs).
func retallyWitnessVotes(s *state.Session) {
	// Step 1: zero every witness's TotalVotesVests.
	type wRec struct{ owner string }
	var witnesses []wRec
	_ = objects.EnsureWitnessStore(s).RangePrimary(func(w *objects.Witness) (bool, error) {
		witnesses = append(witnesses, wRec{owner: w.Owner})
		return true, nil
	})
	// C++ retally_witness_votes (database_witness.cpp:113) zeros
	// w.votes AND w.virtual_position; it does NOT touch
	// virtual_last_update or virtual_scheduled_time. The subsequent
	// per-account adjust_witness_vote calls re-derive both from the
	// current virtual time.
	for _, wr := range witnesses {
		w, err := objects.GetWitness(s, wr.owner)
		if err != nil {
			continue
		}
		w.TotalVotesVests = 0
		w.VirtualPosition = [16]byte{}
		objects.PutWitness(s, &w)
	}

	// Step 2: walk every account. Skip proxy holders. For others,
	// re-walk their witness_vote rows and add governance_vote_power
	// (= direct vest + sum(proxied_vsf_votes)) to each.
	type acctRec struct{ name string }
	var accounts []acctRec
	_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		accounts = append(accounts, acctRec{name: a.Name})
		return true, nil
	})
	dgpo, _ := objects.GetDGPO(s)
	hf := hardfork.SetForBlock(dgpo.HeadBlockNumber)
	for _, ar := range accounts {
		a, err := objects.GetAccount(s, ar.name)
		if err != nil {
			continue
		}
		if a.Proxy != "" {
			continue
		}
		// Direct vest pre-HF24 (HF24+ would subtract sum_delayed_votes
		// but HF24 hasn't fired here at HF2/HF3).
		power := a.VestingShares + objects.SumProxiedVotes(&a)
		if power == 0 {
			continue
		}
		for _, witnessName := range objects.EnsureWitnessVoteStore(s).VotesFor(ar.name) {
			w, gerr := objects.GetWitness(s, witnessName)
			if gerr != nil {
				continue
			}
			// C++ retally calls adjust_witness_vote per voter,
			// which advances virtual_position / virtual_last_update
			// / virtual_scheduled_time alongside the vote bump.
			objects.AdjustWitnessVote(s, &w, power, hf)
			objects.PutWitness(s, &w)
		}
	}
}

// retallyWitnessVoteCountsForProxiedOnly mirrors hived's
// retally_witness_vote_counts(force=false) — and its real behavior
// includes a long-standing bug: for accounts WITHOUT a proxy, the local
// `witnesses_voted_for` counter stays 0, then the comparison
// `a.witnesses_voted_for != 0` triggers a reset of every non-proxy
// account's witnesses_voted_for to 0. (The intended path was to count
// rows only for proxy holders and leave others alone, but the
// implementation slipped.)
//
// This matters: post-HF6, real users like linouxis9 (no proxy, ~14
// votes pre-HF6) have their counter zeroed. Re-adding a vote later
// then starts from 0, so they can accumulate well past the apparent
// cap before HF8's force=true retally restores accuracy. To stay
// replay-compatible with the canonical chain we have to reproduce
// the same bug.
//
// Mirrors libraries/chain/database_witness.cpp:146-173.
func retallyWitnessVoteCountsForProxiedOnly(s *state.Session) {
	type acctRec struct {
		name     string
		hasProxy bool
		curCount uint16
	}
	var accounts []acctRec
	_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		accounts = append(accounts, acctRec{
			name:     a.Name,
			hasProxy: a.Proxy != "",
			curCount: a.WitnessesVotedFor,
		})
		return true, nil
	})
	for _, ar := range accounts {
		a, err := objects.GetAccount(s, ar.name)
		if err != nil {
			continue
		}
		var newCount uint16
		if ar.hasProxy {
			newCount = uint16(objects.EnsureWitnessVoteStore(s).CountFor(ar.name))
		}
		// Mirror C++ bug: even non-proxy accounts get rewritten to
		// the local newCount (= 0), which differs from a stored
		// non-zero value, so they get zeroed.
		if a.WitnessesVotedFor != newCount {
			a.WitnessesVotedFor = newCount
			objects.PutAccount(s, &a)
		}
	}
}

// retallyWitnessVoteCounts mirrors
// database::retally_witness_vote_counts (database_witness.cpp:99): zero
// every account's WitnessesVotedFor, then re-walk every witness_vote
// row to recount how many votes each account currently holds. Called at
// HF6 and HF8 to recover from prior bugs.
func retallyWitnessVoteCounts(s *state.Session) {
	type acctRec struct{ name string }
	var accounts []acctRec
	_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		accounts = append(accounts, acctRec{name: a.Name})
		return true, nil
	})
	for _, ar := range accounts {
		a, err := objects.GetAccount(s, ar.name)
		if err != nil {
			continue
		}
		a.WitnessesVotedFor = 0
		objects.PutAccount(s, &a)
	}

	_ = objects.EnsureWitnessVoteStore(s).RangeAllVoters(func(voter auth.AccountName, witnesses []auth.AccountName) (bool, error) {
		a, err := objects.GetAccount(s, voter)
		if err != nil {
			return true, nil
		}
		a.WitnessesVotedFor += uint16(len(witnesses))
		objects.PutAccount(s, &a)
		return true, nil
	})
}

// truncateFeedHistoryWindow shrinks fho.price_history to at most n
// entries, preserving the most-recent values. HF16 cuts the window from
// 168 (24×7 hourly) to 84 (12×7).
func truncateFeedHistoryWindow(s *state.Session, n int) {
	fh, err := objects.GetFeedHistory(s)
	if err != nil {
		return
	}
	if len(fh.PriceHistory) > n {
		fh.PriceHistory = fh.PriceHistory[len(fh.PriceHistory)-n:]
		objects.PutFeedHistory(s, &fh)
	}
}
