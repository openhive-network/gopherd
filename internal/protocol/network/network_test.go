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
		if AddressPrefix != "TST" {
			t.Fatalf("testnet address prefix must be TST, got %q", AddressPrefix)
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

// TestTestnetChainIDIsGhiveSeed pins the testnet chain_id to
// sha256("ghive-testnet-v1"). This is a regression guard — anyone who
// changes the seed (or accidentally reverts to fc::sha256::hash("testnet"))
// has to update this constant explicitly.
func TestTestnetChainIDIsGhiveSeed(t *testing.T) {
	if Name != "testnet" {
		return
	}
	want := sha256.Sum256([]byte("ghive-testnet-v1"))
	if ChainID.Hex() != hex.EncodeToString(want[:]) {
		t.Fatalf("testnet ChainID = %s, want sha256(ghive-testnet-v1) = %s",
			ChainID.Hex(), hex.EncodeToString(want[:]))
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
