//go:build testnet

package hardfork

import "testing"

// TestAllHardforksActiveOnTestnet pins the rule that makes a gopherd testnet
// node agree with hived: every hardfork is active from block 1.
func TestAllHardforksActiveOnTestnet(t *testing.T) {
	s := SetForBlock(1)
	for i := ID(1); i <= NumHardforks; i++ {
		if !s.Has(i) {
			t.Fatalf("HF%d inactive at testnet block 1; hived reports last_hardfork=%d", i, NumHardforks)
		}
	}
	if got := s.HighestActive(); got != NumHardforks {
		t.Fatalf("highest active hardfork at block 1 = %d, want %d", got, NumHardforks)
	}
}
