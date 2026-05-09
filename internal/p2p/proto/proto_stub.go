// Package proto carries the wire-level message types for hived's P2P
// protocol. THIS IS A STUB — agent-p2p-msg owns the full implementation
// (every type in core_messages.hpp, with FC-correct field encoding).
//
// We define just enough here for transport + connection-state work to
// build and round-trip; the merge with agent-p2p-msg's branch will
// replace these stubs with the canonical types and a richer codec.
//
// What's stubbed:
//   - The MessageType enum values (matched to core_message_type_enum
//     so the wire bytes stay correct after merge).
//   - HelloMessage, ConnectionAcceptedMessage, ConnectionRejectedMessage,
//     CurrentTimeRequestMessage, CurrentTimeReplyMessage as opaque
//     byte-payload structs. Until agent-p2p-msg lands, the body is
//     application-supplied (callers serialize/deserialize themselves).
//   - EncodeMessage / DecodeMessage that bridge a (typeTag, body)
//     pair to/from the framed stream the transport speaks. Frame layout
//     mirrors graphene::net::message_header:
//       uint32_le size       — payload bytes (NOT including this header)
//       uint32_le msg_type   — core_message_type_enum value
//       byte[size]   payload — caller-supplied body
//     The transport on top adds 16-byte padding for AES-CBC alignment.
package proto

import (
	"encoding/binary"
	"errors"
	"io"
)

// MessageType matches graphene::net::core_message_type_enum.
type MessageType uint32

const (
	BlockMessage                MessageType = 1000
	TransactionMessage          MessageType = 1001
	BlockIDsInventoryMessage    MessageType = 5001
	BlocksInventoryMessage      MessageType = 5002
	BlockIDsRequestMessage      MessageType = 5003
	BlocksRequestMessage        MessageType = 5004
	ItemNotAvailableMessage     MessageType = 5005
	LegacyHelloMessage          MessageType = 5006
	ConnectionAcceptedMessage   MessageType = 5007
	LegacyConnectionRejected    MessageType = 5008
	AddressRequestMessage       MessageType = 5009
	LegacyAddressMessage        MessageType = 5010
	ClosingConnectionMessage    MessageType = 5011
	CurrentTimeRequestMessage   MessageType = 5012
	CurrentTimeReplyMessage     MessageType = 5013
	CheckFirewallMessage        MessageType = 5014
	CheckFirewallReplyMessage   MessageType = 5015
	GetCurrentConnectionsRequest MessageType = 5016
	GetCurrentConnectionsReply  MessageType = 5017
	HelloMessage                MessageType = 5018
	ConnectionRejectedMessage   MessageType = 5019
	AddressMessage              MessageType = 5020
)

// MaxMessageSize matches MAX_MESSAGE_SIZE in graphene/net/config.hpp.
// The transport rejects frames larger than this on read.
const MaxMessageSize = 1024*1024*2 + 20

// HeaderSize is the on-wire size of message_header (size:u32, msg_type:u32).
const HeaderSize = 8

// EncodeMessage frames a (typeTag, body) pair into the byte sequence the
// transport puts on the wire BEFORE AES padding/encryption.
//
// Layout: [size_u32_le][type_u32_le][body...].
// Returns ErrTooLarge if body exceeds MaxMessageSize.
func EncodeMessage(typeTag MessageType, body []byte) ([]byte, error) {
	if len(body) > MaxMessageSize {
		return nil, ErrTooLarge
	}
	out := make([]byte, HeaderSize+len(body))
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(body)))
	binary.LittleEndian.PutUint32(out[4:8], uint32(typeTag))
	copy(out[HeaderSize:], body)
	return out, nil
}

// DecodeMessage reads one framed message from r. The transport is
// responsible for un-padding the trailing AES block before passing
// bytes to this function.
//
// Returns the message type, body slice (length = header.size), and
// any I/O error.
func DecodeMessage(r io.Reader) (MessageType, []byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	size := binary.LittleEndian.Uint32(hdr[0:4])
	typeTag := MessageType(binary.LittleEndian.Uint32(hdr[4:8]))
	if size > MaxMessageSize {
		return 0, nil, ErrTooLarge
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typeTag, body, nil
}

// ErrTooLarge is returned when a body exceeds MaxMessageSize. Mirrors the
// FC_ASSERT in message_oriented_connection's read_loop.
var ErrTooLarge = errors.New("p2p/proto: message exceeds MAX_MESSAGE_SIZE")
