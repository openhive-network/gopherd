package evaluator

import (
	"fmt"
	"math"
	"math/bits"
	"os"
	"strings"

	"hived-go/internal/protocol/auth"
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
	if v == "" || v == "1" || !strings.ContainsRune(v, ',') {
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
//   - account.VestingShares += new_vesting (or RewardVests if toReward)
//   - dgpo.TotalVestingShares += new_vesting
//   - dgpo.TotalVestingFundHive += hiveSatoshi
//   - witness votes get +new_vesting via AdjustProxiedWitnessVotes
//     (only when toReward is false — reward vests count when claimed,
//     not when accumulated)
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
	dgpo, err := objects.GetDGPO(s)
	if err != nil && err != state.ErrNotFound {
		return 0, err
	}

	var vestsAmount int64
	if dgpo.TotalVestingFundHive == 0 || dgpo.TotalVestingShares == 0 {
		vestsAmount = hiveSatoshi * 1000
	} else {
		// vests = hive * total_vesting_shares / total_vesting_fund_hive.
		// Both inputs are non-negative int64. Product fits in uint128;
		// quotient back to int64 (mirrors the old IsInt64 saturation).
		// Native uint128 replaces a math/big triple-alloc that fired on
		// every CreateVesting call (~1.78 GB cum / 4M-block run).
		hi, lo := bits.Mul64(uint64(hiveSatoshi), uint64(dgpo.TotalVestingShares))
		d := uint64(dgpo.TotalVestingFundHive)
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

	// MutateAccount: in-place mutate via slot pointer, captures snap only
	// under UndoEnabled. Saves the GetAccount value-copy (~3.43s flat on
	// the hot path, the largest CreateVesting cost beneath
	// AdjustProxiedWitnessVotes).
	var aSlot *objects.Account
	if !objects.MutateAccount(s, auth.AccountName(accountName), func(a *objects.Account) {
		if toReward {
			a.RewardVests += vestsAmount
		} else {
			a.VestingShares += vestsAmount
		}
		aSlot = a
	}) {
		return 0, fmt.Errorf("createVesting: account %q not found", accountName)
	}

	dgpo.TotalVestingShares += vestsAmount
	dgpo.TotalVestingFundHive += hiveSatoshi
	objects.PutDGPO(s, &dgpo)
	traceAll := envGhivediffAllVest != ""
	if vestTraceAccount(accountName) || (traceAll && dgpo.HeadBlockNumber >= 2888500 && dgpo.HeadBlockNumber <= 2910000) {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF VEST-CREATE block=%d acct=%s liquid=%d new_vests=%d after_vests=%d fund_pre=%d shares_pre=%d toReward=%v\n",
			dgpo.HeadBlockNumber, accountName, hiveSatoshi, vestsAmount, aSlot.VestingShares, dgpo.TotalVestingFundHive-hiveSatoshi, dgpo.TotalVestingShares-vestsAmount, toReward)
	}

	// C++ create_vesting2 calls adjust_proxied_witness_votes when
	// !to_reward_balance. aSlot points at the post-mutation slot so we
	// pass it directly — saves one AccountStore.Get per CreateVesting.
	if adjustVotes && !toReward && vestsAmount != 0 {
		objects.AdjustProxiedWitnessVotes(s, aSlot, vestsAmount)
	}
	return vestsAmount, nil
}
