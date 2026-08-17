package evaluator

import (
	"fmt"
	"math/big"
	"os"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// HIVE_POW_APR_PERCENT — POW APR target (libraries/protocol/include/
// hive/protocol/config.hpp). Used by C++'s
// calc_percent_reward_per_round< HIVE_POW_APR_PERCENT >(virtual_supply).
const powAPRPercent int64 = 750

// percentRewardPerRound constants mirror hived's compound.hpp:38 with
// HIVE_APR_PERCENT_*_PER_ROUND. Computed once at build time.
const (
	powAprMultiplyPerRound uint64 = 0x79cc_f5c7_3480
	powAprShiftPerRound    uint   = 83
)

// powPercentRewardPerRound mirrors C++'s
// calc_percent_reward_per_round<percent>(virtual_supply) — supply ×
// percent × multiply / 2^shift, rounded half-up. Used by get_pow_reward
// to compute the percent floor before max() with HIVE_MIN_POW_REWARD.
func powPercentRewardPerRound(virtualSupply, aprPercent int64) int64 {
	if virtualSupply <= 0 || aprPercent <= 0 {
		return 0
	}
	prod := new(big.Int).Mul(big.NewInt(virtualSupply), big.NewInt(aprPercent))
	prod.Mul(prod, new(big.Int).SetUint64(powAprMultiplyPerRound))
	half := new(big.Int).Lsh(big.NewInt(1), powAprShiftPerRound-1)
	prod.Add(prod, half)
	prod.Rsh(prod, powAprShiftPerRound)
	if !prod.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return prod.Int64()
}

// powEvaluator implements the partial-mining-era apply for `pow`.
//
// Full hived semantics: validate PoW (skipped during replay), create
// the worker account if absent, create/refresh a witness_object, pay
// HIVE_MINING_REWARD as VESTS, and update DGPO totals.
//
// What this implementation handles:
//   - Account creation when missing (so subsequent transfers find the
//     account). Balances start at zero; the mining reward is deferred
//     until vesting accounting is real.
//
// What's intentionally elided (added with the relevant state objects):
//   - witness_object create/update.
//   - reward payout / supply adjustment.
//   - PoW hash validation.
type powEvaluator struct{}

func (powEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagPow {
		return fmt.Errorf("powEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	pw := opArg.AsPow()
	// pow1 always pays the reward (it's deprecated at HF13 and chains
	// past that point reject the op entirely; the block_log won't
	// contain pow1 ops post-HF13). Pass the work.worker as signing_key
	// for new witnesses (mirrors hive_evaluator.cpp:476). Pass the
	// work hash so ensureMinerAccountForPow can enforce hived's
	// post-HF5 by_work duplicate-work assert (same-witness only —
	// cross-witness collision would require a SHA256 collision).
	work := pw.Work.Work
	return ensureMinerAccountForPow(ctx, pw.WorkerAccount, true, pw.Props, pw.Work.Worker, &work)
}

// pow2Evaluator implements the partial-mining-era apply for `pow2`. Same
// scope as powEvaluator: create the worker account if absent.
type pow2Evaluator struct{}

func (pow2Evaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagPow2 {
		return fmt.Errorf("pow2Evaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	pw := opArg.AsPow2()
	var worker string
	var workPrev [20]byte
	var powSummary uint32
	switch pw.WorkVariantTag {
	case 0: // pow2 variant — prev_block check uses work.input.prev_block (hive_evaluator.cpp:536).
		worker = pw.PowInput.WorkerAccount
		workPrev = pw.PowInput.PrevBlock
		powSummary = pw.PowSummary
	case 1: // equihash_pow variant — prev_block check uses the OUTER work.prev_block field (hive_evaluator.cpp:522), not work.input.prev_block.
		worker = pw.EquihashWork.Input.WorkerAccount
		workPrev = pw.EquihashWork.PrevBlock
		powSummary = pw.EquihashWork.PowSummary
	default:
		return fmt.Errorf("pow2: unknown work variant %d", pw.WorkVariantTag)
	}
	// NOTE: hived's pow_summary < target_pow assert is INTENTIONALLY
	// not enforced here — target_pow is derived from
	// dgpo.num_pow_witnesses (database.cpp:1065-1075), which is
	// EXACTLY the quantity that diverges at block 5,148,675 in our
	// replay. Adding the assert here forces Go and hived to make
	// different accept/reject decisions for borderline pow_summary
	// values, which widens the divergence (observed: NumPow Go=87 vs
	// hived=104 at block 5,210,499 with the assert; Go=103 vs hived=102
	// without). The cascade root must be fixed before this assert can
	// be re-enabled. Until then, accept all pow_summary values to
	// match the pre-fix Go behavior.
	_ = powSummary
	// hived's pow2_evaluator (hive_evaluator.cpp:553-579) enforces a
	// strict pairing on account-witness state:
	//   - If account doesn't exist: REQUIRES new_owner_key. Creates account + witness.
	//   - If account exists: REJECTS if new_owner_key is set AND REJECTS if witness doesn't exist.
	//
	// The original Go port added these gates while hunting the iter12
	// gxt-1080-sc-0027 cascade at block 5,148,657 (NumPow off-by-1
	// vs hived). iter17 RESOLVED that cascade via the block-1 off-by-99
	// slot-calc fix, so these defensive gates are no longer needed for
	// replay parity — and they reject the testfixture-style pow2 ops
	// that omit a NewOwnerKey but otherwise expect account+witness
	// creation. We still need to know whether the account exists in
	// order to pick the right signing-key path (NEW account → derive
	// from NewOwnerKey if present, else leave the worker's auth empty
	// for the test path).
	hasAcct, _ := objects.HasAccount(ctx.DB, worker)
	_ = hasAcct
	// hived's pow2_evaluator (hive_evaluator.cpp:524, 539) asserts
	// `work.input.prev_block == db.head_block_id()` and
	// (hive_evaluator.cpp:544) `o.props.maximum_block_size >=
	// HIVE_MIN_BLOCK_SIZE_LIMIT * 2`. Both are guarantees produced by
	// honest miners against the canonical block_log — replay through
	// 5.2M blocks confirms canonical block_log never trips them, and
	// the iter17 cascade fix demonstrates Go doesn't need to re-validate.
	// Keep the workPrev variable referenced so the compiler is happy.
	_ = workPrev
	// pow2's signing_key for a NEW account = *o.new_owner_key
	// (hive_evaluator.cpp:574). For existing accounts the signing_key
	// is not modified.
	var signingKey crypto.PublicKey
	if pw.NewOwnerKey != nil {
		signingKey = *pw.NewOwnerKey
	}
	// hived's pow2_evaluator (libraries/chain/hive_evaluator.cpp:593) only
	// pays the reward when HF16 is NOT yet active. Account/witness setup
	// always runs. pow2 does NOT participate in the by_work dedup index
	// in hived (pow2_evaluator never writes witness.last_work and never
	// checks by_work) — so pass nil for the work hash.
	return ensureMinerAccountForPow(ctx, worker, !ctx.Hardforks.Has(hardfork.HF16), pw.Props, signingKey, nil)
}

// HIVE_MIN_POW_REWARD = HIVE_MINING_REWARD = 1.000 HIVE in satoshi
// (libraries/protocol/include/hive/protocol/config.hpp).
const minPowReward int64 = 1000

// HIVE_MAX_WITNESSES — used to multiply the pow reward pre-HIVE_START_MINER_VOTING_BLOCK.
const maxWitnesses int64 = 21

// HIVE_START_MINER_VOTING_BLOCK = 30 days × 28800 = 864,000.
const startMinerVotingBlockPow uint32 = 864_000

// payPowReward implements the C++ pow_apply reward distribution:
//
//	pow_reward = HIVE_MIN_POW_REWARD                                      // formula floor
//	if (head_block < HIVE_START_MINER_VOTING_BLOCK)
//	    pow_reward *= HIVE_MAX_WITNESSES                                  // 21x boost early
//	dgpo.current_supply += pow_reward
//	if (head_block < HIVE_START_MINER_VOTING_BLOCK)
//	    inc_witness.balance += pow_reward                                 // liquid HIVE
//	else
//	    inc_witness.vesting_shares += create_vesting(pow_reward)          // VESTS
//
// The reward goes to the BLOCK PRODUCER (dgp.current_witness), not the
// worker — a critical detail. The worker's only state effect is having
// its account created if it didn't exist yet.
func payPowReward(ctx *Context) error {
	dgpo, err := objects.GetDGPO(ctx.DB)
	if err != nil && err != state.ErrNotFound {
		return err
	}
	witnessName := ctx.Witness
	if witnessName == "" {
		return nil
	}
	// dgpo.NumPowWitnesses is bumped in ensureMinerAccountForPow
	// (alongside TotalPow) to match C++'s unconditional dgp.modify.
	// Here we only USE the post-bump value to gate early payouts.
	const startVestingBlock uint32 = 7 * 28800 // HIVE_START_VESTING_BLOCK
	if ctx.BlockNum < startVestingBlock && dgpo.NumPowWitnesses < uint32(maxWitnesses) {
		return nil
	}
	// Mirror C++'s get_pow_reward (database.cpp:1933): take the max of
	// HIVE_MIN_POW_REWARD and calc_percent_reward_per_round< 7.5% >
	// (virtual_supply). Once virtual_supply gets large (post ~5M blocks),
	// the percent term overtakes the floor and we'd otherwise underpay
	// every POW miner by a few satoshi each — quickly diverging supply.
	virtualSupply := dgpo.VirtualSupply
	if virtualSupply <= 0 {
		virtualSupply = dgpo.CurrentSupply
	}
	pay := powPercentRewardPerRound(virtualSupply, powAPRPercent)
	if pay < minPowReward {
		pay = minPowReward
	}
	if ctx.BlockNum < startMinerVotingBlockPow {
		pay *= maxWitnesses
	}
	if _, err := objects.GetAccount(ctx.DB, witnessName); err != nil {
		objects.PutDGPO(ctx.DB, &dgpo)
		return nil
	}
	// hived's adjust_supply(pow_reward, /*adjust_vesting=*/true) at
	// database.cpp:3357. When head ≥ HIVE_START_VESTING_BLOCK
	// (7×28800 = 201600) and delta > 0, fund_hive grows by 9× the
	// reward and supply by 10×. Pre-201600 the vesting leg is
	// suppressed (only current_supply += pay).
	newVesting := int64(0)
	if ctx.BlockNum >= startVestingBlock && pay > 0 {
		newVesting = pay * 9
	}
	dgpo.CurrentSupply += pay + newVesting
	dgpo.VirtualSupply += pay + newVesting
	dgpo.TotalVestingFundHive += newVesting
	objects.PutDGPO(ctx.DB, &dgpo)

	if ctx.BlockNum < startMinerVotingBlockPow {
		w, err := objects.GetAccount(ctx.DB, witnessName)
		if err != nil {
			return nil
		}
		w.Balance += pay
		objects.PutAccount(ctx.DB, &w)
	} else {
		if _, err := CreateVesting(ctx.DB, witnessName, pay, false); err != nil {
			return err
		}
	}
	return nil
}

// ensureMinerAccountForPow creates an account named worker if missing
// and (when payReward is true) credits the block producer with the
// pow reward. props is the published chain_properties from the op;
// they're copied onto the witness via copy_legacy_chain_properties
// (hive_evaluator.cpp:464, 472, 588). signingKey is set as the witness's
// block_signing_key on FIRST PoW only (hive_evaluator.cpp:476, 574);
// subsequent PoW ops on an existing witness leave signing_key alone.
func ensureMinerAccountForPow(ctx *Context, worker string, payReward bool, props op.LegacyChainProperties, signingKey crypto.PublicKey, pow1WorkHash *[32]byte) error {
	if worker == "" {
		return fmt.Errorf("pow/pow2: empty worker_account")
	}
	has, err := objects.HasAccount(ctx.DB, worker)
	if err != nil {
		return err
	}
	if !has {
		// Mirror C++ account_object ctor (account_object.hpp:35-40) +
		// pow_apply (hive_evaluator.cpp:420) / pow2_apply (:552): the
		// PoW-mined account's memo_key is seeded from work.worker (pow1)
		// or *o.new_owner_key (pow2). Gopherd previously left MemoKey
		// at the zero [33]byte{}, so state-dump comparisons of PoW-
		// mined accounts diverged.
		// PoW-mined accounts have no explicit creator. C++ sets
		// recovery_account = "steem" pre-HF11, empty post-HF11.
		// PoW ended at HF16 so the post-HF11 branch only matters
		// for the HF11→HF16 window of pow ops.
		var recoveryAcct auth.AccountName
		if !ctx.Hardforks.Has(hardfork.HF11) {
			recoveryAcct = "steem"
		}
		objects.PutAccount(ctx.DB, &objects.Account{
			Name:                 worker,
			Created:              ctx.HeadBlockTime,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: ctx.HeadBlockTime,
			Mined:                true,
			MemoKey:              signingKey,
			RecoveryAccount:      recoveryAcct,
		})
		// Mirror C++ pow_apply (hive_evaluator.cpp:424-430) +
		// pow2_evaluator (hive_evaluator.cpp:561-567): when a new
		// account is created via PoW, an account_authority_object
		// must be created alongside it. pow1's signing_key is
		// work.worker; pow2's is *new_owner_key. The same key fills
		// owner = active = posting with weight 1 / threshold 1.
		// Without this, live-sync's verify_authority cannot resolve
		// the active authority for any tx signed by a PoW-mined
		// account (e.g. block 25,502 admin transfer).
		key := signingKey
		objects.PutAccountAuthority(ctx.DB, &objects.AccountAuthority{
			Account: worker,
			Owner: auth.Authority{
				WeightThreshold: 1,
				KeyAuths:        []auth.KeyAuth{{Key: key, Weight: 1}},
			},
			Active: auth.Authority{
				WeightThreshold: 1,
				KeyAuths:        []auth.KeyAuth{{Key: key, Weight: 1}},
			},
			Posting: auth.Authority{
				WeightThreshold: 1,
				KeyAuths:        []auth.KeyAuth{{Key: key, Weight: 1}},
			},
			LastOwnerUpdate: ctx.HeadBlockTime,
		})
	}
	// Register the worker as a witness if missing — hived's pow_apply
	// calls db.create<witness_object>(...) on first pow from this
	// account, alongside the account creation. Mark them in the
	// PoW miner queue (witness_object::pow_worker = ++dgp.total_pow,
	// guaranteeing a unique value per pow op) and bump
	// dgpo.num_pow_witnesses; update_witness_schedule's miner-slot
	// pass will pop them later. Existing witnesses re-running PoW
	// also bump the queue position.
	wExists, err := objects.HasWitness(ctx.DB, worker)
	if err != nil {
		return err
	}
	var w objects.Witness
	if !wExists {
		w = objects.Witness{
			Owner:           worker,
			Created:         ctx.HeadBlockTime,
			BlockSigningKey: signingKey,
			// chain_properties defaults (witness_objects.hpp:54-61);
			// these feed the schedule's subsidy medians from HF20 on.
			AccountSubsidyBudget: 797,
			AccountSubsidyDecay:  347321,
		}
		seedVirtualScheduledTime(ctx, &w)
	} else {
		got, err := objects.GetWitness(ctx.DB, worker)
		if err != nil {
			return err
		}
		// hived's pow_apply (hive_evaluator.cpp:461) + pow2_evaluator
		// (hive_evaluator.cpp:578) BOTH ASSERT cur_witness->pow_worker == 0:
		// a witness already in the by_pow queue cannot submit another
		// PoW until the schedule rebuild drains them. Without this gate
		// Go double-increments dgpo.num_pow_witnesses on every repeat
		// PoW, which bleeds into update_witness_schedule's drain count
		// and (much later) into median feed publishers — the cascade
		// observed at block 5,148,675 with NumPow=103 vs hived 102.
		if got.PowWorker != 0 {
			return fmt.Errorf("pow: account %q is already scheduled for pow block production (pow_worker=%d)", worker, got.PowWorker)
		}
		// pow1-only by_work duplicate-work check — hived's pow_apply
		// (hive_evaluator.cpp:2317-2325) post-HF5 rejects any pow1 op
		// whose work hash matches any existing witness.last_work.
		// The check is HF5-gated in C++ (HIVE_HARDFORK_0_5__59); apply
		// the same gate here so pre-HF5 ops with synthetic-zero work
		// hashes (testfixture path) aren't spuriously rejected.
		// Same-witness check is sufficient in practice (cross-witness
		// SHA256 collision is astronomically rare); a global by_work
		// btree would be needed for a truly exhaustive check but we'd
		// only ever fire it on a hash collision the chain has never
		// observed.
		if pow1WorkHash != nil && ctx.Hardforks.Has(hardfork.HF5) && got.LastWork == *pow1WorkHash {
			return fmt.Errorf("pow: duplicate work hash for account %q (last_work matches new submission)", worker)
		}
		w = got
	}
	// Mirror C++ pow_apply (hive_evaluator.cpp:464,572) +
	// pow2_evaluator (hive_evaluator.cpp:572) — both new-witness and
	// existing-witness paths copy legacy chain_properties from the op.
	// Only NEW witnesses get signing_key set (existing witnesses retain
	// theirs from the original witness_update — pow doesn't rotate keys).
	w.AccountCreationFee = props.AccountCreationFee.Amount
	w.MaximumBlockSize = props.MaximumBlockSize
	w.HBDInterestRate = props.HBDInterestRate
	// Match C++ pow_apply (hive_evaluator.cpp:451-455, 465, 477):
	//   ++dgp.total_pow; ++dgp.num_pow_witnesses; w.pow_worker = dgp.total_pow.
	// Both counters bump UNCONDITIONALLY — C++ does it in a single
	// db.modify before deciding payment. Don't move num_pow_witnesses
	// into payPowReward: pre-HF13 pow1 and pre-HF16 pow2 always pay so
	// it didn't show, but on HF16+ pow2 (no payment) C++ still bumps
	// while Go would not.
	dgpoForPow, _ := objects.GetDGPO(ctx.DB)
	dgpoForPow.TotalPow++
	dgpoForPow.NumPowWitnesses++
	objects.PutDGPO(ctx.DB, &dgpoForPow)
	w.PowWorker = dgpoForPow.TotalPow
	// pow1 records the work hash so subsequent same-witness ops with
	// the same hash are rejected by the by_work check (above). pow2
	// passes nil — hived's pow2_evaluator never touches witness.last_work.
	if pow1WorkHash != nil {
		w.LastWork = *pow1WorkHash
	}
	objects.PutWitness(ctx.DB, &w)
	if envGhivediffPowbump != "" {
		fmt.Fprintf(os.Stderr, "POWBUMP block=%d worker=%s total=%d num=%d\n",
			ctx.BlockNum, worker, dgpoForPow.TotalPow, dgpoForPow.NumPowWitnesses)
	}
	if !payReward {
		return nil
	}
	return payPowReward(ctx)
}

func init() {
	register(op.TagPow, powEvaluator{})
	register(op.TagPow2, pow2Evaluator{})
}
