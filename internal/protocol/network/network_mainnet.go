//go:build !testnet

package network

// Mainnet constants. Source: config.hpp under the IS_LIVE_HIVE_NETWORK branch
// without USE_ALTERNATE_CHAIN_ID.
const (
	Name          = "mainnet"
	AddressPrefix = "STM"

	// HiveSymbol / HbdSymbol / VestsSymbol are the legacy 3-character symbols
	// used by parts of the JSON-RPC layer. Asset binary encoding is symbol-aware
	// via internal/protocol/asset; these strings are the human-readable form.
	HiveSymbol  = "HIVE"
	HbdSymbol   = "HBD"
	VestsSymbol = "VESTS"

	// chainIDHex is HIVE_CHAIN_ID for mainnet.
	// Pre-HF24, transactions were signed with OldChainID (the zero hash);
	// post-HF24, signing uses ChainID below.
	chainIDHex = "beeab0de00000000000000000000000000000000000000000000000000000000"

	// GenesisTimeUnix = 1458835200 (Friday, January 1, 2016 12:00:00 AM UTC)
	GenesisTimeUnix = 1458835200
)

// ChainID is the post-HF24 chain ID used in transaction signing.
var ChainID = MustHexHash32(chainIDHex)

// OldChainID is the chain ID used for signing pre-HF24. It is the all-zero hash.
var OldChainID = Hash32{}

// GlobalRemoveThreshold mirrors HIVE_GLOBAL_REMOVE_THRESHOLD
// (libraries/protocol/include/hive/protocol/config.hpp:174 on the live-network
// branch). It bounds how many state objects a single removal pass may erase;
// hived stores it in dynamic_global_property_object::current_remove_threshold,
// which only the debug plugin ever changes, so a constant is faithful for any
// chain gopherd replays. A negative value would mean "no limit".
const GlobalRemoveThreshold int16 = 200

// BlockchainVersion mirrors HIVE_BLOCKCHAIN_VERSION for the live network
// (config.hpp:139 at 1.29.0-rc2). hived defines it in THREE places — mainnet,
// testnet and mirrornet — and 1.29 bumped only mainnet, to 1.28.7 (commit
// dab219074, the release carrying the p2p fork-tolerance fixes); the testnet
// definition went to 1.29.0 separately.
//
// Two consumers, one source of truth: database_api.get_version reports the
// string, and a produced block header carries the packed v_num in its version
// extension (which is what sets the witness's running_version). They are
// derived from the same triple so they cannot drift.
const (
	BlockchainVersionMajor    = 1
	BlockchainVersionHardfork = 28
	BlockchainVersionRevision = 7
)
