//go:build testnet

package network

import "testing"

// TestSetChainIDOverride covers joining a testnet started with a custom
// --chain-id, which hived supports and operators use to isolate networks.
func TestSetChainIDOverride(t *testing.T) {
	if Name != "testnet" {
		return
	}
	orig := ChainID
	defer SetChainID(orig)
	custom := MustHexHash32("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	SetChainID(custom)
	if ChainID != custom || OldChainID != custom {
		t.Fatalf("SetChainID did not apply: ChainID=%s OldChainID=%s", ChainID.Hex(), OldChainID.Hex())
	}
}
