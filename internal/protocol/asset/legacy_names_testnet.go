//go:build testnet

package asset

// Legacy (pre-NAI) asset symbol names for testnet.
//
// hived's asset_symbol.hpp redefines OBSOLETE_SYMBOL_U64 as "TESTS" and
// OBD_SYMBOL_U64 as "TBD" under IS_TEST_NET. Emitting the mainnet "STEEM"
// here silently breaks every legacy-serialised transaction: the bytes differ,
// so the signature digest differs, and hived rejects the transaction with
// "missing required active authority" — pointing at the key rather than at
// the serialisation. Found exactly this way against a live hived testnet,
// by diffing our transaction hex against the node's own re-serialisation.
//
// The binary NAI codes are identical to mainnet; only these legacy names differ.
const (
	legacyHiveName = "TESTS"
	legacyHbdName  = "TBD"
)
