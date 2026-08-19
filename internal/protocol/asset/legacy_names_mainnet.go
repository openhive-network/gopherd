//go:build !testnet

package asset

// Legacy (pre-NAI) asset symbol names for mainnet. hived writes these from
// OBSOLETE_SYMBOL_U64 / OBD_SYMBOL_U64, which carry the chain's Steem-era
// names rather than HIVE/HBD.
const (
	legacyHiveName = "STEEM"
	legacyHbdName  = "SBD"
)
