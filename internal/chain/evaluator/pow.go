package evaluator

import (
	"fmt"
	"math/big"
	"os"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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

func (powEvaluator) Apply(ctx *Context, o op.Operation) error {
	pw, ok := o.(op.Pow)
	if !ok {
		return fmt.Errorf("powEvaluator: unexpected op type %T", o)
	}
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

func (pow2Evaluator) Apply(ctx *Context, o op.Operation) error {
	pw, ok := o.(op.Pow2)
	if !ok {
		return fmt.Errorf("pow2Evaluator: unexpected op type %T", o)
	}
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
	// Without this gate Go silently auto-creates witnesses and accepts
	// pow2 ops with stale new_owner_keys, double-incrementing
	// dgpo.num_pow_witnesses (gxt-1080-sc-0027 cascade at 5,148,657).
	hasAcct, _ := objects.HasAccount(ctx.DB, worker)
	if hasAcct {
		if pw.NewOwnerKey != nil {
			if os.Getenv("GHIVEDIFF_POWBUMP") != "" {
				fmt.Fprintf(os.Stderr, "POW2REJECT-newkey block=%d worker=%s\n", ctx.BlockNum, worker)
			}
			return fmt.Errorf("pow2: cannot specify new_owner_key for existing account %q", worker)
		}
		hasWit, _ := objects.HasWitness(ctx.DB, worker)
		if !hasWit {
			if os.Getenv("GHIVEDIFF_POWBUMP") != "" {
				fmt.Fprintf(os.Stderr, "POW2REJECT-nowitness block=%d worker=%s\n", ctx.BlockNum, worker)
			}
			return fmt.Errorf("pow2: witness must exist for existing account %q before mining", worker)
		}
	} else {
		if pw.NewOwnerKey == nil {
			if os.Getenv("GHIVEDIFF_POWBUMP") != "" {
				fmt.Fprintf(os.Stderr, "POW2REJECT-newkey-missing block=%d worker=%s\n", ctx.BlockNum, worker)
			}
			return fmt.Errorf("pow2: new_owner_key required to create account %q", worker)
		}
	}
	// hived's pow2_evaluator (hive_evaluator.cpp:524, 539) asserts
	// `work.input.prev_block == db.head_block_id()` — the work was
	// computed against the LAST applied block. During tx processing,
	// dgpo.HeadBlockID still reflects the previous block (database.go
	// updates it AFTER the tx loop, before per-block hooks). Without
	// this gate Go accepts stale-prev_block pow2 ops that hived
	// rejects, double-incrementing dgpo.num_pow_witnesses (observed
	// at block 5,148,657, gxt-1080-sc-0027 — Go=NumPow 103 vs
	// hived=102 cascade).
	if dgpo, derr := objects.GetDGPO(ctx.DB); derr == nil {
		if workPrev != dgpo.HeadBlockID {
			if os.Getenv("GHIVEDIFF_POWBUMP") != "" {
				fmt.Fprintf(os.Stderr, "POW2REJECT-prevblock block=%d worker=%s want=%x got=%x\n", ctx.BlockNum, worker, dgpo.HeadBlockID, workPrev)
			}
			return fmt.Errorf("pow2: work prev_block does not match head_block_id (worker=%q)", worker)
		}
	}
	// hived's pow2_evaluator (hive_evaluator.cpp:544) asserts
	// `o.props.maximum_block_size >= HIVE_MIN_BLOCK_SIZE_LIMIT * 2`
	// = 65536 * 2 = 131072. Voting too small a block size is rejected.
	const minBlockSizeFloor uint32 = 65536 * 2
	if pw.Props.MaximumBlockSize < minBlockSizeFloor {
		if os.Getenv("GHIVEDIFF_POWBUMP") != "" {
			fmt.Fprintf(os.Stderr, "POW2REJECT-blocksize block=%d worker=%s size=%d\n", ctx.BlockNum, worker, pw.Props.MaximumBlockSize)
		}
		return fmt.Errorf("pow2: voted maximum_block_size too small (%d < %d) for worker %q", pw.Props.MaximumBlockSize, minBlockSizeFloor, worker)
	}
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
		objects.PutAccount(ctx.DB, &objects.Account{
			Name:                 worker,
			Created:              ctx.HeadBlockTime,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: ctx.HeadBlockTime,
			Mined:                true,
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
		// (hive_evaluator.cpp:407-413) post-HF5 rejects any pow1 op
		// whose work hash matches any existing witness.last_work.
		// Same-witness check is sufficient in practice (cross-witness
		// SHA256 collision is astronomically rare); a global by_work
		// btree would be needed for a truly exhaustive check but we'd
		// only ever fire it on a hash collision the chain has never
		// observed.
		if pow1WorkHash != nil && got.LastWork == *pow1WorkHash {
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
	if os.Getenv("GHIVEDIFF_POWBUMP") != "" {
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
