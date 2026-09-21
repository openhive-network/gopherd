// Package proto implements the hived P2P wire-protocol message catalog.
//
// Every Hive p2p message is wrapped in a tiny envelope and serialized with
// fc::raw (FC binary). The envelope is defined in
// libraries/net/include/graphene/net/message.hpp:
//
//	struct message_header {
//	    uint32_t size;       // length of the *unpadded* body in bytes
//	    uint32_t msg_type;   // core_message_type_enum value
//	};
//	// followed by `size` body bytes
//	// followed by zero-padding so that (header + body) is a multiple of 16 bytes
//
// The 16-byte alignment exists because the underlying transport
// (graphene::net::stcp_socket) is Blowfish-encrypted in 16-byte blocks; the
// padding lets the receiver always read in 16-byte chunks. There is NO
// checksum in the envelope — fc::ripemd160 hashing is only used in-memory
// for inventory IDs (`message::id()`), never on the wire.
//
// This package ports the core_messages.hpp catalog plus the envelope. Each
// message type has a Marshal / Unmarshal pair that produces and consumes the
// FC binary form of the message body (the bytes that go between the
// 8-byte header and the padding).
package proto

// CoreProtocolVersion is the value hived sends in hello_message's
// core_protocol_version field. It mirrors GRAPHENE_NET_PROTOCOL_VERSION,
// defined as 107 in libraries/net/include/graphene/net/config.hpp.
//
// The bump from 106 → 107 unlocks two peer-side behaviors:
//
//	GRAPHENE_NET_PROTOCOL_COMPRESSED_BLOCKS_VERSION (107) — peers will send
//	  us compressed_block_message during sync IF we also advertise a
//	  last_available_zstd_compression_dictionary_number in user_data.
//	  We do — see internal/p2p/node/node.go::buildHelloPayload.
//	GRAPHENE_NET_PROTOCOL_ADVERTISE_BLOCKS_BY_BLOCK_ID_VERSION (107) —
//	  peers may advertise inventory by block_id rather than block-message
//	  hash. Our handleInv accepts both via ItemTypeBlock /
//	  ItemTypeBlockCompressed.
const CoreProtocolVersion uint32 = 107

// MessageType is the on-wire numeric tag used in the envelope's msg_type
// field. Values match core_message_type_enum exactly; do NOT renumber.
type MessageType uint32

const (
	TrxMessageType                          MessageType = 1000
	BlockMessageType                        MessageType = 1001
	CompressedBlockMessageType              MessageType = 1002
	CoreMessageTypeFirst                    MessageType = 5000
	ItemIDsInventoryMessageType             MessageType = 5001
	BlockchainItemIDsInventoryMessageType   MessageType = 5002
	FetchBlockchainItemIDsMessageType       MessageType = 5003
	FetchItemsMessageType                   MessageType = 5004
	ItemNotAvailableMessageType             MessageType = 5005
	LegacyHelloMessageType                  MessageType = 5006
	ConnectionAcceptedMessageType           MessageType = 5007
	LegacyConnectionRejectedMessageType     MessageType = 5008
	AddressRequestMessageType               MessageType = 5009
	LegacyAddressMessageType                MessageType = 5010
	ClosingConnectionMessageType            MessageType = 5011
	CurrentTimeRequestMessageType           MessageType = 5012
	CurrentTimeReplyMessageType             MessageType = 5013
	LegacyCheckFirewallMessageType          MessageType = 5014
	LegacyCheckFirewallReplyMessageType     MessageType = 5015
	GetCurrentConnectionsRequestMessageType MessageType = 5016
	GetCurrentConnectionsReplyMessageType   MessageType = 5017
	HelloMessageType                        MessageType = 5018
	ConnectionRejectedMessageType           MessageType = 5019
	AddressMessageType                      MessageType = 5020
	CheckFirewallMessageType                MessageType = 5021
	CheckFirewallReplyMessageType           MessageType = 5022
	CoreMessageTypeLast                     MessageType = 5099
)

// String renders the wire tag with its symbolic hived name where one is
// known. Useful in logs / diff tooling that compare ghive vs hived sessions.
func (t MessageType) String() string {
	switch t {
	case TrxMessageType:
		return "trx_message"
	case BlockMessageType:
		return "block_message"
	case CompressedBlockMessageType:
		return "compressed_block_message"
	case ItemIDsInventoryMessageType:
		return "item_ids_inventory_message"
	case BlockchainItemIDsInventoryMessageType:
		return "blockchain_item_ids_inventory_message"
	case FetchBlockchainItemIDsMessageType:
		return "fetch_blockchain_item_ids_message"
	case FetchItemsMessageType:
		return "fetch_items_message"
	case ItemNotAvailableMessageType:
		return "item_not_available_message"
	case LegacyHelloMessageType:
		return "legacy_hello_message"
	case ConnectionAcceptedMessageType:
		return "connection_accepted_message"
	case LegacyConnectionRejectedMessageType:
		return "legacy_connection_rejected_message"
	case AddressRequestMessageType:
		return "address_request_message"
	case LegacyAddressMessageType:
		return "legacy_address_message"
	case ClosingConnectionMessageType:
		return "closing_connection_message"
	case CurrentTimeRequestMessageType:
		return "current_time_request_message"
	case CurrentTimeReplyMessageType:
		return "current_time_reply_message"
	case LegacyCheckFirewallMessageType:
		return "legacy_check_firewall_message"
	case LegacyCheckFirewallReplyMessageType:
		return "legacy_check_firewall_reply_message"
	case GetCurrentConnectionsRequestMessageType:
		return "get_current_connections_request_message"
	case GetCurrentConnectionsReplyMessageType:
		return "get_current_connections_reply_message"
	case HelloMessageType:
		return "hello_message"
	case ConnectionRejectedMessageType:
		return "connection_rejected_message"
	case AddressMessageType:
		return "address_message"
	case CheckFirewallMessageType:
		return "check_firewall_message"
	case CheckFirewallReplyMessageType:
		return "check_firewall_reply_message"
	}
	return "unknown_message_type"
}

// ItemHashSize is the on-wire length of an inventory item hash — fc::ripemd160,
// 20 bytes. Used by item_ids_inventory_message and friends.
const ItemHashSize = 20

// ItemHash is the 20-byte inventory hash (fc::ripemd160 / message_hash_type).
type ItemHash [ItemHashSize]byte

// PublicKeyDataSize is the size of fc::ecc::public_key_data: a 33-byte
// compressed secp256k1 public key. It is used in hello_message as
// node_public_key.
const PublicKeyDataSize = 33

// PublicKeyData is fc::ecc::public_key_data — a 33-byte secp256k1 compressed
// pubkey serialization.
type PublicKeyData [PublicKeyDataSize]byte

// CompactSignatureSize is the size of fc::ecc::compact_signature — 65 bytes
// (1 recovery byte + 32 r + 32 s).
const CompactSignatureSize = 65

// CompactSignature is fc::ecc::compact_signature.
type CompactSignature [CompactSignatureSize]byte

// ItemID mirrors graphene::net::item_id (item_type uint32 + item_hash).
type ItemID struct {
	ItemType uint32
	ItemHash ItemHash
}

// Inventory item-type identifiers used by the sync messages
// (item_ids_inventory_message, blockchain_item_ids_inventory_message,
// fetch_items_message, item_not_available_message). They reuse the
// numeric MessageType tag for the corresponding wire payload —
// graphene's core_message_type_enum overloads the same enum for both
// roles. Source: libraries/net/include/graphene/net/core_messages.hpp.
const (
	ItemTypeTrx             = uint32(TrxMessageType)             // 1000
	ItemTypeBlock           = uint32(BlockMessageType)           // 1001
	ItemTypeBlockCompressed = uint32(CompressedBlockMessageType) // 1002
)

// MinimumProtocolVersion mirrors GRAPHENE_NET_MINIMUM_PROTOCOL_VERSION
// (hive 1.29.0-rc2, commit eb556e22d). Until that commit no floor was ever
// enforced: core_protocol_version was read for feature detection only, so a
// nonconforming peer reporting nothing walked through every compatibility
// check. 106 shipped in the steem era and every client since reports 107, so
// the floor costs nothing and closes the "reports 0, gets treated as a peer"
// hole.
const MinimumProtocolVersion uint32 = 106

// AdvertiseBlocksByBlockIDVersion is the graphene core_protocol_version at
// which inventory identifies blocks by BLOCK ID rather than by the legacy
// block_message hash (hived config.hpp:
// GRAPHENE_NET_PROTOCOL_ADVERTISE_BLOCKS_BY_BLOCK_ID_VERSION, added in 107).
//
// We only speak the block-id form, so this is the floor for advertising to a
// peer at all: a peer below it reads our ids as legacy hashes, cannot match
// them, and requests something we do not have under that identity — which
// hived answers with a disconnect.
const AdvertiseBlocksByBlockIDVersion uint32 = 107
