// Package genesis seeds a fresh state.Database with the initial chain
// state hived starts from before block 1 is applied. Mirrors
// libraries/chain/database.cpp::init_genesis (substantially simplified).
package genesis

import (
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/network"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// MainnetInitminer is the canonical initial-witness account name used at
// genesis. Source: HIVE_INIT_MINER_NAME in libraries/protocol/include/hive/protocol/config.hpp.
const MainnetInitminer = "initminer"

// Apply seeds db with the genesis state.
//
// The seed creates the initminer account with a minimal record; hived's
// genesis also sets up the default witnesses, the price feed, the global
// properties, and grants initial supply. We seed only what the currently
// implemented evaluators read so this is consensus-correct under the
// stub model — broader genesis lands alongside the relevant state
// objects (witness_object, feed_history_object, etc.).
func Apply(db *state.Database) error {
	s := state.NewSession(db)

	// Decode the canonical initminer public key to use as memo_key.
	memoKey, err := crypto.DecodePublicKey("STM8GC13uCZbP44HzMLV6zPZGwVQ8Nt4Kji8PapsPiNq1BK153XTX")
	if err != nil {
		return err
	}

	objects.PutAccount(s, &objects.Account{
		Name:                 MainnetInitminer,
		Created:              network.GenesisTimeUnix,
		MemoKey:              memoKey,
		VotingManaCurrent:    10000, // HIVE_100_PERCENT — starts at full
		VotingManaLastUpdate: network.GenesisTimeUnix,
	})

	// Mirror init_genesis (database_init.cpp:195-291): hived seeds the
	// system accounts (null/temp/miner) plus the legacy "steem" account
	// at genesis. Pre-HF14 transfers may target "null" before any
	// hardfork seed runs, and clearNullAccountBalance / null transfer
	// burn paths depend on these existing.
	for _, name := range []string{"null", "temp", "miner", "steem"} {
		objects.PutAccount(s, &objects.Account{
			Name:                 name,
			Created:              network.GenesisTimeUnix,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: network.GenesisTimeUnix,
		})
	}

	// Genesis witness object for initminer — mirrors hived's
	// init_genesis_witnesses (database.cpp::init_genesis_witnesses).
	// Without this, the witness schedule re-shuffle drops initminer
	// the moment any other account registers as a witness.
	objects.PutWitness(s, &objects.Witness{
		Owner:              MainnetInitminer,
		Created:            network.GenesisTimeUnix,
		URL:                "",
		BlockSigningKey:    memoKey,
		AccountCreationFee: 0,
		MaximumBlockSize:   65536,
		HBDInterestRate:    1000,
	})

	objects.PutDGPO(s, &objects.DynamicGlobalProperty{
		HeadBlockNumber: 0,
		Time:            network.GenesisTimeUnix,
		CurrentWitness:  MainnetInitminer,
		// HIVE_DEFAULT_HBD_INTEREST_RATE = 1000 (10% APR). Witnesses
		// publish their preferred rate; the median lands here once the
		// witness schedule subsystem is wired up.
		HBDInterestRate: 1000,
		// Pre-HF14 print rate is pinned at 100%; HF14 activation hook
		// also explicitly sets this.
		HBDPrintRate: 10000,
		// HBD print-rate ramp boundaries. Match hived's struct default
		// (HIVE_HBD_START_PERCENT_HF14 = 2%, HIVE_HBD_STOP_PERCENT_HF14
		// = 5%); HF20 and HF26 reset these.
		HBDStartPercent: 200,
		HBDStopPercent:  500,
		// HF16+ inflation split percentages (75/15/0 — witness gets
		// the remainder). Pre-HF16 path ignores these.
		ContentRewardPercent: 7500,
		VestingRewardPercent: 1500,
		ProposalFundPercent:  0,
		// Pre-HF6 there's no auction window; HF6 sets it to 30 min.
		ReverseAuctionSeconds: 0,
		// Pre-HF20 default delegation return period is 5 days.
		DelegationReturnPeriod: 5 * 24 * 60 * 60,
	})

	// Treasury accounts ("steem.dao" pre-HF24, "hive.fund" HF24+) are
	// created on-chain at HF21 / HF24 respectively, NOT seeded at
	// genesis. processProposals tolerates either name being absent.

	// Genesis witness schedule: initminer is the sole producer until
	// real witnesses register and accumulate votes. Mirrors hived's
	// init_genesis_state which seeds the schedule with HIVE_INIT_MINER_NAME.
	objects.PutWitnessSchedule(s, &objects.WitnessSchedule{
		NumScheduledWitnesses: 1,
		CurrentShuffledWitnesses: []auth.AccountName{
			auth.AccountName(MainnetInitminer),
		},
		MaxVotedWitnesses:        objects.MaxVotedWitnessesHF0,
		MaxMinerWitnesses:        objects.MaxMinerWitnessesHF0,
		NextShuffleBlockNum:      1,
		MedianAccountCreationFee: 0,
		MedianMaximumBlockSize:   65536, // HIVE_MIN_BLOCK_SIZE_LIMIT
		MedianHBDInterestRate:    1000,
	})

	return s.Commit(true)
}
