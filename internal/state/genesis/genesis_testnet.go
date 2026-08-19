// Package genesis — genesis_testnet.go reproduces hived's TESTNET genesis
// (database_init.cpp::init_genesis compiled with IS_TEST_NET +
// USE_ALTERNATE_CHAIN_ID) so a gopherd node can join a hived testnet and
// agree with it from block 1.
//
// An earlier version of this file built a BESPOKE gopherd-only testnet: its
// own chain ID, 21 pre-elected init-1..init-21 witnesses, TST key prefix, and
// pre-funded operator accounts. That chain was self-consistent but could never
// interoperate with hived, because two nodes with different genesis state
// disagree at block 1 no matter how correct each is on its own.
//
// Every constant here was taken from a RUNNING hived testnet node via
// database_api.get_config / list_accounts / list_witnesses, not from
// config.hpp — the header and the shipped binary disagree (most importantly on
// the chain ID), so the source is not a reliable spec.
package genesis

import (
	"crypto/sha256"
	"fmt"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Testnet initial supply, issued to initminer at genesis.
// HIVE_INIT_SUPPLY = 243 * 1e6 * 1e3, HIVE_HBD_INIT_SUPPLY = 7 * 1e6 * 1e3.
// Confirmed live: initminer holds 243000000000 / 7000000000 at block 1.
const (
	TestnetInitHiveSupply = int64(243) * 1_000_000 * 1_000
	TestnetInitHbdSupply  = int64(7) * 1_000_000 * 1_000
)

// TestnetSkeletonSeed is the preimage hived hashes to build the default
// testnet initminer key: private_key::regenerate(sha256("init_key")).
// The resulting public key is STM6LLegbAgLAy28EHrffBVuANFWcFgmqRMW13wBmTExqFE9SCkg4,
// which TestInitminerKeyMatchesHived pins against the live node.
const TestnetSkeletonSeed = "init_key"

// TestnetInitminerKey derives the testnet initminer key pair from the
// skeleton seed. hived exposes the same thing as --skeleton-key, defaulting
// to the WIF 5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n.
func TestnetInitminerKey() (*crypto.PrivateKey, error) {
	sec := sha256.Sum256([]byte(TestnetSkeletonSeed))
	return crypto.PrivateKeyFromBytes(sec[:])
}

// ApplyTestnet seeds db with hived's testnet genesis state.
//
// extraWitnesses mirrors hived's configurable `init_witnesses` list: each name
// is created as an account and registered as a witness signing with the SAME
// init key, exactly as init_genesis does. Pass nil for hived's default (only
// initminer).
func ApplyTestnet(db *state.Database, extraWitnesses []string) error {
	priv, err := TestnetInitminerKey()
	if err != nil {
		return fmt.Errorf("genesis: derive testnet initminer key: %w", err)
	}
	initPub := priv.PublicKey()

	s := state.NewSession(db)

	const gt = network.GenesisTimeUnix

	// locked_auth( 1 ): threshold 1 with no keys — unusable by design.
	lockedAuth := auth.Authority{WeightThreshold: 1}
	// open_auth( 0 ): threshold 0, so ANY signature satisfies it. hived uses
	// this for "temp" only; seeding it as a locked authority instead would
	// silently reject transactions the network accepts.
	openAuth := auth.Authority{WeightThreshold: 0}

	putSystem := func(name string, a auth.Authority) {
		objects.PutAccount(s, &objects.Account{
			Name:                 name,
			Created:              gt,
			VotingManaCurrent:    0,
			VotingManaLastUpdate: gt,
			Mined:                true,
		})
		objects.PutAccountAuthority(s, &objects.AccountAuthority{
			Account: name, Owner: a, Active: a, Posting: a,
			LastOwnerUpdate: gt, PreviousOwnerUpdate: gt,
		})
	}

	// Order mirrors init_genesis. On testnet BOTH treasuries exist from
	// genesis (mainnet creates them at HF21/HF24 instead) — confirmed live,
	// list_accounts returns steem.dao and hive.fund at block 1.
	putSystem("miners", lockedAuth)
	putSystem("null", lockedAuth)
	putSystem("steem.dao", lockedAuth)
	putSystem("hive.fund", lockedAuth)
	putSystem("temp", openAuth)

	// initminer + any configured init witnesses, all signing with initPub.
	initAuth := auth.Authority{
		WeightThreshold: 1,
		KeyAuths:        []auth.KeyAuth{{Key: initPub, Weight: 1}},
	}
	var vstMax [16]byte
	for i := range vstMax {
		vstMax[i] = 0xff
	}
	witnessNames := append([]string{MainnetInitminer}, extraWitnesses...)
	scheduled := make([]auth.AccountName, 0, len(witnessNames))
	for _, name := range witnessNames {
		objects.PutAccount(s, &objects.Account{
			Name:                 name,
			Created:              gt,
			MemoKey:              initPub,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: gt,
		})
		objects.PutAccountAuthority(s, &objects.AccountAuthority{
			Account: name, Owner: initAuth, Active: initAuth, Posting: initAuth,
			LastOwnerUpdate: gt, PreviousOwnerUpdate: gt,
		})
		objects.PutWitness(s, &objects.Witness{
			Owner: name,
			// hived's init_witness does not set `created`, so the field keeps
			// its zero value — list_witnesses reports 1970-01-01, NOT the
			// genesis time.
			Created:              0,
			BlockSigningKey:      initPub,
			AccountCreationFee:   0,
			MaximumBlockSize:     65536,
			HBDInterestRate:      1000,
			AccountSubsidyBudget: 797,
			AccountSubsidyDecay:  347321,
			VirtualScheduledTime: vstMax,
			Schedule:             objects.WitnessScheduleMiner,
		})
		scheduled = append(scheduled, auth.AccountName(name))
	}

	// "steem": on testnet its authority carries the init key ALONGSIDE the
	// legacy steem key (init_genesis wraps this in USE_ALTERNATE_CHAIN_ID), so
	// testnet tooling can act as it.
	steemKey, err := crypto.DecodePublicKey("STM65wH1LZ7BfSHcK69SShnqCAH5xdoSZpGkUjmzHJ5GCuxEK9V5G")
	if err != nil {
		return fmt.Errorf("genesis: decode steem key: %w", err)
	}
	steemAuth := auth.Authority{
		WeightThreshold: 1,
		KeyAuths: []auth.KeyAuth{
			{Key: steemKey, Weight: 1},
			{Key: initPub, Weight: 1},
		},
	}
	objects.PutAccount(s, &objects.Account{
		Name: "steem", Created: gt, MemoKey: steemKey,
		VotingManaCurrent: 10000, VotingManaLastUpdate: gt, Mined: true,
	})
	objects.PutAccountAuthority(s, &objects.AccountAuthority{
		Account: "steem", Owner: steemAuth, Active: steemAuth, Posting: steemAuth,
		LastOwnerUpdate: gt, PreviousOwnerUpdate: gt,
	})

	// Issue the initial supply to initminer.
	initminer, err := objects.GetAccount(s, MainnetInitminer)
	if err != nil {
		return fmt.Errorf("genesis: initminer missing after seeding: %w", err)
	}
	initminer.Balance = TestnetInitHiveSupply
	initminer.HBDBalance = TestnetInitHbdSupply
	objects.PutAccount(s, &initminer)

	// feed_history at a 1:1 TESTS/TBD price. Testnet seeds this at genesis
	// (mainnet has no feed until witnesses publish), and virtual_supply
	// depends on it from the very first block.
	oneToOne := asset.Price{
		Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}},
		Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
	}
	objects.PutFeedHistory(s, &objects.FeedHistory{
		CurrentMedianHistory: oneToOne,
		MarketMedianHistory:  oneToOne,
		CurrentMinHistory:    oneToOne,
		CurrentMaxHistory:    oneToOne,
	})

	objects.PutDGPO(s, &objects.DynamicGlobalProperty{
		HeadBlockNumber:  0,
		Time:             gt,
		CurrentWitness:   MainnetInitminer,
		CurrentSupply:    TestnetInitHiveSupply,
		CurrentHBDSupply: TestnetInitHbdSupply,
		// 1:1 feed at genesis, so virtual = HIVE + HBD.
		VirtualSupply:          TestnetInitHiveSupply + TestnetInitHbdSupply,
		HBDInterestRate:        0,
		HBDPrintRate:           10000,
		HBDStartPercent:        200,
		HBDStopPercent:         500,
		ContentRewardPercent:   7500,
		VestingRewardPercent:   1500,
		ProposalFundPercent:    0,
		ReverseAuctionSeconds:  1800,
		DelegationReturnPeriod: 7 * 24 * 60 * 60,
		TotalPow:               ^uint64(0),
	})

	objects.PutHardforkPropertyObject(s, &objects.HardforkPropertyObject{
		ProcessedHardforks:     []uint32{gt},
		LastHardfork:           0,
		CurrentHardforkVersion: hardfork.PackedVersion(hardfork.HF0),
		NextHardfork:           hardfork.PackedVersion(hardfork.HF0),
		NextHardforkTime:       gt,
	})

	objects.PutWitnessSchedule(s, &objects.WitnessSchedule{
		NumScheduledWitnesses:    uint8(len(scheduled)),
		CurrentShuffledWitnesses: scheduled,
		MaxVotedWitnesses:        objects.MaxVotedWitnessesHF0,
		MaxMinerWitnesses:        objects.MaxMinerWitnessesHF0,
		NextShuffleBlockNum:      1,
		MedianAccountCreationFee: 0,
		MedianMaximumBlockSize:   65536,
		MedianHBDInterestRate:    1000,
		// witness_schedule_object's C++ struct defaults. These are NOT
		// decorative: process_funds multiplies the witness reward by
		// HIVE_MAX_WITNESSES and then by the producer's schedule weight, and
		// divides by witness_pay_normalization_factor — which is itself
		// computed as (elected_weight * num_elected + ...). Leaving the weights
		// at zero makes the normalization factor zero, gopherd's divide-by-zero
		// guard then pays the UNWEIGHTED reward, and the witness earns 1/21 of
		// what hived pays. Measured against a live hived testnet: 2093 vs 6773
		// sat/block of inflation.
		ElectedWeight:             1,
		MinerWeight:               1,
		TimeshareWeight:           5,
		HardforkRequiredWitnesses: 17,
	})

	return s.Commit(true)
}
