// Package genesis seeds a fresh state.Database with the initial chain
// state hived starts from before block 1 is applied. Mirrors
// libraries/chain/database.cpp::init_genesis (substantially simplified).
package genesis

import (
	"hived-go/internal/crypto"
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
		Name:    MainnetInitminer,
		Created: network.GenesisTimeUnix,
		MemoKey: memoKey,
	})

	objects.PutDGPO(s, &objects.DynamicGlobalProperty{
		HeadBlockNumber: 0,
		Time:            network.GenesisTimeUnix,
		CurrentWitness:  MainnetInitminer,
	})

	return s.Commit(true)
}
