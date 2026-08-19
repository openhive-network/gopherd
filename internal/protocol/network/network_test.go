package network

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestChainIDHexLength(t *testing.T) {
	if len(ChainID.Hex()) != 64 {
		t.Fatalf("ChainID hex must be 64 chars, got %d (%q)", len(ChainID.Hex()), ChainID.Hex())
	}
}

func TestNameMatchesPrefix(t *testing.T) {
	switch Name {
	case "mainnet":
		if AddressPrefix != "STM" {
			t.Fatalf("mainnet address prefix must be STM, got %q", AddressPrefix)
		}
		if !strings.HasPrefix(ChainID.Hex(), "beeab0de") {
			t.Fatalf("mainnet ChainID must start with beeab0de, got %s", ChainID.Hex())
		}
	case "testnet":
		// hived defines HIVE_ADDRESS_PREFIX as "STM" unconditionally, testnet
		// included. Using "TST" here made every key string incompatible with
		// hived's testnet, so a gopherd node could not join one.
		if AddressPrefix != "STM" {
			t.Fatalf("testnet address prefix must be STM (hived uses STM on testnet too), got %q", AddressPrefix)
		}
	default:
		t.Fatalf("unknown network %q", Name)
	}
}

// mainnetChainIDHex is the well-known mainnet chain_id, repeated here
// so we can assert the active build's ChainID is NOT equal to it
// (regardless of which build tag we're under).
const mainnetChainIDHex = "beeab0de00000000000000000000000000000000000000000000000000000000"

// TestChainIDDifferentFromMainnet ensures the active build (whichever
// network it targets) does not collide with mainnet's chain_id. Without
// this check the testnet-build accidentally re-deriving "beeab0de..."
// would let testnet signatures replay against mainnet.
func TestChainIDDifferentFromMainnet(t *testing.T) {
	if Name == "mainnet" {
		// On mainnet, the chain_id IS the literal mainnet hex — this
		// test is only meaningful on alternate builds.
		return
	}
	if ChainID.Hex() == mainnetChainIDHex {
		t.Fatalf("network %q ChainID collides with mainnet (%s) — must differ", Name, mainnetChainIDHex)
	}
}

// TestTestnetChainIDMatchesHived pins the testnet chain_id to hived's
// --chain-id DEFAULT on a testnet build, which is what makes joining a hived
// testnet possible at all.
//
// This value CANNOT be derived from config.hpp, which reads
// `HIVE_CHAIN_ID (fc::sha256::hash("testnet"))` = 9afbce9f... The shipped
// binary reports 18dcf0a2... because the testnet build exposes --chain-id as
// a runtime option with that default. It was taken from a running hived
// testnet node via database_api.get_config; re-derive it the same way if it
// ever changes, not from the header.
func TestTestnetChainIDMatchesHived(t *testing.T) {
	if Name != "testnet" {
		return
	}
	const hivedTestnetChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
	if ChainID.Hex() != hivedTestnetChainID {
		t.Fatalf("testnet ChainID = %s, want hived's testnet default %s",
			ChainID.Hex(), hivedTestnetChainID)
	}
	// The stale source-derived value, kept as an explicit tripwire.
	stale := sha256.Sum256([]byte("testnet"))
	if ChainID.Hex() == hex.EncodeToString(stale[:]) {
		t.Fatal("ChainID is sha256(\"testnet\") — that is the config.hpp value, not what hived actually runs")
	}
}

func TestBlockTiming(t *testing.T) {
	if BlockIntervalSec != 3 {
		t.Fatalf("BlockIntervalSec must be 3, got %d", BlockIntervalSec)
	}
	if BlocksPerDay != 28800 {
		t.Fatalf("BlocksPerDay must be 28800, got %d", BlocksPerDay)
	}
	if MaxWitnesses != 21 {
		t.Fatalf("MaxWitnesses must be 21, got %d", MaxWitnesses)
	}
}
