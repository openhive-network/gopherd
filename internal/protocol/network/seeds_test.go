package network

import (
	"strings"
	"testing"
)

func TestMainnetSeedsNonEmpty(t *testing.T) {
	if len(MainnetSeeds) == 0 {
		t.Fatal("MainnetSeeds must be non-empty for the mainnet build")
	}
	for i, s := range MainnetSeeds {
		if !strings.Contains(s, ":") {
			t.Errorf("MainnetSeeds[%d]=%q is not host:port form", i, s)
		}
		// All hived seed nodes use port 2001 by convention; keep the
		// suffix as a regression guard against accidental edits.
		if !strings.HasSuffix(s, ":2001") {
			t.Errorf("MainnetSeeds[%d]=%q does not end in :2001", i, s)
		}
	}
}

func TestTestnetSeedsEmpty(t *testing.T) {
	// hived ships an empty default for testnet; preserve the expectation.
	if len(TestnetSeeds) != 0 {
		t.Fatalf("TestnetSeeds must be empty; got %d entries", len(TestnetSeeds))
	}
}
