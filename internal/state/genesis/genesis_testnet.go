// Package genesis — genesis_testnet.go bootstraps a fresh state.Database
// for a multi-node ghive testnet. Unlike Apply (mainnet shape, only
// initminer seeded), ApplyTestnet pre-creates 21 elected witnesses with
// deterministic keys and 5 (default) node-operator accounts pre-funded
// with HIVE/HBD/VESTS so they can vote, transfer, and vest from block 1.
//
// Determinism is the whole point: every operator running `scripts/testnet-up`
// gets the same genesis state, the same 21 witnesses, the same WIFs.
// Reproducibility makes consensus drift trivial to diff across nodes.
package genesis

import (
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Per-account testnet funding numbers. Picked to be:
//
//   - large enough that 21 elected witnesses each get serious vote
//     weight from the operators' VESTS,
//   - small enough that the totals comfortably fit in int64 (we're not
//     stress-testing satoshi precision here),
//   - round numbers in human form so tests can assert exact totals.
//
// Operator: 1,000,000 HIVE liquid + 1,000,000 HBD liquid + 10,000,000
// VESTS-backing-HIVE worth of vesting (denominated in HIVE-satoshi at
// the genesis 1:1000 vest ratio = 10,000,000,000,000,000 sat-VESTS).
//
// Witness: 1,000 HIVE liquid + 1,000,000 sat-VESTS (just enough to cast
// a vote; operators do the heavy voting).
const (
	// HIVE_BLOCKCHAIN_PRECISION = 1000 (libraries/protocol/include/hive/
	// protocol/config.hpp). HIVE / HBD are 3-decimal.
	hiveSatPerCoin = 1_000

	// HIVE_BLOCKCHAIN_PRECISION_DIGITS = 3; VESTS is 6-decimal so 1 VESTS
	// = 1_000_000 sat-VESTS.
	vestsSatPerCoin = 1_000_000

	// Initial vest:HIVE ratio at genesis. hived's create_vesting uses
	// 1 HIVE → 1000 VESTS when both totals are 0, so 1 sat-HIVE → 1000
	// sat-VESTS (precision adjustment baked in: 3-decimal HIVE × 1000 =
	// 6-decimal VESTS-satoshi). We mirror that ratio for the genesis
	// vesting pool so first-block convert math doesn't surprise.
	initialVestPerHiveSat = 1_000
)

// per-operator funding (in satoshi).
var (
	operatorLiquidHive = int64(1_000_000) * hiveSatPerCoin // 1,000,000.000 HIVE
	operatorLiquidHBD  = int64(1_000_000) * hiveSatPerCoin // 1,000,000.000 HBD
	// 10,000,000 HIVE worth of VESTS at genesis ratio
	operatorVestingHiveBacking = int64(10_000_000) * hiveSatPerCoin
	operatorVestingShares      = operatorVestingHiveBacking * initialVestPerHiveSat

	witnessLiquidHive    = int64(1_000) * hiveSatPerCoin // 1,000.000 HIVE
	witnessVestingShares = int64(1_000_000)              // 1.000000 VESTS — enough to register
	// Witnesses don't need VESTS-backing-HIVE separately on the dgpo
	// side; we lump their share into the operator total when computing
	// TotalVestingFundHive below.
)

// ApplyTestnet seeds db with the testnet genesis state. Idempotent:
// re-applying on an already-bootstrapped db is a no-op (writes overwrite
// to identical values).
//
// Builds:
//   - initminer + the system accounts (null/temp/miner/steem) — kept for
//     parity with mainnet evaluators that special-case "null" etc.
//   - 21 deterministic witnesses (init-1 ... init-21) registered with
//     valid block-signing keys, pre-elected into the schedule.
//   - operatorCount node accounts (node-1 ... node-N) pre-funded with
//     HIVE / HBD / VESTS.
//   - dgpo with CurrentSupply / CurrentHBDSupply / TotalVestingShares /
//     TotalVestingFundHive consistent with the seeded balances so
//     update_virtual_supply on block 1 doesn't surprise.
//   - witness_schedule with all 21 witnesses scheduled.
//
// operatorCount must be in [1, 64]; pass 0 for the default
// (DefaultTestnetOperatorCount).
func ApplyTestnet(db *state.Database, operatorCount int) error {
	if operatorCount == 0 {
		operatorCount = DefaultTestnetOperatorCount
	}
	if operatorCount < 1 || operatorCount > 64 {
		return fmt.Errorf("genesis: operatorCount must be in [1,64], got %d", operatorCount)
	}
	s := state.NewSession(db)

	// Initminer: kept for compatibility with evaluators that still
	// reference HIVE_INIT_MINER_NAME. On testnet it does NOT produce
	// blocks (it's not in the schedule) and holds no balances.
	memoKey, err := crypto.DecodePublicKey(testnetInitminerKey())
	if err != nil {
		return fmt.Errorf("genesis: decode initminer key: %w", err)
	}
	objects.PutAccount(s, &objects.Account{
		Name:                 MainnetInitminer,
		Created:              network.GenesisTimeUnix,
		MemoKey:              memoKey,
		VotingManaCurrent:    10000,
		VotingManaLastUpdate: network.GenesisTimeUnix,
	})
	initAuth := auth.Authority{
		WeightThreshold: 1,
		KeyAuths:        []auth.KeyAuth{{Key: memoKey, Weight: 1}},
	}
	objects.PutAccountAuthority(s, &objects.AccountAuthority{
		Account:             MainnetInitminer,
		Owner:               initAuth,
		Active:              initAuth,
		Posting:             initAuth,
		LastOwnerUpdate:     network.GenesisTimeUnix,
		PreviousOwnerUpdate: network.GenesisTimeUnix,
	})

	// System accounts — same set Apply() seeds. null is the burn sink;
	// removing it would break first-block transfers to "null".
	systemAuth := auth.Authority{WeightThreshold: 1}
	// "miners" with 's' matches hived's HIVE_MINER_ACCOUNT
	// (config.hpp:443 + database_init.cpp:197).
	for _, name := range []string{"null", "temp", "miners", "steem"} {
		objects.PutAccount(s, &objects.Account{
			Name:                 name,
			Created:              network.GenesisTimeUnix,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: network.GenesisTimeUnix,
		})
		objects.PutAccountAuthority(s, &objects.AccountAuthority{
			Account:             name,
			Owner:               systemAuth,
			Active:              systemAuth,
			Posting:             systemAuth,
			LastOwnerUpdate:     network.GenesisTimeUnix,
			PreviousOwnerUpdate: network.GenesisTimeUnix,
		})
	}

	// 21 witness accounts + their witness_object rows.
	witnessKeys := TestnetWitnessKeys()
	scheduled := make([]auth.AccountName, 0, len(witnessKeys))
	for _, k := range witnessKeys {
		acctAuth := auth.Authority{
			WeightThreshold: 1,
			KeyAuths:        []auth.KeyAuth{{Key: k.PublicKey, Weight: 1}},
		}
		objects.PutAccount(s, &objects.Account{
			Name:                 k.Name,
			Created:              network.GenesisTimeUnix,
			Balance:              witnessLiquidHive,
			VestingShares:        witnessVestingShares,
			MemoKey:              k.PublicKey,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: network.GenesisTimeUnix,
		})
		objects.PutAccountAuthority(s, &objects.AccountAuthority{
			Account:             k.Name,
			Owner:               acctAuth,
			Active:              acctAuth,
			Posting:             acctAuth,
			LastOwnerUpdate:     network.GenesisTimeUnix,
			PreviousOwnerUpdate: network.GenesisTimeUnix,
		})

		// Pre-elected witness — VST=0 puts them at the FRONT of
		// by_schedule_time, so the first update_witness_schedule will
		// pick them as elected slots immediately. (A genuine schedule
		// rebuild on block 1 will set real VSTs; we just need the
		// initial scheduled set to be non-empty.)
		objects.PutWitness(s, &objects.Witness{
			Owner:              k.Name,
			Created:            network.GenesisTimeUnix,
			URL:                "",
			BlockSigningKey:    k.PublicKey,
			AccountCreationFee: 0,
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
			HBDExchangeRate: asset.Price{
				Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}},
				Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
			},
			LastHBDExchangeUpdate: network.GenesisTimeUnix,
			Schedule:              objects.WitnessScheduleElected,
		})
		scheduled = append(scheduled, auth.AccountName(k.Name))
	}

	// N operator accounts pre-funded with serious balances.
	operatorKeys := TestnetOperatorKeys(operatorCount)
	for _, k := range operatorKeys {
		opAuth := auth.Authority{
			WeightThreshold: 1,
			KeyAuths:        []auth.KeyAuth{{Key: k.PublicKey, Weight: 1}},
		}
		objects.PutAccount(s, &objects.Account{
			Name:                 k.Name,
			Created:              network.GenesisTimeUnix,
			Balance:              operatorLiquidHive,
			HBDBalance:           operatorLiquidHBD,
			VestingShares:        operatorVestingShares,
			MemoKey:              k.PublicKey,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: network.GenesisTimeUnix,
		})
		objects.PutAccountAuthority(s, &objects.AccountAuthority{
			Account:             k.Name,
			Owner:               opAuth,
			Active:              opAuth,
			Posting:             opAuth,
			LastOwnerUpdate:     network.GenesisTimeUnix,
			PreviousOwnerUpdate: network.GenesisTimeUnix,
		})
	}

	// Tally what the dgpo invariants expect: liquid HIVE + HBD across
	// all funded accounts, plus the HIVE backing the vesting pool.
	totalLiquidHive := int64(len(operatorKeys)) * operatorLiquidHive
	totalLiquidHive += int64(len(witnessKeys)) * witnessLiquidHive
	totalHBD := int64(len(operatorKeys)) * operatorLiquidHBD

	// VESTS-side: each operator's vest backing + each witness's vest
	// backing (witnesses get a tiny stake, denominated in sat-VESTS;
	// convert back to sat-HIVE backing at the genesis 1:1000 ratio).
	totalVestingShares := int64(len(operatorKeys))*operatorVestingShares +
		int64(len(witnessKeys))*witnessVestingShares
	witnessVestingHiveBacking := witnessVestingShares / initialVestPerHiveSat
	totalVestingFundHive := int64(len(operatorKeys))*operatorVestingHiveBacking +
		int64(len(witnessKeys))*witnessVestingHiveBacking

	// CurrentSupply per dynamic_global_property_object semantics is the
	// liquid HIVE supply + the vesting fund's HIVE backing (vesting
	// shares aren't HIVE — only their backing counts).
	currentSupply := totalLiquidHive + totalVestingFundHive

	objects.PutDGPO(s, &objects.DynamicGlobalProperty{
		HeadBlockNumber: 0,
		Time:            network.GenesisTimeUnix,
		// Block 0 has no producer in either chain — leave empty so
		// process_funds doesn't try to credit a phantom producer.
		CurrentWitness:         "",
		CurrentSupply:          currentSupply,
		CurrentHBDSupply:       totalHBD,
		TotalVestingShares:     totalVestingShares,
		TotalVestingFundHive:   totalVestingFundHive,
		VirtualSupply:          currentSupply + totalHBD, // 1:1 feed at genesis
		HBDInterestRate:        1000,
		HBDPrintRate:           10000,
		HBDStartPercent:        200,
		HBDStopPercent:         500,
		ContentRewardPercent:   7500,
		VestingRewardPercent:   1500,
		ProposalFundPercent:    0,
		ReverseAuctionSeconds:  0,
		DelegationReturnPeriod: 5 * 24 * 60 * 60,
		// total_pow init — see Apply() for the full rationale; pre-HF13
		// PoW eval still references this even on a chain that never sees
		// a real pow op.
		TotalPow: ^uint64(0),
	})

	objects.PutHardforkPropertyObject(s, &objects.HardforkPropertyObject{
		ProcessedHardforks:     []uint32{network.GenesisTimeUnix},
		LastHardfork:           0,
		CurrentHardforkVersion: hardfork.PackedVersion(hardfork.HF0),
		NextHardfork:           hardfork.PackedVersion(hardfork.HF0),
		NextHardforkTime:       network.GenesisTimeUnix,
	})

	// Pre-elected schedule: all 21 witnesses, no initminer fallback.
	// Median chain props pulled from witness defaults (every witness
	// publishes the same values at genesis so the median IS the
	// default).
	objects.PutWitnessSchedule(s, &objects.WitnessSchedule{
		NumScheduledWitnesses:    uint8(len(scheduled)),
		CurrentShuffledWitnesses: scheduled,
		MaxVotedWitnesses:        objects.MaxVotedWitnessesHF20,
		MaxMinerWitnesses:        0,
		MaxRunnerWitnesses:       1,
		NextShuffleBlockNum:      uint32(len(scheduled)) + 1, // first reshuffle after one full round
		MedianAccountCreationFee: 0,
		MedianMaximumBlockSize:   65536,
		MedianHBDInterestRate:    1000,
		// Pay weights match witness_schedule_object defaults
		// (timeshare=5, miner=1, elected=1). Normalization picks up
		// 21*1 = 21 since on testnet every slot is elected.
		TimeshareWeight:               5,
		MinerWeight:                   1,
		ElectedWeight:                 1,
		WitnessPayNormalizationFactor: uint32(len(scheduled)),
		MajorityVersion:               0,
		HardforkRequiredWitnesses:     17,
	})

	return s.Commit(true)
}

// testnetInitminerKey returns the initminer's compile-time-prefixed
// public key. We use the same canonical key as mainnet so historical
// references in tests / docs keep working; on testnet the address
// prefix is TST so the encoded form differs even though the underlying
// 33 bytes are identical.
func testnetInitminerKey() string {
	// We can't reuse the literal mainnet "STM..." string because
	// network.AddressPrefix is "TST" under -tags testnet, and
	// crypto.DecodePublicKey rejects mismatched prefixes.
	//
	// Easier path: re-encode the well-known initminer pubkey under the
	// active prefix at call time. The compressed bytes of the canonical
	// initminer are the public form of the secret 5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n
	// — but we can produce ANY known pubkey here. Use the testnet-witness
	// scope with name "initminer" so even initminer's key is deterministic.
	k := mustDeriveTestnetKey("witness", "initminer")
	return k.PublicKey.EncodeBase58()
}
