package hardfork

import "testing"

func TestActivationOrdering(t *testing.T) {
	if MainnetActivationBlock[HF20] == 0 {
		t.Skip("testnet build: activation blocks are zeroed")
	}
	// Activation block numbers must be monotonically non-decreasing in HF order.
	// HF18 == HF17 + 31 and HF22 is only ~52k blocks after HF21, so equal-or-greater
	// with strict-greater between distant hardforks.
	prev := uint32(0)
	for i := ID(1); i <= NumHardforks; i++ {
		got := MainnetActivationBlock[i]
		if got < prev {
			t.Fatalf("HF%d activation %d goes backwards from prior %d", i, got, prev)
		}
		prev = got
	}
}

// TestSetForBlock covers MAINNET activation semantics: a hardfork is inactive
// during its own activation block and active from the next one. It is
// meaningless on a testnet build, where every hardfork is active from block 1
// (see hardfork_testnet.go) — the testnet rule is pinned by
// TestAllHardforksActiveOnTestnet instead.
func TestSetForBlock(t *testing.T) {
	if MainnetActivationBlock[HF20] == 0 {
		t.Skip("testnet build: all hardforks active from block 1")
	}
	// Genesis: only HF0.
	if !SetForBlock(0).Has(HF0) {
		t.Fatal("HF0 must always be active")
	}
	if SetForBlock(0).Has(HF1) {
		t.Fatal("HF1 must not be active at block 0")
	}

	// At HF20 activation, HF20 NOT yet active during evaluators (process_hardforks
	// runs at end of block apply); HF20 first active for block N+1 onward.
	at20 := SetForBlock(MainnetActivationBlock[HF20])
	if at20.Has(HF20) {
		t.Fatal("HF20 must NOT be active during its activation block (process_hardforks runs at end of apply)")
	}
	if !at20.Has(HF19) {
		t.Fatal("HF19 must already be active during HF20 activation block")
	}

	// At activation_block + 1, HF20 active.
	at20Next := SetForBlock(MainnetActivationBlock[HF20] + 1)
	if !at20Next.Has(HF20) {
		t.Fatal("HF20 must be active one block after activation")
	}
	if at20Next.Has(HF21) {
		t.Fatal("HF21 must not be active at HF20 activation+1")
	}
	if at20Next.HighestActive() != HF20 {
		t.Fatalf("HighestActive at HF20 activation+1: got %d, want %d", at20Next.HighestActive(), HF20)
	}

	// Far in the future: all 28 hardforks active.
	far := SetForBlock(1_000_000_000)
	for i := ID(0); i <= NumHardforks; i++ {
		if !far.Has(i) {
			t.Fatalf("HF%d must be active at far-future block", i)
		}
	}
}

func TestHasBoundsCheck(t *testing.T) {
	var s Set = 0xFFFFFFFF
	if s.Has(255) {
		t.Fatal("Has() must return false for out-of-range IDs")
	}
}
