//go:build testnet

package network

// Testnet constants, matching hived's testnet build (IS_TEST_NET +
// USE_ALTERNATE_CHAIN_ID) so a gopherd node can join a hived testnet.
//
// Verified empirically against a running hived testnet node
// (build_testnet, node_type "testnet") via database_api.get_config rather
// than read off config.hpp — the source and the shipped binary disagreed on
// the single most important value here. config.hpp reads
// `HIVE_CHAIN_ID (fc::sha256::hash("testnet"))`, but the actual chain ID is
// 18dcf0a2..., because the testnet build exposes --chain-id as a RUNTIME
// option with that default. Deriving it from the source would have produced
// a node that could never handshake.
const (
	Name = "testnet"

	// hived defines HIVE_ADDRESS_PREFIX as "STM" UNCONDITIONALLY — testnet
	// included. An earlier gopherd testnet used "TST", which alone makes
	// every key string incompatible with hived's.
	AddressPrefix = "STM"

	// Legacy display symbols differ from mainnet, but the binary NAI codes
	// do not (@@000000021 / @@000000013 / @@000000037 are identical), so
	// asset serialisation is unaffected.
	HiveSymbol  = "TESTS"
	HbdSymbol   = "TBD"
	VestsSymbol = "VESTS"

	// HIVE_GENESIS_TIME — 2016-01-01T00:00:00 UTC.
	GenesisTimeUnix = 1451606400

	// defaultChainIDHex is hived's --chain-id default on a testnet build.
	defaultChainIDHex = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
)

// ChainID is the testnet chain ID used for transaction signing and the P2P
// handshake. Overridable via SetChainID so a node can join a testnet started
// with a non-default --chain-id, exactly as hived allows.
var ChainID = MustHexHash32(defaultChainIDHex)

// OldChainID equals ChainID on testnet — the pre-HF24 zero-hash signing rule
// is mainnet legacy that testnet never had.
var OldChainID = ChainID

// SetChainID overrides the chain ID at startup. Must be called before any
// signing or peer handshake; it is not safe to call on a running node.
func SetChainID(h Hash32) {
	ChainID = h
	OldChainID = h
}

// GlobalRemoveThreshold mirrors HIVE_GLOBAL_REMOVE_THRESHOLD
// (config.hpp:96 on the IS_TEST_NET branch). Ten times smaller than mainnet's,
// which is what makes the threshold reachable in hived's own proposal tests.
const GlobalRemoveThreshold int16 = 20
