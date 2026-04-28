package network

import (
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
