package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Envelope wire format
// -------------------------------------------------------------------
// libraries/net/include/graphene/net/message.hpp defines the on-wire header:
//
//   struct message_header {
//     uint32_t size;       // length of the message body in bytes (unpadded)
//     uint32_t msg_type;   // core_message_type_enum value
//   };
//
// Followed by `size` bytes of body, then enough zero-padding to round
// (HeaderSize + size) up to a multiple of 16. The padding exists because
// stcp_socket transports data in 16-byte Blowfish blocks; both sender and
// receiver always read/write whole blocks.
//
// libraries/net/message_oriented_connection.cpp:
//   - send_message: builds a buffer of size = round_up_16(HeaderSize + body),
//     copies header + body, zeroes the tail, and writes it in one shot.
//   - read_loop:  reads BUFFER_SIZE=16 bytes (header + first 8 body bytes),
//     then reads round_up_16(size - 8) more bytes if needed, then truncates
//     the trailing padding by resizing data to `size`.
//
// HeaderSize is 8 (two uint32 LE). Note that the brief mentioned a
// 4-byte checksum field — that is incorrect for the actual wire format;
// hived's `id()` ripemd160 hashing is used in-memory for inventory dedup,
// not in the envelope. We follow the C++ implementation, not the brief.

// HeaderSize is the on-wire length of the envelope header (size + msg_type),
// in bytes.
const HeaderSize = 8

// PadAlign is the block alignment hived uses for the entire envelope
// (header + body + padding), driven by stcp's 16-byte Blowfish blocks.
const PadAlign = 16

// MaxMessageSize mirrors graphene::net::MAX_MESSAGE_SIZE in
// libraries/net/include/graphene/net/config.hpp. Currently 32 MB.
const MaxMessageSize = 32 * 1024 * 1024

// ErrMessageTooLarge is returned when a message body exceeds MaxMessageSize.
var ErrMessageTooLarge = errors.New("proto: message body exceeds MaxMessageSize")

// PaddedLen returns the on-wire length of an envelope with the given body
// size — header + body, rounded up to PadAlign.
func PaddedLen(bodyLen int) int {
	total := HeaderSize + bodyLen
	rem := total % PadAlign
	if rem == 0 {
		return total
	}
	return total + (PadAlign - rem)
}

// EncodeMessage builds the on-wire envelope for body with the given message
// type. The returned slice is a freshly allocated buffer of length
// PaddedLen(len(body)); the trailing padding bytes are zero.
func EncodeMessage(typeTag MessageType, body []byte) ([]byte, error) {
	if len(body) > MaxMessageSize {
		return nil, fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, len(body), MaxMessageSize)
	}
	out := make([]byte, PaddedLen(len(body)))
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(body)))
	binary.LittleEndian.PutUint32(out[4:8], uint32(typeTag))
	copy(out[HeaderSize:], body)
	return out, nil
}

// DecodeMessage reads one full envelope from r: it reads exactly
// PaddedLen(size) bytes, returns the type tag and an exact-length copy of
// the body (padding stripped).
//
// If r returns io.EOF before any header byte is read, DecodeMessage returns
// io.EOF unchanged so callers can detect a clean disconnect. A short header
// or a short body returns io.ErrUnexpectedEOF.
func DecodeMessage(r io.Reader) (MessageType, []byte, error) {
	var hdr [HeaderSize]byte
	n, err := io.ReadFull(r, hdr[:])
	if err != nil {
		// io.ReadFull collapses 0-byte EOF to io.EOF and partial-read EOF to
		// io.ErrUnexpectedEOF — that's exactly what we want, so propagate.
		_ = n
		return 0, nil, err
	}
	bodyLen := binary.LittleEndian.Uint32(hdr[0:4])
	typeTag := MessageType(binary.LittleEndian.Uint32(hdr[4:8]))
	if int(bodyLen) > MaxMessageSize {
		return typeTag, nil, fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, bodyLen, MaxMessageSize)
	}
	padded := PaddedLen(int(bodyLen))
	rest := padded - HeaderSize
	if rest == 0 {
		return typeTag, []byte{}, nil
	}
	buf := make([]byte, rest)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return typeTag, nil, err
	}
	body := buf[:bodyLen:bodyLen]
	return typeTag, body, nil
}
