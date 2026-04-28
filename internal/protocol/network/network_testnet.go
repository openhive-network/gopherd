//go:build testnet

package network

// Testnet constants. Source: config.hpp under the IS_TEST_NET branch.
const (
	Name          = "testnet"
	AddressPrefix = "TST"

	HiveSymbol  = "TESTS"
	HbdSymbol   = "TBD"
	VestsSymbol = "VESTS"

	// Testnet genesis time — 2016-01-01 00:00:00 UTC, same as mainnet's
	// configurable HF0 time.
	GenesisTimeUnix = 1451606400
)

// On testnet HIVE_CHAIN_ID is fc::sha256::hash("testnet"). We compute it once
// at package init rather than hardcoding a derived constant.
var ChainID = sha256OfString("testnet")

// OldChainID equals ChainID on testnet (same fc::sha256::hash("testnet")).
var OldChainID = ChainID
