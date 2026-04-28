package hardfork

import "testing"

func TestActivationOrdering(t *testing.T) {
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

func TestSetForBlock(t *testing.T) {
	// Genesis: only HF0.
	if !SetForBlock(0).Has(HF0) {
		t.Fatal("HF0 must always be active")
	}
	if SetForBlock(0).Has(HF1) {
		t.Fatal("HF1 must not be active at block 0")
	}

	// At HF20 activation, HF20 active and HF21 not.
	at20 := SetForBlock(MainnetActivationBlock[HF20])
	if !at20.Has(HF20) {
		t.Fatal("HF20 must be active at its activation block")
	}
	if at20.Has(HF21) {
		t.Fatal("HF21 must not be active at HF20 activation")
	}
	if at20.HighestActive() != HF20 {
		t.Fatalf("HighestActive at HF20 block: got %d, want %d", at20.HighestActive(), HF20)
	}

	// One block before HF20 activation, HF20 must NOT be active.
	just := SetForBlock(MainnetActivationBlock[HF20] - 1)
	if just.Has(HF20) {
		t.Fatal("HF20 must not yet be active one block before activation")
	}
	if !just.Has(HF19) {
		t.Fatal("HF19 must already be active one block before HF20 activation")
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
