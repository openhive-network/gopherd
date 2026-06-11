// Package genesis seeds a fresh state.Database with the initial chain
// state hived starts from before block 1 is applied. Mirrors
// libraries/chain/database.cpp::init_genesis (substantially simplified).
package genesis

import (
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
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
	// Genesis account_authority_object for initminer — owner/active/
	// posting all carry the canonical init key as a 1-of-1 authority.
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

	// Mirror init_genesis (database_init.cpp:195-291): hived seeds the
	// system accounts (null/temp/miner) plus the legacy "steem" account
	// at genesis. Pre-HF14 transfers may target "null" before any
	// hardfork seed runs, and clearNullAccountBalance / null transfer
	// burn paths depend on these existing.
	systemAuth := auth.Authority{WeightThreshold: 1}
	// Mirror C++ database_init.cpp:197-211: hived creates "miners"
	// (plural — HIVE_MINER_ACCOUNT in config.hpp:443), "null", and
	// "temp" at genesis. Gopherd previously seeded "miner" (no 's')
	// — a literal name typo that would surface in any hived-state
	// diff and would silently fail any future op referencing the
	// canonical account by literal "miners".
	for _, name := range []string{"null", "temp", "miners"} {
		// Mirror C++ account_object minimal ctor at account_object.hpp:64-68
		// — system accounts use the minimal ctor with _fill_mana=false,
		// leaving voting_manabar.current_mana = 0. The default ctor also
		// sets mined=true on every account (account_object.hpp:270). Pass-14
		// genesis audit findings A2 + A3 — cosmetic state-dump diff but
		// trivial to mirror.
		objects.PutAccount(s, &objects.Account{
			Name:                 name,
			Created:              network.GenesisTimeUnix,
			VotingManaCurrent:    0,
			VotingManaLastUpdate: network.GenesisTimeUnix,
			Mined:                true,
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
	// "steem" account: hived's database_init.cpp:276-291 creates it as a
	// system account at genesis with HIVE_STEEM_PUBLIC_KEY_STR
	// ("STM65wH1LZ7BfSHcK69SShnqCAH5xdoSZpGkUjmzHJ5GCuxEK9V5G") wired
	// into owner = active = posting at threshold 1. The original chain
	// later "officially" mined this account in tx
	// 46ddcba847f2297d13e32be07d72d15c530a7271 — but well before that
	// happens the key is what the tx signatures recover to.
	steemKey, err := crypto.DecodePublicKey("STM65wH1LZ7BfSHcK69SShnqCAH5xdoSZpGkUjmzHJ5GCuxEK9V5G")
	if err != nil {
		return err
	}
	steemAuth := auth.Authority{
		WeightThreshold: 1,
		KeyAuths:        []auth.KeyAuth{{Key: steemKey, Weight: 1}},
	}
	objects.PutAccount(s, &objects.Account{
		Name:                 "steem",
		Created:              network.GenesisTimeUnix,
		VotingManaCurrent:    10000,
		VotingManaLastUpdate: network.GenesisTimeUnix,
		Mined:                true,
	})
	objects.PutAccountAuthority(s, &objects.AccountAuthority{
		Account:             "steem",
		Owner:               steemAuth,
		Active:              steemAuth,
		Posting:             steemAuth,
		LastOwnerUpdate:     network.GenesisTimeUnix,
		PreviousOwnerUpdate: network.GenesisTimeUnix,
	})

	// Genesis witness object for initminer — mirrors hived's
	// init_genesis_witnesses (database.cpp::init_genesis_witnesses).
	// Without this, the witness schedule re-shuffle drops initminer
	// the moment any other account registers as a witness.
	//
	// VirtualScheduledTime is initialized to uint128_max_value to
	// match C++'s witness_object default-init (witness_objects.hpp:149).
	// That keeps initminer at the END of by_schedule_time until votes
	// arrive and adjust_witness_vote sets a real VST.
	var vstMax [16]byte
	for i := range vstMax {
		vstMax[i] = 0xff
	}
	objects.PutWitness(s, &objects.Witness{
		Owner:                MainnetInitminer,
		Created:              network.GenesisTimeUnix,
		URL:                  "",
		BlockSigningKey:      memoKey,
		AccountCreationFee:   0,
		MaximumBlockSize:     65536,
		HBDInterestRate:      1000,
		AccountSubsidyBudget: 797,
		AccountSubsidyDecay:  347321,
		VirtualScheduledTime: vstMax,
	})

	objects.PutDGPO(s, &objects.DynamicGlobalProperty{
		HeadBlockNumber: 0,
		Time:            network.GenesisTimeUnix,
		CurrentWitness:  MainnetInitminer,
		// Mirror C++ global_property_object.hpp:205 — hbd_interest_rate
		// = 0 at genesis. Hived only credits HBD interest once
		// witnesses have published rates and the schedule rebuild
		// computes a non-zero median. Gopherd previously seeded 1000
		// (10% APR) defensively; that caused interest to start
		// accruing on the FIRST block instead of waiting for the feed
		// publish — directly diverging early-chain HBD balances.
		HBDInterestRate: 0,
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
		// Mirror C++ global_property_object.hpp struct default —
		// reverse_auction_seconds = HIVE_REVERSE_AUCTION_WINDOW_SECONDS_HF6
		// = 1800. The pre-HF6 vote evaluator gates the discount on
		// hardfork.HF6 / `now > HF6_TIME` predicates, so the field is
		// functionally inactive pre-HF6; mirror the C++ value anyway for
		// state-dump parity (HF6 hook overwrites to the same 1800).
		// Pass-14 genesis audit finding A7.
		ReverseAuctionSeconds: 1800,
		// Mirror C++ global_property_object.hpp:238 —
		// HIVE_DELEGATION_RETURN_PERIOD_HF0 = HIVE_CASHOUT_WINDOW_SECONDS
		// = 7 days (HF0 default). HF20 transitions to 5 days
		// (HIVE_VOTING_MANA_REGENERATION_SECONDS). Gopherd previously
		// seeded the post-HF20 5-day value at genesis, returning
		// delegated VESTS 2 days too early pre-HF20.
		DelegationReturnPeriod: 7 * 24 * 60 * 60,
		// hived's dynamic_global_property_object inits total_pow = -1
		// (= uint64::max) on purpose so the FIRST pow op's `++total_pow`
		// wraps to 0. That makes the first ever pow_worker for any
		// account land at 0, which is exactly what the
		//   HIVE_CHAIN_STATE_ASSERT( cur_witness->pow_worker == 0, … )
		// check at hive_evaluator.cpp:461 expects on a same-block re-pow
		// (admin block 1092 has two consecutive pow ops). Without this
		// init, Go's TotalPow runs +1 ahead of C++'s and every witness's
		// pow_worker is shifted by +1. See feedback memory
		// `pow_worker_minus_one_init` for the bisection that nailed this.
		TotalPow: ^uint64(0),
	})

	// Hardfork property object: at genesis processed_hardforks holds
	// only the genesis time and last_hardfork=0. apply_hardfork
	// pushes additional entries on each activation.
	objects.PutHardforkPropertyObject(s, &objects.HardforkPropertyObject{
		ProcessedHardforks:     []uint32{network.GenesisTimeUnix},
		LastHardfork:           0,
		CurrentHardforkVersion: hardfork.PackedVersion(hardfork.HF0),
		NextHardfork:           hardfork.PackedVersion(hardfork.HF0),
		NextHardforkTime:       network.GenesisTimeUnix,
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
