//go:build testnet

package network

// Testnet constants. Source: config.hpp under the IS_TEST_NET branch,
// with ghive-specific overrides for the chain_id (so that ghive testnet
// traffic can never be confused with hived's stock testnet OR with
// mainnet — pre-empts cross-chain replay attacks the moment we expose
// any RPC).
const (
	Name          = "testnet"
	AddressPrefix = "TST"

	HiveSymbol  = "TESTS"
	HbdSymbol   = "TBD"
	VestsSymbol = "VESTS"

	// Testnet genesis time — 2016-01-01 00:00:00 UTC, same as mainnet's
	// configurable HF0 time.
	GenesisTimeUnix = 1451606400

	// chainIDSeed is the deterministic preimage hashed to produce the
	// testnet chain_id. v1 — bump when we ever need to fork the testnet
	// genesis (post-bump, old testnet binaries refuse to peer with new
	// ones, exactly the desired behavior on a chain reset).
	chainIDSeed = "ghive-testnet-v1"
)

// ChainID is sha256("ghive-testnet-v1"). DIFFERENT from both mainnet
// (beeab0de...) and hived's stock testnet (sha256("testnet")) — verified
// by TestChainIDDifferentFromMainnet.
var ChainID = sha256OfString(chainIDSeed)

// OldChainID equals ChainID on testnet — pre-HF24 mainnet used the
// all-zero hash for tx signing, but testnet has no such legacy.
var OldChainID = ChainID
