package evaluator

import (
	"fmt"
	"math"
	"math/bits"
	"os"
	"strings"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// envGhivediff is the snapshot of GHIVEDIFF at process start. Cached
// to avoid an os.Getenv syscall (~150ns) on the every-vest hot path —
// fires from every CreateVesting call (cashout-cascade + producer
// reward + delegations). Production replays leave GHIVEDIFF unset.
var (
	envGhivediff        = os.Getenv("GHIVEDIFF")
	envGhivediffAllVest = os.Getenv("GHIVEDIFF_ALL_VEST")
	envGhivediffPowbump = os.Getenv("GHIVEDIFF_POWBUMP")
	envGhivediffSimpleV = envGhivediff != "" && (envGhivediff == "1" || (!strings.ContainsRune(envGhivediff, ',') && len(envGhivediff) <= 2))
	envGhivediffNamesV  = splitTraceNames(envGhivediff)
)

func splitTraceNames(v string) []string {
	if v == "" || v == "1" {
		return nil
	}
	return strings.Split(v, ",")
}

// vestTraceAccount mirrors chain.traceAccount for the evaluator package
// (which can't import chain). Comma-separated account list in GHIVEDIFF
// names the accounts to trace; "1" or unset → default set.
func vestTraceAccount(name string) bool {
	if envGhivediff == "" {
		return false
	}
	if envGhivediffSimpleV {
		switch name {
		case "hipster", "proskynneo", "abit", "pfunk":
			return true
		}
		return false
	}
	for _, n := range envGhivediffNamesV {
		if n == name {
			return true
		}
	}
	return false
}

// CreateVesting converts hiveSatoshi liquid HIVE into VESTS for the
// given account, mirroring hived's create_vesting2 (database.cpp:1048).
//
// Conversion price (libraries/chain/include/hive/chain/global_property_object.hpp
// vesting_share_price()):
//
//	if (total_vesting_fund_hive == 0 || total_vesting_shares == 0)
//	    price = VEST_price(1_000_000 VESTS, 1_000 HIVE)        // initial 1:1
//	else
//	    price = VEST_price(total_vesting_shares, total_vesting_fund_hive)
//
//	new_vesting = (hive_amount * shares / fund)   when fund>0,shares>0
//
// At satoshi precision the initial price gives 1 HIVE-satoshi → 1000
// VESTS-satoshi. After HF1 (the ×1,000,000 vesting share split) the
// dynamic ratio quickly diverges from 1:1000; a fixed 1:1000 understates
// new VESTS by ~1000x relative to hived, starving every later payout
// that reads the resulting state.
//
// Side effects (matches C++ create_vesting2):
//   - !toReward (regular path):
//       account.VestingShares          += new_vesting
//       dgpo.TotalVestingShares        += new_vesting
//       dgpo.TotalVestingFundHive      += hiveSatoshi
//       witness votes                  += new_vesting via AdjustProxiedWitnessVotes
//   - toReward (HF17+ comment payout into pending reward pool):
//       account.RewardVests            += new_vesting
//       account.RewardVestingHive      += hiveSatoshi
//       dgpo.PendingRewardedVestingShares += new_vesting
//       dgpo.PendingRewardedVestingHive   += hiveSatoshi
//       (no witness vote adjust — reward vests vote only after claim)
//
// The vesting share price diverges on the toReward branch
// (database_vesting_helpers.hpp:calculate_vesting):
//
//	!toReward → vests = liquid × total_vesting_shares / total_vesting_fund_hive
//	 toReward → vests = liquid × (total_vesting_shares + pending_rewarded_vesting_shares)
//	                            / (total_vesting_fund_hive + pending_rewarded_vesting_hive)
//
// Without the per-pool bookkeeping the toReward path mutates
// total_vesting_* directly, which silently inflates the regular vesting
// share denominator — surfaces as the HF17_659 `claim_reward_balance:
// insufficient balance` mismatch first hit at block 10,635,691 (account
// "ocrdu", ~1.1% under-credit).
//
// Returns the VESTS-satoshi amount credited.
func CreateVesting(s *state.Session, accountName string, hiveSatoshi int64, toReward bool) (int64, error) {
	return createVestingImpl(s, accountName, hiveSatoshi, toReward, true)
}

// AdjustAccountVestingBalance mirrors hived's adjust_account_vesting_balance
// helper — does the balance/dgpo update WITHOUT the witness vote
// adjustment. Used by the HF24+ transfer_to_vesting path where the
// vest is queued in delayed_votes instead of crediting witness totals
// immediately.
func AdjustAccountVestingBalance(s *state.Session, accountName string, hiveSatoshi int64, toReward bool) (int64, error) {
	return createVestingImpl(s, accountName, hiveSatoshi, toReward, false)
}

func createVestingImpl(s *state.Session, accountName string, hiveSatoshi int64, toReward, adjustVotes bool) (int64, error) {
	dgpoRef, ok := objects.GetDGPORef(s)
	if !ok {
		// Singleton not yet populated — defer to the legacy zero path.
	}

	var vestsAmount int64
	var totalVestingFundHive, totalVestingShares int64
	if dgpoRef != nil {
		totalVestingFundHive = dgpoRef.TotalVestingFundHive
		totalVestingShares = dgpoRef.TotalVestingShares
		// HF17+: toReward credits use the reward-pool share price
		// (database_vesting_helpers.hpp:calculate_vesting). Sum the
		// pending pool into both legs so a credit that races with
		// pre-claim pending balances stays at the same price as hived.
		if toReward {
			totalVestingShares += dgpoRef.PendingRewardedVestingShares
			totalVestingFundHive += dgpoRef.PendingRewardedVestingHive
		}
	}
	if totalVestingFundHive == 0 || totalVestingShares == 0 {
		vestsAmount = hiveSatoshi * 1000
	} else {
		// vests = hive * shares / fund.
		// Both inputs are non-negative int64. Product fits in uint128;
		// quotient back to int64 (mirrors the old IsInt64 saturation).
		// Native uint128 replaces a math/big triple-alloc that fired on
		// every CreateVesting call (~1.78 GB cum / 4M-block run).
		hi, lo := bits.Mul64(uint64(hiveSatoshi), uint64(totalVestingShares))
		d := uint64(totalVestingFundHive)
		if hi >= d {
			vestsAmount = 0
		} else {
			q, _ := bits.Div64(hi, lo, d)
			if q > math.MaxInt64 {
				vestsAmount = 0
			} else {
				vestsAmount = int64(q)
			}
		}
	}

	// HF20+ non-reward credits bump the voting manabar by the new vests,
	// with the regen cap taken from PRE-credit effective shares — so the
	// update runs inside the mutate closure before the balance change
	// (mirrors adjust_account_vesting_balance, database.cpp:1028-1031).
	manabarHF20 := false
	var dvPoolPct uint16
	if !toReward && dgpoRef != nil {
		if hardfork.SetForBlock(dgpoRef.HeadBlockNumber).Has(hardfork.HF20) {
			manabarHF20 = true
			dvPoolPct = dgpoRef.DownvotePoolPercent
		}
	}

	// MutateAccount: in-place mutate via slot pointer, captures snap only
	// under UndoEnabled. Saves the GetAccount value-copy (~3.43s flat on
	// the hot path, the largest CreateVesting cost beneath
	// AdjustProxiedWitnessVotes).
	var aSlot *objects.Account
	if !objects.MutateAccount(s, auth.AccountName(accountName), func(a *objects.Account) {
		if toReward {
			a.RewardVests += vestsAmount
			a.RewardVestingHive += hiveSatoshi
		} else {
			if manabarHF20 {
				objects.UpdateVotingManabar(a, dgpoRef.Time, dvPoolPct, vestsAmount, dgpoRef.HeadBlockNumber)
			}
			a.VestingShares += vestsAmount
		}
		aSlot = a
	}) {
		return 0, fmt.Errorf("createVesting: account %q not found", accountName)
	}

	objects.MutateDGPO(s, func(p *objects.DynamicGlobalProperty) {
		if toReward {
			p.PendingRewardedVestingShares += vestsAmount
			p.PendingRewardedVestingHive += hiveSatoshi
		} else {
			p.TotalVestingShares += vestsAmount
			p.TotalVestingFundHive += hiveSatoshi
		}
	})
	traceAll := envGhivediffAllVest != ""
	if dgpoRef != nil && (vestTraceAccount(accountName) || (traceAll && dgpoRef.HeadBlockNumber >= 2888500 && dgpoRef.HeadBlockNumber <= 2910000)) {
		// Note: TotalVesting{Fund,Shares} reflect post-mutate state, so
		// subtract the credit to back out pre-state. For toReward we
		// only mutated Pending*, so the subtraction is a no-op there.
		preTF := dgpoRef.TotalVestingFundHive
		preTS := dgpoRef.TotalVestingShares
		prePH := dgpoRef.PendingRewardedVestingHive
		prePS := dgpoRef.PendingRewardedVestingShares
		if toReward {
			prePH -= hiveSatoshi
			prePS -= vestsAmount
		} else {
			preTF -= hiveSatoshi
			preTS -= vestsAmount
		}
		fmt.Fprintf(os.Stderr, "GHIVEDIFF VEST-CREATE block=%d acct=%s liquid=%d new_vests=%d after_vests=%d fund_pre=%d shares_pre=%d pending_hive_pre=%d pending_shares_pre=%d toReward=%v\n",
			dgpoRef.HeadBlockNumber, accountName, hiveSatoshi, vestsAmount, aSlot.VestingShares, preTF, preTS, prePH, prePS, toReward)
	}

	// C++ create_vesting2 calls adjust_proxied_witness_votes when
	// !to_reward_balance. aSlot points at the post-mutation slot so we
	// pass it directly — saves one AccountStore.Get per CreateVesting.
	// Unconditional on the amount, like C++ — 0 new vests still re-rolls
	// the recipient's voted witnesses' virtual scheduling state.
	if adjustVotes && !toReward {
		objects.AdjustProxiedWitnessVotes(s, aSlot, vestsAmount)
	}
	return vestsAmount, nil
}
