// Package testfixture is the Go equivalent of hived's
// libraries/tests/unit/db_fixture/database_fixture — a lightweight
// scaffold for spinning up a fresh chain.Database in-memory, creating
// accounts, funding them, applying operations, and generating blocks.
//
// Test files use it like this:
//
//	func TestSomeOp(t *testing.T) {
//	    f := testfixture.New(t)
//	    f.Actor("alice")
//	    f.FundLiquid("alice", 1_000)  // 1.000 HIVE
//	    f.GenerateBlock()
//
//	    err := f.PushOp(op.Transfer{
//	        From:   "alice",
//	        To:     "initminer",
//	        Amount: testfixture.HIVE(500),
//	        Memo:   "hi",
//	    })
//	    require.NoError(t, err)
//	    f.GenerateBlock()
//
//	    require.Equal(t, int64(500), f.Balance("alice"))
//	}
//
// Mirrors the conveniences of hived's PUSH_OP / GENERATE_BLOCK / ACTOR
// / fund / vest macros. Signatures are skipped entirely (just like
// during ghive replay) — the focus is consensus state, not crypto.
package testfixture

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/chain/evaluator"
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// HIVE_BLOCK_INTERVAL.
const BlockInterval uint32 = 3

// Default initminer matches genesis.MainnetInitminer.
const InitMiner = "initminer"

// HIVE wraps a HIVE-satoshi int64 into an op.Asset with HIVE_SYMBOL.
// 1 HIVE = 1000 HIVE-satoshi.
func HIVE(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.HiveSymbol}}
}

// HBD wraps an HBD-satoshi amount.
func HBD(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.HbdSymbol}}
}

// VESTS wraps a VESTS-satoshi amount.
func VESTS(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.VestsSymbol}}
}

// LegacyHIVE returns a LegacyHiveAsset (used in witness_update's
// `props.account_creation_fee` field).
func LegacyHIVE(amount int64) asset.LegacyHiveAsset {
	// Canonical legacy hive symbol "STEEM" with precision 3.
	return asset.LegacyHiveAsset{
		Amount: amount,
		Sym:    []byte{0x03, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00},
	}
}

// Fixture is the test scaffold. Each test gets its own Fixture with a
// fresh state.Database; the temp dir is cleaned up via t.Cleanup.
type Fixture struct {
	t *testing.T

	State            *state.Database
	Chain            *chain.Database
	BlockNum         uint32 // last applied block number
	prevID           block.ID
	pending          []op.Op
	pendingSignedTxs []tx.SignedTransaction

	// curBlockTime tracks the timestamp of the next block to be
	// generated. Starts at network.GenesisTimeUnix + BlockInterval.
	curBlockTime uint32

	// EnforceRC, when true, enables ApplyOptions.EnforceRC on every
	// ApplyBlock call. Tests opt in to mirror hived's
	// rc-skip-reject-not-enough-rc=0 path (default-off in replay).
	EnforceRC bool

	// RequireSignatures, when true, flips ApplyOptions.SkipTransactionSignatures
	// to false on every ApplyBlock call (replay mode is opt-in OFF by
	// default to keep MustApplyOp's synthetic-tx convenience working).
	// Tests that want to exercise tx_missing_active_auth /
	// tx_duplicate_sig / tx_irrelevant_sig parity flip this to true and
	// use BuildSignedTx + PushSignedTx (which sign properly) instead of
	// PushOp/MustApplyOp.
	RequireSignatures bool

	// actorKeys is the per-actor PrivateKey cache keyed by name. Filled
	// lazily by Actor() so tests calling f.ActorKey("alice") get a stable
	// key even when RequireSignatures is off. Mirrors hived's
	// ACTORS(...) macro which seeds `alice_private_key = generate_private_key("alice")`.
	actorKeys map[string]*crypto.PrivateKey
}

// New initializes an empty database with the genesis state and returns
// a fixture ready for use. Calls t.Parallel() automatically — each
// fixture has its own t.TempDir-backed state.Database, so tests are
// fully isolated and the test suite saturates available cores.
func New(t *testing.T) *Fixture {
	t.Helper()
	t.Parallel()
	// Mainnet HIVE_OWNER_AUTH_HISTORY_TRACKING_START_BLOCK_NUM = 3,186,477
	// would prevent any fixture starting at block 1 from ever writing
	// owner_authority_history rows — the recover_account tests would
	// then fail "recent authority not found". Override to 1 for
	// fixture lifetime; restore on cleanup. Mirrors hived's testnet
	// USE_ALTERNATE_CHAIN_ID config which sets the constant to 1.
	// Owner-auth-history tracking starts at block 1 for fixtures so recovery
	// tests need not pre-roll 3M blocks. Set per-Context (see
	// OwnerAuthHistoryDefaultStartBlock) rather than via a package global,
	// which raced between parallel tests.
	dir := t.TempDir()
	db, err := state.Open(dir, 16<<20)
	if err != nil {
		t.Fatalf("testfixture: open state: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := genesis.Apply(db); err != nil {
		t.Fatalf("testfixture: genesis: %v", err)
	}
	ch := chain.New(db)
	// Owner-auth-history tracking from block 1 (mainnet: 3,186,477) so
	// recover_account tests need not pre-roll 3M blocks. Per-Database, not a
	// package global — the global raced between parallel fixtures.
	ch.SetOwnerAuthHistoryStartBlock(1)
	f := &Fixture{
		t:            t,
		State:        db,
		Chain:        ch,
		BlockNum:     0,
		curBlockTime: network.GenesisTimeUnix + BlockInterval,
	}
	return f
}

// HardforksUpTo activates every hardfork up to and including hf
// without walking the actual mainnet activation block numbers (which
// would require 10M+ block applies for HF17). Instead it:
//
//  1. Generates one block to bootstrap initminer accumulators.
//  2. Bumps dgpo.HeadBlockNumber + Time forward to the target HF's
//     activation block.
//  3. Calls applyHardfork(id) for each HF in [HF1..hf] directly.
//
// Functionally equivalent to running the activation hooks at the
// right block numbers; orders of magnitude faster for tests.
func (f *Fixture) HardforksUpTo(hf hardfork.ID) {
	// Generate at least one block so the chain has a head.
	if f.BlockNum == 0 {
		f.GenerateBlock()
	}
	target := hardfork.MainnetActivationBlock[hf]
	if f.BlockNum >= target {
		return
	}
	// Jump dgpo forward.
	s := state.NewSession(f.State)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		f.t.Fatalf("HardforksUpTo: read dgpo: %v", err)
	}
	dgpo.HeadBlockNumber = target
	dgpo.Time = network.GenesisTimeUnix + target*BlockInterval
	objects.PutDGPO(s, &dgpo)
	if err := s.Commit(false); err != nil {
		f.t.Fatalf("HardforksUpTo: commit dgpo: %v", err)
	}
	// Run activation logic for each HF in order.
	s2 := state.NewSession(f.State)
	for id := hardfork.ID(1); id <= hf; id++ {
		applyHardforkInTest(f.t, s2, id)
	}
	if err := s2.Commit(false); err != nil {
		f.t.Fatalf("HardforksUpTo: commit applies: %v", err)
	}
	f.BlockNum = target
	f.curBlockTime = dgpo.Time + BlockInterval
}

// applyHardforkInTest is a thin wrapper that calls
// chain.processHardforks-equivalent activation logic. We can't import
// chain's private applyHardfork, so we synthesize the same effect by
// running processHardforks transitions one HF at a time.
func applyHardforkInTest(t *testing.T, s *state.Session, id hardfork.ID) {
	// processHardforks compares previous-block HardforkSet to current.
	// Simulate one HF at a time by passing block num = activation+1
	// for `id` and activation for `id-1`.
	cur := hardfork.MainnetActivationBlock[id]
	chain.RunHardforkActivation(s, id, cur)
}

// PushOp adds an operation to the pending queue. The next
// GenerateBlock call bundles them into a single transaction.
func (f *Fixture) PushOp(o op.Operation) {
	f.pending = append(f.pending, op.FromOperation(o))
}

// PushOps adds multiple operations.
func (f *Fixture) PushOps(ops ...op.Operation) {
	for _, o := range ops {
		f.pending = append(f.pending, op.FromOperation(o))
	}
}

// GenerateBlock packs all pending ops into one transaction, builds a
// signed_block, and applies it through chain.Database.ApplyBlock — the
// same path used by replay. Returns the new head block number.
func (f *Fixture) GenerateBlock() uint32 {
	f.t.Helper()
	rawBlock := f.buildBlock(f.pending, f.curBlockTime)
	f.pending = nil
	blockNum := f.BlockNum + 1
	stats := chain.Stats{}
	dgpo, err := f.Chain.ApplyBlock(rawBlock, blockNum, chain.ApplyOptions{
		Sync:                      false,
		SkipUnknownOps:            false,
		TolerateFailingOps:        false,
		SkipTransactionSignatures: !f.RequireSignatures,
		SkipBlockSignature:        true,
		EnforceRC:                 f.EnforceRC,
		// Fixture blocks are generated at the head — the C++ unit tests
		// these port run with is_in_control()==true, so admission-only
		// asserts (delegate_rc min/sufficiency) stay active.
		InControl: true,
	}, &stats)
	if err != nil {
		f.t.Fatalf("testfixture: ApplyBlock %d: %v", blockNum, err)
	}
	f.BlockNum = blockNum
	f.prevID = dgpo.HeadBlockID
	f.curBlockTime += BlockInterval
	return blockNum
}

// GenerateBlocks generates n blocks. Pending ops attach to the first
// block; subsequent blocks are empty.
func (f *Fixture) GenerateBlocks(n int) {
	for i := 0; i < n; i++ {
		f.GenerateBlock()
	}
}

// SkipBlocks advances the head by n blocks without per-block hook side
// effects beyond the standard apply pipeline. Useful for jumping past
// time-gated checks (cashout windows, etc.).
func (f *Fixture) SkipBlocks(n int) {
	f.GenerateBlocks(n)
}

// SkipSeconds advances the chain by approximately `secs` seconds
// (rounded up to the next 3-second block boundary).
//
// FAST PATH (default): jumps DGPO.HeadBlockNumber + Time forward in
// one shot, then generates ONE block at the target so per-block
// schedule hooks (savings_withdraw, decline_voting_rights,
// vesting_withdrawal, etc.) sweep their tables and fire every matured
// entry. Functionally equivalent to walking each intermediate block
// for tests that only care about end-state — and roughly 1000× faster.
//
// REAL PATH: when correctness depends on per-block side effects
// firing along the way (inflation accrual, per-block witness reward,
// curation auction window, vote-mana regen across multiple votes),
// call SkipSecondsReal instead — that walks block by block.
func (f *Fixture) SkipSeconds(secs uint32) {
	if secs <= BlockInterval {
		f.GenerateBlock()
		return
	}
	blocks := uint32((secs + BlockInterval - 1) / BlockInterval)
	if blocks <= 1 {
		f.GenerateBlock()
		return
	}
	// Mirror C++ db_fixture generate_blocks(timestamp, miss_intermediate=
	// true): ONE real block lands at the target time and every skipped
	// slot is a MISSED slot — block numbers stay DENSE, only time jumps.
	// The old implementation bumped HeadBlockNumber by the slot count,
	// which made the %21 witness-schedule rebuild fire only when the
	// landing number happened to be ≡0 (mod 21): medians (e.g.
	// dgpo.HBDInterestRate) stayed at their genesis zeros for the rest
	// of the test, silently zeroing HBD interest and freezing schedule
	// maintenance — the root of the long-standing interest/timeshare
	// fixture failures.
	f.curBlockTime += (blocks - 1) * BlockInterval
	// One real block at the target so hooks fire.
	f.GenerateBlock()
}

// SkipSecondsReal advances the chain by `secs` seconds the slow,
// faithful way: every intermediate block is generated and applied,
// firing process_funds, processProducerReward, processCommentCashouts,
// and every other per-block hook on each tick. Use this for tests
// that depend on the cumulative effect of per-block work — inflation
// accrual, witness per-block payouts, voting mana regeneration over
// many votes, curation reverse-auction across many seconds.
//
// Cost: ~10K blocks/second wall clock. For 30 days = 864K blocks =
// ~90 seconds. Use sparingly.
func (f *Fixture) SkipSecondsReal(secs uint32) {
	blocks := int((secs + BlockInterval - 1) / BlockInterval)
	f.GenerateBlocks(blocks)
}

// HeadBlockTime returns the timestamp of the most recently applied block.
func (f *Fixture) HeadBlockTime() uint32 {
	if f.BlockNum == 0 {
		return network.GenesisTimeUnix
	}
	return f.curBlockTime - BlockInterval
}

// HeadBlockTimeForApply returns the timestamp that ops applied in the
// most recently produced block actually saw — that is, the C++
// `head_block_time()` value during their tx execution, which is the
// PREVIOUS block's timestamp (dgpo.time only advances to the new block
// after tx application). Used by tests that assert evaluator-set
// timestamps (CashoutTime, EffectiveDate, etc.).
func (f *Fixture) HeadBlockTimeForApply() uint32 {
	if f.BlockNum < 2 {
		return network.GenesisTimeUnix
	}
	return f.curBlockTime - 2*BlockInterval
}

// Account creates a new account named `name`. Equivalent to hived's
// ACTOR(name) macro: the account exists, has zero balance, and a
// non-empty memo key. We bypass account_create_operation (and its
// fee) by writing the row directly — same shortcut as hived's
// account_create() helper which forwards to db_plugin->debug_update.
//
// Also seeds an AccountAuthority row with owner/active/posting all
// satisfied by sha256("<name>") — the same convention as hived's
// generate_private_key("<name>") in db_fixture. The posting variant
// uses sha256("<name>_post"). This lets tests retrieve a stable key
// via f.ActorKey(name) / f.ActorPostingKey(name) and build signed
// transactions when RequireSignatures is on.
func (f *Fixture) Actor(name string) {
	f.t.Helper()
	s := state.NewSession(f.State)
	if has, _ := objects.HasAccount(s, name); has {
		return
	}
	objects.PutAccount(s, &objects.Account{
		Name:                 name,
		Created:              f.HeadBlockTime(),
		VotingManaCurrent:    10000,
		VotingManaLastUpdate: f.HeadBlockTime(),
		// Fixture accounts use "steem" as recovery_account by default
		// (matches pre-HF11 system convention). Tests that exercise
		// the recovery-agent path explicitly call SetRecoveryAccount
		// to set an agent.
		RecoveryAccount: "steem",
	})
	// Seed AccountAuthority so VerifyAuthority can resolve the row when
	// RequireSignatures is on. The active key is sha256("<name>") and
	// the posting key is sha256("<name>_post") — same convention used
	// by hived's database_fixture::generate_private_key.
	activeKey := f.actorKey(name).PublicKey()
	postingKey := f.actorPostingKey(name).PublicKey()
	objects.PutAccountAuthority(s, &objects.AccountAuthority{
		Account: name,
		Owner: auth.Authority{
			WeightThreshold: 1,
			KeyAuths:        []auth.KeyAuth{{Key: activeKey, Weight: 1}},
		},
		Active: auth.Authority{
			WeightThreshold: 1,
			KeyAuths:        []auth.KeyAuth{{Key: activeKey, Weight: 1}},
		},
		Posting: auth.Authority{
			WeightThreshold: 1,
			KeyAuths:        []auth.KeyAuth{{Key: postingKey, Weight: 1}},
		},
	})
	if err := s.Commit(false); err != nil {
		f.t.Fatalf("testfixture: actor %q: %v", name, err)
	}
}

// ActorKey returns the deterministic active/owner private key for
// `name` — sha256("<name>"), matching hived's generate_private_key.
// The key is also bound to the account's Active and Owner authority
// rows (see Actor), so signing with it satisfies any active-or-owner
// authority requirement.
func (f *Fixture) ActorKey(name string) *crypto.PrivateKey {
	f.t.Helper()
	return f.actorKey(name)
}

// ActorPostingKey returns the deterministic posting-authority private
// key for `name` — sha256("<name>_post").
func (f *Fixture) ActorPostingKey(name string) *crypto.PrivateKey {
	f.t.Helper()
	return f.actorPostingKey(name)
}

func (f *Fixture) actorKey(name string) *crypto.PrivateKey {
	if f.actorKeys == nil {
		f.actorKeys = make(map[string]*crypto.PrivateKey, 4)
	}
	if k, ok := f.actorKeys[name]; ok {
		return k
	}
	k := derivePrivateKey(f.t, name)
	f.actorKeys[name] = k
	return k
}

func (f *Fixture) actorPostingKey(name string) *crypto.PrivateKey {
	postingName := name + "_post"
	if f.actorKeys == nil {
		f.actorKeys = make(map[string]*crypto.PrivateKey, 4)
	}
	if k, ok := f.actorKeys[postingName]; ok {
		return k
	}
	k := derivePrivateKey(f.t, postingName)
	f.actorKeys[postingName] = k
	return k
}

// derivePrivateKey returns sha256(seed)-derived PrivateKey, mirroring
// hived's generate_private_key seed convention.
func derivePrivateKey(t *testing.T, seed string) *crypto.PrivateKey {
	t.Helper()
	digest := sha256.Sum256([]byte(seed))
	k, err := crypto.PrivateKeyFromBytes(digest[:])
	if err != nil {
		t.Fatalf("testfixture: derive private key for %q: %v", seed, err)
	}
	return k
}

// Actors creates multiple accounts in one shot.
func (f *Fixture) Actors(names ...string) {
	for _, n := range names {
		f.Actor(n)
	}
}

// SetRecoveryAccount sets `target`'s recovery_account to `agent`.
// Used by recovery-flow tests to establish the (target, agent)
// relationship without going through the 30-day
// change_recovery_account wait.
func (f *Fixture) SetRecoveryAccount(target, agent string) {
	f.t.Helper()
	s := state.NewSession(f.State)
	a, err := objects.GetAccount(s, target)
	if err != nil {
		f.t.Fatalf("SetRecoveryAccount: target %q: %v", target, err)
	}
	a.RecoveryAccount = auth.AccountName(agent)
	objects.PutAccount(s, &a)
	if err := s.Commit(false); err != nil {
		f.t.Fatalf("SetRecoveryAccount commit: %v", err)
	}
}

// FundLiquid credits `account` with `hive` HIVE-satoshi directly,
// bypassing transfer_from_initminer. Mirrors hived's fund() helper
// which uses adjust_balance.
func (f *Fixture) FundLiquid(account string, hive int64) {
	f.t.Helper()
	s := state.NewSession(f.State)
	a, err := objects.GetAccount(s, account)
	if err != nil {
		f.t.Fatalf("testfixture: fund %q: %v", account, err)
	}
	a.Balance += hive
	objects.PutAccount(s, &a)
	if dgpo, err := objects.GetDGPO(s); err == nil {
		dgpo.CurrentSupply += hive
		objects.PutDGPO(s, &dgpo)
	}
	if err := s.Commit(false); err != nil {
		f.t.Fatalf("testfixture: fund commit: %v", err)
	}
}

// FundLiquidHBD credits `account` with HBD-satoshi.
func (f *Fixture) FundLiquidHBD(account string, hbd int64) {
	f.t.Helper()
	s := state.NewSession(f.State)
	a, err := objects.GetAccount(s, account)
	if err != nil {
		f.t.Fatalf("testfixture: fund_hbd %q: %v", account, err)
	}
	objects.AdjustHBDBalance(s, &a, f.HeadBlockTime(), hbd)
	objects.PutAccount(s, &a)
	if dgpo, err := objects.GetDGPO(s); err == nil {
		dgpo.CurrentHBDSupply += hbd
		objects.PutDGPO(s, &dgpo)
	}
	if err := s.Commit(false); err != nil {
		f.t.Fatalf("testfixture: fund_hbd commit: %v", err)
	}
}

// FundVesting credits `account` with VESTS equivalent to `hive` HIVE
// at the genesis 1:1000 ratio (or dynamic price post-bootstrap).
// Mirrors hived's vest() helper, which calls create_vesting on the
// recipient with HIVE drawn from initminer.
func (f *Fixture) FundVesting(account string, hive int64) {
	f.t.Helper()
	s := state.NewSession(f.State)
	if _, err := evaluator.CreateVesting(s, account, hive, false); err != nil {
		f.t.Fatalf("testfixture: vest %q: %v", account, err)
	}
	// adjust supply for the new HIVE entering the vesting fund.
	if dgpo, err := objects.GetDGPO(s); err == nil {
		dgpo.CurrentSupply += hive
		objects.PutDGPO(s, &dgpo)
	}
	// HF20+ manabar wire-up: voting_manabar uses VESTS units; the
	// CreateVesting evaluator updates the manabar inline when called
	// inside a block apply (via Context.Hardforks), but the
	// fixture's direct CreateVesting bypass doesn't carry hardfork
	// context. Refresh here so test accounts have full mana to vote
	// with after funding. Pre-HF20 leaves VotingManaCurrent alone
	// (percent-scale).
	hf := hardfork.SetForBlock(f.BlockNum + 1)
	if hf.Has(hardfork.HF20) {
		a, err := objects.GetAccount(s, auth.AccountName(account))
		if err == nil {
			// Snap mana to max so subsequent votes have full capacity
			// (mirrors hived's HF20 _fill_mana=true ctor for newly-
			// vested accounts). The downvote bar fills to its pool max
			// too once the pool exists (HF21+) — real vest credits bump
			// it via update_manabar, so a freshly funded account in
			// hived's tests has a full downvote bar.
			a.VotingManaCurrent = a.EffectiveVestingSharesExclWithdraw()
			a.VotingManaLastUpdate = f.curBlockTime
			a.DownvoteManaCurrent = 0
			if dgpo, derr := objects.GetDGPO(s); derr == nil && dgpo.DownvotePoolPercent > 0 {
				a.DownvoteManaCurrent = a.VotingManaCurrent * int64(dgpo.DownvotePoolPercent) / 10000
			}
			a.DownvoteManaLastUpdate = f.curBlockTime
			objects.PutAccount(s, &a)
		}
	}
	if err := s.Commit(false); err != nil {
		f.t.Fatalf("testfixture: vest commit: %v", err)
	}
}

// ApplyOp pushes a single op and immediately generates a 1-op block.
// Returns the apply error (nil on success).
func (f *Fixture) ApplyOp(o op.Operation) error {
	f.PushOp(o)
	return f.tryGenerateBlock()
}

// MustApplyOp is ApplyOp with t.Fatal on error.
func (f *Fixture) MustApplyOp(o op.Operation) {
	f.t.Helper()
	if err := f.ApplyOp(o); err != nil {
		f.t.Fatalf("testfixture: apply %s: %v", o.OpTag().Name(), err)
	}
}

// tryGenerateBlock is GenerateBlock except it returns ApplyBlock errors
// instead of t.Fatal-ing — used by ApplyOp so callers can assert on
// expected failures (HIVE_REQUIRE_THROW analogue).
func (f *Fixture) tryGenerateBlock() error {
	rawBlock := f.buildBlock(f.pending, f.curBlockTime)
	f.pending = nil
	blockNum := f.BlockNum + 1
	stats := chain.Stats{}
	dgpo, err := f.Chain.ApplyBlock(rawBlock, blockNum, chain.ApplyOptions{
		Sync:                      false,
		SkipUnknownOps:            false,
		TolerateFailingOps:        false,
		SkipTransactionSignatures: !f.RequireSignatures,
		SkipBlockSignature:        true,
		EnforceRC:                 f.EnforceRC,
		// Fixture blocks are generated at the head — the C++ unit tests
		// these port run with is_in_control()==true, so admission-only
		// asserts (delegate_rc min/sufficiency) stay active.
		InControl: true,
	}, &stats)
	if err != nil {
		// Increment the block counter even on failure so the next call
		// uses a fresh block num — chain.Database state already moved
		// past the failed apply if it was tx-level.
		return err
	}
	f.BlockNum = blockNum
	f.prevID = dgpo.HeadBlockID
	f.curBlockTime += BlockInterval
	return nil
}

// Balance returns account.Balance (liquid HIVE-satoshi).
func (f *Fixture) Balance(account string) int64 {
	a, err := objects.GetAccount(state.NewSession(f.State), account)
	if err != nil {
		f.t.Fatalf("testfixture: Balance %q: %v", account, err)
	}
	return a.Balance
}

// HBDBalance returns account.HBDBalance.
func (f *Fixture) HBDBalance(account string) int64 {
	a, err := objects.GetAccount(state.NewSession(f.State), account)
	if err != nil {
		f.t.Fatalf("testfixture: HBDBalance %q: %v", account, err)
	}
	return a.HBDBalance
}

// Vests returns account.VestingShares.
func (f *Fixture) Vests(account string) int64 {
	a, err := objects.GetAccount(state.NewSession(f.State), account)
	if err != nil {
		f.t.Fatalf("testfixture: Vests %q: %v", account, err)
	}
	return a.VestingShares
}

// SavingsBalance / SavingsHBDBalance — convenience accessors.
func (f *Fixture) SavingsBalance(account string) int64 {
	a, err := objects.GetAccount(state.NewSession(f.State), account)
	if err != nil {
		f.t.Fatalf("testfixture: SavingsBalance %q: %v", account, err)
	}
	return a.SavingsBalance
}

func (f *Fixture) SavingsHBDBalance(account string) int64 {
	a, err := objects.GetAccount(state.NewSession(f.State), account)
	if err != nil {
		f.t.Fatalf("testfixture: SavingsHBDBalance %q: %v", account, err)
	}
	return a.SavingsHBDBalance
}

// HasAccount reports existence.
func (f *Fixture) HasAccount(name string) bool {
	has, _ := objects.HasAccount(state.NewSession(f.State), name)
	return has
}

// HasWitness reports witness existence.
func (f *Fixture) HasWitness(name string) bool {
	has, _ := objects.HasWitness(state.NewSession(f.State), name)
	return has
}

// Witness fetches the named witness.
func (f *Fixture) Witness(name string) objects.Witness {
	w, err := objects.GetWitness(state.NewSession(f.State), name)
	if err != nil {
		f.t.Fatalf("testfixture: Witness %q: %v", name, err)
	}
	return w
}

// HasWitnessVote reports whether `voter` votes for `witness`.
func (f *Fixture) HasWitnessVote(voter, witness string) bool {
	has, _ := objects.HasWitnessVote(state.NewSession(f.State), voter, witness)
	return has
}

// HasComment reports comment existence.
func (f *Fixture) HasComment(author, permlink string) bool {
	has, _ := objects.HasComment(state.NewSession(f.State), author, permlink)
	return has
}

// Comment fetches the named comment.
func (f *Fixture) Comment(author, permlink string) objects.Comment {
	c, err := objects.GetComment(state.NewSession(f.State), author, permlink)
	if err != nil {
		f.t.Fatalf("testfixture: Comment %q/%q: %v", author, permlink, err)
	}
	return c
}

// Account fetches the full Account object.
func (f *Fixture) Account_(name string) objects.Account {
	a, err := objects.GetAccount(state.NewSession(f.State), name)
	if err != nil {
		f.t.Fatalf("testfixture: Account %q: %v", name, err)
	}
	return a
}

// DGPO returns the current DynamicGlobalProperty.
func (f *Fixture) DGPO() objects.DynamicGlobalProperty {
	dgpo, err := objects.GetDGPO(state.NewSession(f.State))
	if err != nil {
		f.t.Fatalf("testfixture: DGPO: %v", err)
	}
	return dgpo
}

// BuildSignedTx assembles a signed_transaction from `ops` and signs it
// with each of `keys`. Returns the assembled transaction. Mirrors
// hived's `push_transaction(tx, {keys...})` test helper.
//
// The chain ID prefix used in the sig digest follows the active
// hardfork — pre-HF24 = network.OldChainID (zero), HF24+ = network.ChainID.
func (f *Fixture) BuildSignedTx(ops []op.Operation, keys ...*crypto.PrivateKey) tx.SignedTransaction {
	f.t.Helper()
	body := tx.Transaction{
		RefBlockNum:    uint16(f.BlockNum & 0xFFFF),
		RefBlockPrefix: 0,
		Expiration:     f.curBlockTime + 60,
		Extensions:     nil,
	}
	body.Operations = make([]op.Op, 0, len(ops))
	for _, o := range ops {
		body.Operations = append(body.Operations, op.FromOperation(o))
	}
	chainID := f.activeChainID()
	unsigned := tx.AppendTransactionBody(nil, &body)
	digest := crypto.SHA256Of2(chainID[:], unsigned)
	stx := tx.SignedTransaction{Transaction: body}
	for _, k := range keys {
		sig, err := crypto.SignCompact(k, digest)
		if err != nil {
			f.t.Fatalf("testfixture: SignCompact: %v", err)
		}
		stx.Signatures = append(stx.Signatures, sig)
	}
	return stx
}

// activeChainID returns network.ChainID if HF24 has fired on the
// current chain, else network.OldChainID. Reads HardforkPropertyObject
// from a fresh session.
func (f *Fixture) activeChainID() network.Hash32 {
	s := state.NewSession(f.State)
	if hp, err := objects.GetHardforkPropertyObject(s); err == nil {
		if hp.LastHardfork >= uint32(hardfork.HF24) {
			return network.ChainID
		}
	}
	return network.OldChainID
}

// PushSignedTx queues a pre-built signed_transaction for the next
// GenerateBlock. Mirrors hived's push_transaction in tests that build
// custom signature sets to exercise tx_missing_*_auth / tx_duplicate_sig
// / tx_irrelevant_sig.
func (f *Fixture) PushSignedTx(stx tx.SignedTransaction) {
	f.pendingSignedTxs = append(f.pendingSignedTxs, stx)
}

// ApplySignedTx queues `stx` and immediately generates a 1-tx block.
// Returns the apply error (nil on success).
func (f *Fixture) ApplySignedTx(stx tx.SignedTransaction) error {
	f.PushSignedTx(stx)
	return f.tryGenerateBlock()
}

// ApplyOpAs is sugar for "build a signed_transaction around `o`, sign
// it with `key`, and apply". Useful when the test only cares about the
// happy path.
func (f *Fixture) ApplyOpAs(o op.Operation, key *crypto.PrivateKey) error {
	stx := f.BuildSignedTx([]op.Operation{o}, key)
	return f.ApplySignedTx(stx)
}

// buildBlock packs `ops` into a single signed_transaction (if any),
// plus any pre-built signed transactions queued via PushSignedTx,
// computes the merkle root, and serializes a complete signed_block
// with empty witness signature. Returns the raw block bytes.
//
// Uses the WitnessSchedule's ScheduledWitnessForSlot(headBlockNum, 1)
// to pick the producer for this block — matching hived's
// generate_block path which always asks the schedule who's next.
// Falls back to initminer if the schedule is empty.
func (f *Fixture) buildBlock(ops []op.Op, blockTime uint32) []byte {
	witness := f.scheduledWitness()
	hdr := block.Header{
		Previous:   f.prevID,
		Timestamp:  blockTime,
		Witness:    witness,
		Extensions: []byte{0}, // empty flat_set
	}

	// Serialize transactions and compute merkle root.
	var txBytes []byte
	var digests []tx.Digest
	if len(ops) > 0 {
		signedTx := tx.SignedTransaction{
			Transaction: tx.Transaction{
				RefBlockNum:    uint16(f.BlockNum & 0xFFFF),
				RefBlockPrefix: 0,
				Expiration:     blockTime + 60,
				Operations:     ops,
				Extensions:     nil,
			},
		}
		signedBytes := tx.AppendSignedTransaction(nil, &signedTx)
		txBytes = append(txBytes, signedBytes...)
		// merkle uses the SIGNED tx digest (sha256 of signed bytes).
		digests = append(digests, tx.MerkleDigestFromBytes(signedBytes))
	}
	for i := range f.pendingSignedTxs {
		signedBytes := tx.AppendSignedTransaction(nil, &f.pendingSignedTxs[i])
		txBytes = append(txBytes, signedBytes...)
		digests = append(digests, tx.MerkleDigestFromBytes(signedBytes))
	}
	f.pendingSignedTxs = nil
	hdr.TransactionMerkleRoot = tx.MerkleRoot(digests)

	// Header → bytes; signed header = header || 65-byte zero signature.
	headerBytes := block.MarshalHeader(nil, &hdr)
	signedHeader := make([]byte, 0, len(headerBytes)+65)
	signedHeader = append(signedHeader, headerBytes...)
	var zeroSig [65]byte
	signedHeader = append(signedHeader, zeroSig[:]...)

	// Block body = signed_header || varint(tx_count) || tx_bytes.
	out := make([]byte, 0, len(signedHeader)+1+len(txBytes))
	out = append(out, signedHeader...)
	out = appendVarUint(out, uint32(len(digests)))
	out = append(out, txBytes...)
	return out
}

// appendVarUint writes a LEB128-style varint to dst.
func appendVarUint(dst []byte, v uint32) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// SetMedianAccountCreationFee force-sets the witness schedule's median
// account_creation_fee — stand-in for the witness props that feed the
// median in hived's testnet (where the default witness fee is non-zero,
// so HF20's exact-fee asserts have a real value to check against).
func (f *Fixture) SetMedianAccountCreationFee(fee int64) {
	s := state.NewSession(f.State)
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		f.t.Fatalf("testfixture: get witness schedule: %v", err)
	}
	ws.MedianAccountCreationFee = fee
	objects.PutWitnessSchedule(s, &ws)
	if err := s.Commit(false); err != nil {
		f.t.Fatalf("testfixture: set median fee commit: %v", err)
	}
}

// SetHardfork is an alias for HardforksUpTo.
func (f *Fixture) SetHardfork(hf hardfork.ID) { f.HardforksUpTo(hf) }

// scheduledWitness returns the producer for the next block, per the
// active WitnessSchedule. Reads from the committed state; returns
// InitMiner as a fallback if the schedule is missing or empty (e.g.,
// before genesis writes one — shouldn't happen but defensive).
func (f *Fixture) scheduledWitness() string {
	ws, err := objects.GetWitnessSchedule(state.NewSession(f.State))
	if err != nil || ws.NumScheduledWitnesses == 0 || len(ws.CurrentShuffledWitnesses) == 0 {
		return InitMiner
	}
	idx := (f.BlockNum + 1) % uint32(ws.NumScheduledWitnesses)
	if int(idx) >= len(ws.CurrentShuffledWitnesses) {
		return InitMiner
	}
	name := string(ws.CurrentShuffledWitnesses[idx])
	if name == "" {
		return InitMiner
	}
	return name
}

// AccountName converts a string to auth.AccountName (alias).
func AccountName(s string) auth.AccountName { return auth.AccountName(s) }

// MustNotApply expects ApplyOp to fail; t.Fatal if it succeeds.
func (f *Fixture) MustNotApply(o op.Operation) {
	f.t.Helper()
	if err := f.ApplyOp(o); err == nil {
		f.t.Fatalf("testfixture: expected %s to fail but it succeeded", o.OpTag().Name())
	}
}

// ApplyError returns ApplyOp's error string for assertion. Used in
// HIVE_REQUIRE_EXCEPTION-style checks.
func (f *Fixture) ApplyError(o op.Operation) string {
	if err := f.ApplyOp(o); err != nil {
		return err.Error()
	}
	return ""
}

// formatAsset formats an asset for error messages.
func formatAsset(a asset.Asset) string {
	return fmt.Sprintf("%d %s", a.Amount, a.Symbol.String())
}

var _ = formatAsset // suppress unused-warn if not used
