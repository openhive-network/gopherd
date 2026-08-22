package proto

import (
	"fmt"

	"gopherd/internal/fcraw"
)

// ItemIDsInventoryMessage mirrors graphene::net::item_ids_inventory_message.
//
// FC_REFLECT order:
//
//	item_type             uint32 LE
//	item_hashes_available vector<item_hash_t>     (varuint count + 20-byte hashes)
type ItemIDsInventoryMessage struct {
	ItemType            uint32
	ItemHashesAvailable []ItemHash
}

// Type returns the message type tag.
func (ItemIDsInventoryMessage) Type() MessageType { return ItemIDsInventoryMessageType }

// Marshal serializes the body.
func (m *ItemIDsInventoryMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteUint32(m.ItemType)
	w.WriteVarUint(uint32(len(m.ItemHashesAvailable)))
	out := w.Bytes()
	for i := range m.ItemHashesAvailable {
		out = append(out, m.ItemHashesAvailable[i][:]...)
	}
	return out, nil
}

// Unmarshal parses src into m.
func (m *ItemIDsInventoryMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.ItemType, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("item_ids_inventory.item_type: %w", err)
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return fmt.Errorf("item_ids_inventory.count: %w", err)
	}
	if err := r.CheckCount(count, 20); err != nil {
		return fmt.Errorf("item_ids: %w", err)
	}
	m.ItemHashesAvailable = make([]ItemHash, count)
	for i := range m.ItemHashesAvailable {
		if err := r.ReadInto(m.ItemHashesAvailable[i][:]); err != nil {
			return fmt.Errorf("item_ids_inventory[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return fmt.Errorf("item_ids_inventory: %d trailing bytes", r.Remaining())
	}
	return nil
}

// BlockchainItemIDsInventoryMessage mirrors
// graphene::net::blockchain_item_ids_inventory_message.
//
// FC_REFLECT order:
//
//	total_remaining_item_count  uint32 LE
//	item_type                   uint32 LE
//	item_hashes_available       vector<item_hash_t>
type BlockchainItemIDsInventoryMessage struct {
	TotalRemainingItemCount uint32
	ItemType                uint32
	ItemHashesAvailable     []ItemHash
}

// Type returns the message type tag.
func (BlockchainItemIDsInventoryMessage) Type() MessageType {
	return BlockchainItemIDsInventoryMessageType
}

// Marshal serializes the body.
func (m *BlockchainItemIDsInventoryMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteUint32(m.TotalRemainingItemCount)
	w.WriteUint32(m.ItemType)
	w.WriteVarUint(uint32(len(m.ItemHashesAvailable)))
	out := w.Bytes()
	for i := range m.ItemHashesAvailable {
		out = append(out, m.ItemHashesAvailable[i][:]...)
	}
	return out, nil
}

// Unmarshal parses src into m.
func (m *BlockchainItemIDsInventoryMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.TotalRemainingItemCount, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("blockchain_item_ids_inventory.total_remaining: %w", err)
	}
	if m.ItemType, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("blockchain_item_ids_inventory.item_type: %w", err)
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return fmt.Errorf("blockchain_item_ids_inventory.count: %w", err)
	}
	if err := r.CheckCount(count, 20); err != nil {
		return fmt.Errorf("item_ids: %w", err)
	}
	m.ItemHashesAvailable = make([]ItemHash, count)
	for i := range m.ItemHashesAvailable {
		if err := r.ReadInto(m.ItemHashesAvailable[i][:]); err != nil {
			return fmt.Errorf("blockchain_item_ids_inventory[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return fmt.Errorf("blockchain_item_ids_inventory: %d trailing bytes", r.Remaining())
	}
	return nil
}

// FetchBlockchainItemIDsMessage mirrors
// graphene::net::fetch_blockchain_item_ids_message.
//
// FC_REFLECT order:
//
//	item_type            uint32 LE
//	blockchain_synopsis  vector<item_hash_t>
type FetchBlockchainItemIDsMessage struct {
	ItemType           uint32
	BlockchainSynopsis []ItemHash
}

// Type returns the message type tag.
func (FetchBlockchainItemIDsMessage) Type() MessageType { return FetchBlockchainItemIDsMessageType }

// Marshal serializes the body.
func (m *FetchBlockchainItemIDsMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteUint32(m.ItemType)
	w.WriteVarUint(uint32(len(m.BlockchainSynopsis)))
	out := w.Bytes()
	for i := range m.BlockchainSynopsis {
		out = append(out, m.BlockchainSynopsis[i][:]...)
	}
	return out, nil
}

// Unmarshal parses src into m.
func (m *FetchBlockchainItemIDsMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.ItemType, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("fetch_blockchain_item_ids.item_type: %w", err)
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return fmt.Errorf("fetch_blockchain_item_ids.count: %w", err)
	}
	if err := r.CheckCount(count, 20); err != nil {
		return fmt.Errorf("synopsis: %w", err)
	}
	m.BlockchainSynopsis = make([]ItemHash, count)
	for i := range m.BlockchainSynopsis {
		if err := r.ReadInto(m.BlockchainSynopsis[i][:]); err != nil {
			return fmt.Errorf("fetch_blockchain_item_ids[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return fmt.Errorf("fetch_blockchain_item_ids: %d trailing bytes", r.Remaining())
	}
	return nil
}

// FetchItemsMessage mirrors graphene::net::fetch_items_message.
//
// FC_REFLECT order:
//
//	item_type        uint32 LE
//	items_to_fetch   vector<item_hash_t>
type FetchItemsMessage struct {
	ItemType     uint32
	ItemsToFetch []ItemHash
}

// Type returns the message type tag.
func (FetchItemsMessage) Type() MessageType { return FetchItemsMessageType }

// Marshal serializes the body.
func (m *FetchItemsMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteUint32(m.ItemType)
	w.WriteVarUint(uint32(len(m.ItemsToFetch)))
	out := w.Bytes()
	for i := range m.ItemsToFetch {
		out = append(out, m.ItemsToFetch[i][:]...)
	}
	return out, nil
}

// Unmarshal parses src into m.
func (m *FetchItemsMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.ItemType, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("fetch_items.item_type: %w", err)
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return fmt.Errorf("fetch_items.count: %w", err)
	}
	if err := r.CheckCount(count, 20); err != nil {
		return fmt.Errorf("items_to_fetch: %w", err)
	}
	m.ItemsToFetch = make([]ItemHash, count)
	for i := range m.ItemsToFetch {
		if err := r.ReadInto(m.ItemsToFetch[i][:]); err != nil {
			return fmt.Errorf("fetch_items[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return fmt.Errorf("fetch_items: %d trailing bytes", r.Remaining())
	}
	return nil
}

// ItemNotAvailableMessage mirrors graphene::net::item_not_available_message.
//
// FC_REFLECT order:
//
//	requested_item   item_id  (uint32 item_type + 20-byte item_hash)
type ItemNotAvailableMessage struct {
	RequestedItem ItemID
}

// Type returns the message type tag.
func (ItemNotAvailableMessage) Type() MessageType { return ItemNotAvailableMessageType }

// Marshal serializes the body.
func (m *ItemNotAvailableMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteUint32(m.RequestedItem.ItemType)
	w.WriteRaw(m.RequestedItem.ItemHash[:])
	return w.Bytes(), nil
}

// Unmarshal parses src into m.
func (m *ItemNotAvailableMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.RequestedItem.ItemType, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("item_not_available.item_type: %w", err)
	}
	if err := r.ReadInto(m.RequestedItem.ItemHash[:]); err != nil {
		return fmt.Errorf("item_not_available.item_hash: %w", err)
	}
	if !r.AtEnd() {
		return fmt.Errorf("item_not_available: %d trailing bytes", r.Remaining())
	}
	return nil
}

// BlockMessage carries a serialized signed_block followed by its 20-byte
// block_id. See libraries/net/core_messages.cpp:66 for the wire format —
// the C++ message constructor concatenates the uncompressed block bytes
// with a memcpy of the block_id. There is no extra length prefix.
//
// On the receive side hived currently *ignores* the trailing block_id and
// re-derives it from the block bytes (core_messages.cpp:97 comment), but it
// remains on the wire for backwards compatibility, so we honour it.
type BlockMessage struct {
	// SignedBlockBytes is the FC-binary encoded signed_block (header +
	// witness signature + transactions). Callers that already have a
	// serialized block out of the block_log can pass those bytes through
	// without re-encoding.
	SignedBlockBytes []byte
	// BlockID is the 20-byte block ID. If left at the zero value at Marshal
	// time, EncodeBody will derive it from SignedBlockBytes.
	BlockID ItemHash
}

// Type returns the message type tag.
func (BlockMessage) Type() MessageType { return BlockMessageType }

// Marshal serializes the body. The block_id field is appended verbatim;
// callers that want the hived "ignored" behaviour can leave it zero.
func (m *BlockMessage) Marshal() ([]byte, error) {
	out := make([]byte, 0, len(m.SignedBlockBytes)+ItemHashSize)
	out = append(out, m.SignedBlockBytes...)
	out = append(out, m.BlockID[:]...)
	return out, nil
}

// Unmarshal parses src into m.
func (m *BlockMessage) Unmarshal(src []byte) error {
	if len(src) < ItemHashSize {
		return fmt.Errorf("block_message: %d bytes < trailing block_id (%d)", len(src), ItemHashSize)
	}
	bodyEnd := len(src) - ItemHashSize
	m.SignedBlockBytes = append([]byte(nil), src[:bodyEnd]...)
	copy(m.BlockID[:], src[bodyEnd:])
	return nil
}

// CompressedBlockMessage carries a zstd-compressed signed_block.
//
// Wire format (libraries/net/core_messages.cpp:114):
//
//	byte 0   bit 7  → zstd flag       (0x80 = zstd-compressed)
//	         bit 6  → dictionary flag (0x40 = a dictionary number follows)
//	byte 1   uint8  → dictionary number when bit 6 set, else 0
//	bytes 2+        → compressed payload bytes
type CompressedBlockMessage struct {
	Zstd             bool
	DictionaryNumber uint8
	HasDictionary    bool
	CompressedBytes  []byte
}

// Type returns the message type tag.
func (CompressedBlockMessage) Type() MessageType { return CompressedBlockMessageType }

// Marshal serializes the body.
func (m *CompressedBlockMessage) Marshal() ([]byte, error) {
	out := make([]byte, 2, 2+len(m.CompressedBytes))
	if m.Zstd {
		out[0] |= 0x80
	}
	if m.HasDictionary {
		out[0] |= 0x40
		out[1] = m.DictionaryNumber
	}
	out = append(out, m.CompressedBytes...)
	return out, nil
}

// Unmarshal parses src into m.
func (m *CompressedBlockMessage) Unmarshal(src []byte) error {
	if len(src) < 2 {
		return fmt.Errorf("compressed_block_message: %d bytes < 2-byte flag prefix", len(src))
	}
	m.Zstd = (src[0] & 0x80) != 0
	m.HasDictionary = (src[0] & 0x40) != 0
	if m.HasDictionary {
		m.DictionaryNumber = src[1]
	}
	m.CompressedBytes = append([]byte(nil), src[2:]...)
	return nil
}

// TrxMessage carries a serialized signed_transaction.
//
// Wire format (libraries/net/core_messages.cpp:164): the entire body is
// the FC-binary form of signed_transaction (no length prefix, no trailing
// id). The receiver re-derives the transaction ID from the bytes.
type TrxMessage struct {
	SignedTxBytes []byte
}

// Type returns the message type tag.
func (TrxMessage) Type() MessageType { return TrxMessageType }

// Marshal serializes the body.
func (m *TrxMessage) Marshal() ([]byte, error) {
	out := append([]byte(nil), m.SignedTxBytes...)
	return out, nil
}

// Unmarshal parses src into m.
func (m *TrxMessage) Unmarshal(src []byte) error {
	m.SignedTxBytes = append([]byte(nil), src...)
	return nil
}
