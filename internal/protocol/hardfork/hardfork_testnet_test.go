//go:build testnet

package hardfork

import "testing"

// TestAllHardforksActiveOnTestnet pins the rule that makes a gopherd testnet
// node agree with hived: every SCHEDULED hardfork is active from block 1.
//
// The qualifier matters. This test used to loop to NumHardforks and report
// "hived reports last_hardfork=<NumHardforks>", which silently became a false
// claim the moment a hardfork was defined but not yet scheduled. A stock hived
// testnet does NOT activate HF29: its default time is 2100-01-01 specifically
// "so HF29 never auto-activates on testnets that don't explicitly schedule it"
// (libraries/protocol/hardfork.d/1_29.hf), and a mainnet build does not even
// register it. So a testnet reports last_hardfork=28 while HF29 is in
// development, and switching it on here would make gopherd enforce rules its
// hived peers do not — a consensus split, not a state drift.
func TestAllHardforksActiveOnTestnet(t *testing.T) {
	s := SetForBlock(1)
	highestScheduled := ID(0)
	for i := ID(1); i <= NumHardforks; i++ {
		if MainnetActivationBlock[i] == NeverActivates {
			if s.Has(i) {
				t.Errorf("HF%d is UNSCHEDULED but active at testnet block 1; hived leaves it dormant, so we would enforce rules our peers do not", i)
			}
			continue
		}
		highestScheduled = i
		if !s.Has(i) {
			t.Fatalf("HF%d inactive at testnet block 1; hived reports last_hardfork=%d", i, highestScheduled)
		}
	}
	if got := s.HighestActive(); got != highestScheduled {
		t.Fatalf("highest active hardfork at block 1 = %d, want %d (the newest SCHEDULED fork)", got, highestScheduled)
	}
}
