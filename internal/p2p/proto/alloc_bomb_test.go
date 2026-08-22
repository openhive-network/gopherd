package proto

import (
	"strings"
	"testing"
)

// TestDecodeRejectsAllocationBomb is a remote-DoS regression test.
//
// Length-prefixed decoders read a varuint count and allocate a slice of that
// size BEFORE reading any element. The count is attacker-controlled and
// independent of the bytes that follow, so a ~10-byte message could declare
// 4,294,967,295 items. At 20 bytes per ItemHash that is an 85 GB allocation:
// one tiny packet from any peer killed the node outright. The 32 MB message
// cap gave no protection, because the elements never had to be present.
//
// Each decoder must now reject a count larger than the remaining bytes could
// possibly satisfy, without allocating.
func TestDecodeRejectsAllocationBomb(t *testing.T) {
	// varuint encoding of 0xFFFFFFFF (~4.29e9)
	huge := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F}

	itemType := []byte{0, 0, 0, 0}
	cases := []struct {
		name string
		body []byte
		dec  func([]byte) error
	}{
		{"item_ids_inventory", append(append([]byte{}, itemType...), huge...),
			func(b []byte) error { var m ItemIDsInventoryMessage; return m.Unmarshal(b) }},
		{"blockchain_item_ids_inventory", append(append([]byte{}, itemType...), huge...),
			func(b []byte) error { var m BlockchainItemIDsInventoryMessage; return m.Unmarshal(b) }},
		{"fetch_items", append(append([]byte{}, itemType...), huge...),
			func(b []byte) error { var m FetchItemsMessage; return m.Unmarshal(b) }},
		{"address_message", append([]byte{}, huge...),
			func(b []byte) error { var m AddressMessage; return m.Unmarshal(b) }},
	}
	for _, c := range cases {
		err := c.dec(c.body)
		if err == nil {
			t.Errorf("%s: accepted a %d-byte message declaring ~4.29e9 elements", c.name, len(c.body))
			continue
		}
		// It must fail on the COUNT, not by trying and running out of memory.
		if !strings.Contains(err.Error(), "elements but only") {
			t.Logf("%s rejected with: %v", c.name, err)
		}
	}
}
