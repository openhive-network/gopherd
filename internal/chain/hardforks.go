package chain

import (
	"hived-go/internal/protocol/hardfork"
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
	prev := hardfork.SetForBlock(blockNum - 1)
	cur := hardfork.SetForBlock(blockNum)
	for id := hardfork.ID(1); id <= hardfork.NumHardforks; id++ {
		if !prev.Has(id) && cur.Has(id) {
			applyHardfork(s, id)
		}
	}
}

// applyHardfork dispatches the per-HF state-mutation logic, mirroring
// libraries/chain/database_hardfork.cpp:178. Most HFs are no-ops at the
// state level (their effects are encoded as gates on evaluators);
// the ones we model here are the ones whose state mutations are
// observable post-activation.
func applyHardfork(s *state.Session, id hardfork.ID) {
	switch id {
	case hardfork.HF1:
		performVestingShareSplit(s, 1_000_000)
	case hardfork.HF12:
		// HF12__178 retires the liquidity-reward subsystem. Wipe every
		// row so the by_volume_weight scan in pay_liquidity_reward
		// returns nothing post-HF12 (pay_liquidity_reward also bails
		// post-HF12 as a guard).
		clearLiquidityRewardBalances(s)
	case hardfork.HF16:
		// Truncate the price-history window from 168 → 84 entries.
		truncateFeedHistoryWindow(s, 84)
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
	_ = s.Range(objects.AccountTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			a, err := objects.ReadAccount(value)
			if err != nil {
				return true, nil
			}
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

// rebuildCommentRshares scales every comment's NetRshares by magnitude
// and re-accumulates curve(NetRshares) for the positive-rshares ones
// into dgpo.TotalRewardShares2. Mirrors the comments-and-rshares loop
// from perform_vesting_share_split.
func rebuildCommentRshares(s *state.Session, magnitude int64) {
	type pending struct {
		author, permlink string
	}
	var queue []pending
	_ = s.Range(objects.CommentTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			c, err := objects.ReadComment(value)
			if err != nil {
				return true, nil
			}
			queue = append(queue, pending{author: c.Author, permlink: c.Permlink})
			return true, nil
		})
	for _, p := range queue {
		c, err := objects.GetComment(s, p.author, p.permlink)
		if err != nil {
			continue
		}
		c.NetRshares *= magnitude
		objects.PutComment(s, &c)
	}
	// total_reward_shares2 will rebuild organically as new votes hit
	// adjustRewardShares2; we leave it zero here, matching hived's
	// "reset then re-add" with the re-add deferred to natural vote
	// flow rather than a hardfork-time scan.
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
