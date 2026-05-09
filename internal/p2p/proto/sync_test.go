package proto

import (
	"bytes"
	"testing"
)

func TestItemIDsInventoryRoundtrip(t *testing.T) {
	m := ItemIDsInventoryMessage{
		ItemType: 1001,
		ItemHashesAvailable: []ItemHash{
			{0x01, 0x02, 0x03},
			{0xa1, 0xa2, 0xa3, 0xa4},
		},
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ItemIDsInventoryMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ItemType != m.ItemType || len(got.ItemHashesAvailable) != len(m.ItemHashesAvailable) {
		t.Fatalf("item_ids_inventory mismatch: got %+v want %+v", got, m)
	}
	for i := range m.ItemHashesAvailable {
		if got.ItemHashesAvailable[i] != m.ItemHashesAvailable[i] {
			t.Fatalf("hash[%d] mismatch", i)
		}
	}
}

func TestBlockchainItemIDsInventoryRoundtrip(t *testing.T) {
	m := BlockchainItemIDsInventoryMessage{
		TotalRemainingItemCount: 12345,
		ItemType:                1001,
		ItemHashesAvailable:     []ItemHash{{0xfe, 0xed}},
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got BlockchainItemIDsInventoryMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.TotalRemainingItemCount != m.TotalRemainingItemCount || got.ItemType != m.ItemType {
		t.Fatalf("blockchain_item_ids_inventory mismatch: got %+v want %+v", got, m)
	}
}

func TestFetchBlockchainItemIDsRoundtrip(t *testing.T) {
	m := FetchBlockchainItemIDsMessage{
		ItemType:           1001,
		BlockchainSynopsis: []ItemHash{{0xab}, {0xcd}, {0xef}},
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got FetchBlockchainItemIDsMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ItemType != m.ItemType || len(got.BlockchainSynopsis) != 3 {
		t.Fatalf("fetch_blockchain_item_ids mismatch: got %+v", got)
	}
}

func TestFetchItemsRoundtrip(t *testing.T) {
	m := FetchItemsMessage{
		ItemType:     1001,
		ItemsToFetch: []ItemHash{{0x01}},
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got FetchItemsMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ItemType != m.ItemType || got.ItemsToFetch[0] != m.ItemsToFetch[0] {
		t.Fatalf("fetch_items mismatch: got %+v want %+v", got, m)
	}
}

func TestItemNotAvailableRoundtrip(t *testing.T) {
	m := ItemNotAvailableMessage{
		RequestedItem: ItemID{ItemType: 1001, ItemHash: ItemHash{0xc0, 0xff, 0xee}},
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ItemNotAvailableMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.RequestedItem != m.RequestedItem {
		t.Fatalf("item_not_available mismatch: got %+v want %+v", got, m)
	}
}

func TestBlockMessageRoundtrip(t *testing.T) {
	m := BlockMessage{
		SignedBlockBytes: []byte{0xde, 0xad, 0xbe, 0xef, 0x12, 0x34, 0x56, 0x78},
		BlockID:          ItemHash{0x00, 0x01, 0x86, 0xa0}, // first 4 bytes = block num 100000 BE
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(body) != len(m.SignedBlockBytes)+ItemHashSize {
		t.Fatalf("block_message body length: got %d, want %d", len(body), len(m.SignedBlockBytes)+ItemHashSize)
	}
	var got BlockMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !bytes.Equal(got.SignedBlockBytes, m.SignedBlockBytes) || got.BlockID != m.BlockID {
		t.Fatalf("block_message mismatch")
	}
}

func TestCompressedBlockMessageRoundtrip(t *testing.T) {
	m := CompressedBlockMessage{
		Zstd:             true,
		HasDictionary:    true,
		DictionaryNumber: 7,
		CompressedBytes:  []byte{0x28, 0xb5, 0x2f, 0xfd}, // zstd magic prefix
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if body[0]&0x80 == 0 || body[0]&0x40 == 0 || body[1] != 7 {
		t.Fatalf("compressed_block flag header: %x", body[:2])
	}
	var got CompressedBlockMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Zstd || !got.HasDictionary || got.DictionaryNumber != 7 ||
		!bytes.Equal(got.CompressedBytes, m.CompressedBytes) {
		t.Fatalf("compressed_block mismatch: got %+v want %+v", got, m)
	}
}

func TestTrxMessageRoundtrip(t *testing.T) {
	m := TrxMessage{SignedTxBytes: []byte("not a real signed_transaction but fine for round-trip")}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got TrxMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !bytes.Equal(got.SignedTxBytes, m.SignedTxBytes) {
		t.Fatalf("trx_message mismatch")
	}
}
