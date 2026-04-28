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
