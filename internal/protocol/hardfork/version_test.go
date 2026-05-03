package hardfork

import "testing"

// TestPackedVersionFormat ports a slice of serialization_tests.cpp::
// hardfork_version_test. Verifies the (major, minor) packing into v_num
// matches the C++ encoding: major in bits 24-31, minor in bits 16-23,
// rev in bits 0-15 (0 for hardfork_version).
func TestPackedVersionFormat(t *testing.T) {
	cases := []struct {
		id       ID
		wantPack uint32
	}{
		{HF0, 0x00000000},
		{HF1, 0x00010000},
		{HF2, 0x00020000},
		{HF22, 0x00160000},
		{HF23, 0x00170000},
		{HF24, 0x01180000},
		{HF25, 0x01190000},
		{HF26, 0x011a0000},
		{HF27, 0x011b0000},
		{HF28, 0x011c0000},
	}
	for _, c := range cases {
		got := PackedVersion(c.id)
		if got != c.wantPack {
			t.Errorf("PackedVersion(HF%d): got %#x, want %#x", c.id, got, c.wantPack)
		}
	}
}

// TestPackedVersionMonotonic verifies the packed version increases
// monotonically across HF1 → HF28. Tested independently of the bit
// layout so a future repacking still satisfies "newer HF > older HF".
func TestPackedVersionMonotonic(t *testing.T) {
	prev := PackedVersion(HF0)
	for id := ID(1); id <= NumHardforks; id++ {
		got := PackedVersion(id)
		if got <= prev {
			t.Errorf("PackedVersion(HF%d)=%d should be greater than HF%d=%d",
				id, got, id-1, prev)
		}
		prev = got
	}
}
