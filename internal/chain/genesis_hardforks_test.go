package chain

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
)

// TestGenesisHardforksNoOpOnMainnet is a consensus guard. Block 1 applies every
// hardfork already due by its timestamp (hived database.cpp:2246), which is
// what lets a testnet launch directly on current rules. On MAINNET that loop
// must select ZERO hardforks — block 1 is dated 2016-03-24 while HF1 activates
// 2016-04-25. If a future edit made it select even one, mainnet replay would
// diverge from the canonical chain at the very first block.
func TestGenesisHardforksNoOpOnMainnet(t *testing.T) {
	if network.Name != "mainnet" {
		t.Skip("mainnet-only invariant")
	}
	const mainnetBlock1Time = network.GenesisTimeUnix + 3
	n := 0
	for id := hardfork.ID(1); id <= hardfork.NumHardforks; id++ {
		if hardfork.MainnetActivationTime[id] > mainnetBlock1Time {
			break
		}
		n++
	}
	if n != 0 {
		t.Fatalf("mainnet block 1 would apply %d hardforks at genesis; must be 0 "+
			"(HF1 time %d must be after block-1 time %d)",
			n, hardfork.MainnetActivationTime[1], mainnetBlock1Time)
	}
}

// TestNoHardforkActivatesAtBlockOne backs the early return in processHardforks:
// block 1 takes the genesis-hardfork path and skips the activation-block scan,
// which is only safe while no hardfork is scheduled for block 1.
func TestNoHardforkActivatesAtBlockOne(t *testing.T) {
	for id := hardfork.ID(1); id <= hardfork.NumHardforks; id++ {
		if hardfork.MainnetActivationBlock[id] == 1 {
			t.Fatalf("HF%d activates at block 1; processHardforks' block-1 early "+
				"return would skip it", id)
		}
	}
}
