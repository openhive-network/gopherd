package objects

import (
	"testing"

	"gopherd/internal/state"
)

func TestStateStampDataRoundTrip(t *testing.T) {
	s := state.NewSession(state.OpenMem())
	if _, ok, err := GetStateStampData(s); ok || err != nil {
		t.Fatalf("empty state: ok=%v err=%v", ok, err)
	}
	for _, d := range []StateStampData{
		{},
		{Stamps: [][32]byte{{1}, {2, 3}}},
		{Stamps: [][32]byte{{9}}, Incomplete: true},
	} {
		PutStateStampData(s, &d)
		got, ok, err := GetStateStampData(s)
		if !ok || err != nil {
			t.Fatalf("get: ok=%v err=%v", ok, err)
		}
		if got.Incomplete != d.Incomplete || len(got.Stamps) != len(d.Stamps) {
			t.Fatalf("got %+v, want %+v", got, d)
		}
		for i := range d.Stamps {
			if got.Stamps[i] != d.Stamps[i] {
				t.Fatalf("stamp %d: got %x, want %x", i, got.Stamps[i], d.Stamps[i])
			}
		}
	}
	// The empty list is two bytes: no flags, zero count.
	if enc := AppendStateStampData(nil, &StateStampData{}); len(enc) != 2 || enc[0] != 0 || enc[1] != 0 {
		t.Fatalf("empty encoding %x", enc)
	}
	for _, bad := range [][]byte{{}, {2, 0}, {0, 1, 0}, {0, 0, 0}} {
		if _, err := ReadStateStampData(bad); err == nil {
			t.Errorf("decoded malformed row %x", bad)
		}
	}
}
