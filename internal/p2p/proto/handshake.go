package proto

import (
	"errors"
	"fmt"
	"net/netip"

	"gopherd/internal/fcraw"
)

// LegacyHelloMessage is graphene::net::legacy_hello_message — the IPv4-only
// version of HelloMessage used by older hived peers (mainnet seeds in
// particular). Same field set as HelloMessage but uses fc::ip::legacy_address
// (raw uint32 LE, no family byte) for inbound_address.
//
// Wire-format note: hived's send_hello_message in node.cpp:4854 sends BOTH
// the new (5018) and the legacy (5006) hello back-to-back so older peers
// that don't recognize 5018 can still handshake on 5006. We emit the same
// pair on every outbound dial for the same reason — without the legacy
// hello, mainnet seeds stuck on a pre-IPv6 hived will silently drop the
// connection after sending us their own legacy hello.
type LegacyHelloMessage struct {
	UserAgent           string
	CoreProtocolVersion uint32
	InboundAddress      netip.Addr // must be IPv4-only
	InboundPort         uint16
	OutboundPort        uint16
	NodePublicKey       PublicKeyData
	SignedSharedSecret  CompactSignature
	UserData            *VarObject
}

// Type returns the message type tag.
func (LegacyHelloMessage) Type() MessageType { return LegacyHelloMessageType }

// Marshal returns the FC binary encoding of m's body (no envelope header).
func (m *LegacyHelloMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteString(m.UserAgent)
	w.WriteUint32(m.CoreProtocolVersion)
	out, err := AppendLegacyAddress(w.Bytes(), m.InboundAddress)
	if err != nil {
		return nil, fmt.Errorf("legacy_hello.inbound_address: %w", err)
	}
	out = append(out, byte(m.InboundPort), byte(m.InboundPort>>8))
	out = append(out, byte(m.OutboundPort), byte(m.OutboundPort>>8))
	out = append(out, m.NodePublicKey[:]...)
	out = append(out, m.SignedSharedSecret[:]...)
	out = AppendVariantObject(out, m.UserData)
	return out, nil
}

// Unmarshal parses src into m.
func (m *LegacyHelloMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.UserAgent, err = r.ReadString(); err != nil {
		return fmt.Errorf("legacy_hello.user_agent: %w", err)
	}
	if m.CoreProtocolVersion, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("legacy_hello.core_protocol_version: %w", err)
	}
	addr, err := ReadLegacyAddress(r)
	if err != nil {
		return fmt.Errorf("legacy_hello.inbound_address: %w", err)
	}
	m.InboundAddress = addr
	if m.InboundPort, err = r.ReadUint16(); err != nil {
		return fmt.Errorf("legacy_hello.inbound_port: %w", err)
	}
	if m.OutboundPort, err = r.ReadUint16(); err != nil {
		return fmt.Errorf("legacy_hello.outbound_port: %w", err)
	}
	if err := r.ReadInto(m.NodePublicKey[:]); err != nil {
		return fmt.Errorf("legacy_hello.node_public_key: %w", err)
	}
	if err := r.ReadInto(m.SignedSharedSecret[:]); err != nil {
		return fmt.Errorf("legacy_hello.signed_shared_secret: %w", err)
	}
	o, err := ReadVariantObject(r)
	if err != nil {
		return fmt.Errorf("legacy_hello.user_data: %w", err)
	}
	m.UserData = o
	if !r.AtEnd() {
		return fmt.Errorf("legacy_hello: %d trailing bytes", r.Remaining())
	}
	return nil
}

// HelloMessage is graphene::net::hello_message — the first message a peer
// sends after the stcp handshake.
//
// FC_REFLECT order:
//
//	user_agent              string
//	core_protocol_version   uint32 LE
//	inbound_address         fc::ip::address  (family + body)
//	inbound_port            uint16 LE
//	outbound_port           uint16 LE
//	node_public_key         fc::ecc::public_key_data  (33 raw bytes)
//	signed_shared_secret    fc::ecc::compact_signature (65 raw bytes)
//	user_data               fc::variant_object
type HelloMessage struct {
	UserAgent           string
	CoreProtocolVersion uint32
	InboundAddress      netip.Addr
	InboundPort         uint16
	OutboundPort        uint16
	NodePublicKey       PublicKeyData
	SignedSharedSecret  CompactSignature
	UserData            *VarObject
}

// Type returns the message type tag.
func (HelloMessage) Type() MessageType { return HelloMessageType }

// Marshal returns the FC binary encoding of m's body (no envelope header).
func (m *HelloMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteString(m.UserAgent)
	w.WriteUint32(m.CoreProtocolVersion)
	out := AppendAddress(w.Bytes(), m.InboundAddress)
	out = append(out, byte(m.InboundPort), byte(m.InboundPort>>8))
	out = append(out, byte(m.OutboundPort), byte(m.OutboundPort>>8))
	out = append(out, m.NodePublicKey[:]...)
	out = append(out, m.SignedSharedSecret[:]...)
	out = AppendVariantObject(out, m.UserData)
	return out, nil
}

// Unmarshal parses src into m.
func (m *HelloMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.UserAgent, err = r.ReadString(); err != nil {
		return fmt.Errorf("hello.user_agent: %w", err)
	}
	if m.CoreProtocolVersion, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("hello.core_protocol_version: %w", err)
	}
	addr, err := ReadAddress(r)
	if err != nil {
		return fmt.Errorf("hello.inbound_address: %w", err)
	}
	m.InboundAddress = addr
	if m.InboundPort, err = r.ReadUint16(); err != nil {
		return fmt.Errorf("hello.inbound_port: %w", err)
	}
	if m.OutboundPort, err = r.ReadUint16(); err != nil {
		return fmt.Errorf("hello.outbound_port: %w", err)
	}
	if err := r.ReadInto(m.NodePublicKey[:]); err != nil {
		return fmt.Errorf("hello.node_public_key: %w", err)
	}
	if err := r.ReadInto(m.SignedSharedSecret[:]); err != nil {
		return fmt.Errorf("hello.signed_shared_secret: %w", err)
	}
	o, err := ReadVariantObject(r)
	if err != nil {
		return fmt.Errorf("hello.user_data: %w", err)
	}
	m.UserData = o
	if !r.AtEnd() {
		return fmt.Errorf("hello: %d trailing bytes", r.Remaining())
	}
	return nil
}

// ConnectionAcceptedMessage is empty (FC_REFLECT_EMPTY).
type ConnectionAcceptedMessage struct{}

// Type returns the message type tag.
func (ConnectionAcceptedMessage) Type() MessageType { return ConnectionAcceptedMessageType }

// Marshal of an empty message returns a zero-length body.
func (ConnectionAcceptedMessage) Marshal() ([]byte, error) { return []byte{}, nil }

// Unmarshal asserts the body is empty.
func (*ConnectionAcceptedMessage) Unmarshal(src []byte) error {
	if len(src) != 0 {
		return fmt.Errorf("connection_accepted: expected empty body, got %d bytes", len(src))
	}
	return nil
}

// RejectionReasonCode mirrors graphene::net::rejection_reason_code.
type RejectionReasonCode uint8

const (
	RejectUnspecified             RejectionReasonCode = 0
	RejectDifferentChain          RejectionReasonCode = 1
	RejectAlreadyConnected        RejectionReasonCode = 2
	RejectConnectedToSelf         RejectionReasonCode = 3
	RejectNotAcceptingConnections RejectionReasonCode = 4
	RejectBlocked                 RejectionReasonCode = 5
	RejectInvalidHelloMessage     RejectionReasonCode = 6
	RejectClientTooOld            RejectionReasonCode = 7
)

// ConnectionRejectedMessage mirrors graphene::net::connection_rejected_message.
//
// FC_REFLECT order:
//
//	user_agent              string
//	core_protocol_version   uint32 LE
//	remote_endpoint         fc::ip::endpoint  (address + uint16 port)
//	reason_code             uint8 (fc::enum_type<uint8_t, rejection_reason_code>)
//	reason_string           string
type ConnectionRejectedMessage struct {
	UserAgent           string
	CoreProtocolVersion uint32
	RemoteEndpoint      Endpoint
	ReasonCode          RejectionReasonCode
	ReasonString        string
}

// Type returns the message type tag.
func (ConnectionRejectedMessage) Type() MessageType { return ConnectionRejectedMessageType }

// Marshal serializes the body.
func (m *ConnectionRejectedMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteString(m.UserAgent)
	w.WriteUint32(m.CoreProtocolVersion)
	out := AppendEndpoint(w.Bytes(), m.RemoteEndpoint)
	out = append(out, byte(m.ReasonCode))
	out = fcraw.AppendVarUint(out, uint32(len(m.ReasonString)))
	out = append(out, m.ReasonString...)
	return out, nil
}

// Unmarshal parses src into m.
func (m *ConnectionRejectedMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.UserAgent, err = r.ReadString(); err != nil {
		return fmt.Errorf("connection_rejected.user_agent: %w", err)
	}
	if m.CoreProtocolVersion, err = r.ReadUint32(); err != nil {
		return fmt.Errorf("connection_rejected.core_protocol_version: %w", err)
	}
	if m.RemoteEndpoint, err = ReadEndpoint(r); err != nil {
		return fmt.Errorf("connection_rejected.remote_endpoint: %w", err)
	}
	rc, err := r.ReadByte()
	if err != nil {
		return fmt.Errorf("connection_rejected.reason_code: %w", err)
	}
	m.ReasonCode = RejectionReasonCode(rc)
	if m.ReasonString, err = r.ReadString(); err != nil {
		return fmt.Errorf("connection_rejected.reason_string: %w", err)
	}
	if !r.AtEnd() {
		return fmt.Errorf("connection_rejected: %d trailing bytes", r.Remaining())
	}
	return nil
}

// PeerConnectionDirection mirrors graphene::net::peer_connection_direction.
type PeerConnectionDirection uint8

const (
	DirectionUnknown  PeerConnectionDirection = 0
	DirectionInbound  PeerConnectionDirection = 1
	DirectionOutbound PeerConnectionDirection = 2
)

// FirewalledState mirrors graphene::net::firewalled_state.
type FirewalledState uint8

const (
	FirewalledUnknown       FirewalledState = 0
	FirewalledYes           FirewalledState = 1
	FirewalledNotFirewalled FirewalledState = 2
)

// AddressInfo mirrors graphene::net::address_info.
//
// FC_REFLECT order:
//
//	remote_endpoint  fc::ip::endpoint
//	last_seen_time   fc::time_point_sec  (uint32 LE)
//	latency          fc::microseconds    (uint64 LE)
//	node_id          fc::ecc::public_key_data (33 raw bytes)
//	direction        uint8
//	firewalled       uint8
type AddressInfo struct {
	RemoteEndpoint Endpoint
	LastSeenTime   uint32 // seconds since epoch
	LatencyUs      uint64 // microseconds
	NodeID         PublicKeyData
	Direction      PeerConnectionDirection
	Firewalled     FirewalledState
}

// AppendAddressInfo marshals one address_info to dst.
func AppendAddressInfo(dst []byte, a AddressInfo) []byte {
	dst = AppendEndpoint(dst, a.RemoteEndpoint)
	dst = append(dst,
		byte(a.LastSeenTime), byte(a.LastSeenTime>>8), byte(a.LastSeenTime>>16), byte(a.LastSeenTime>>24))
	for i := 0; i < 8; i++ {
		dst = append(dst, byte(a.LatencyUs>>(i*8)))
	}
	dst = append(dst, a.NodeID[:]...)
	dst = append(dst, byte(a.Direction))
	dst = append(dst, byte(a.Firewalled))
	return dst
}

// ReadAddressInfo decodes one address_info from r.
func ReadAddressInfo(r *fcraw.Reader) (AddressInfo, error) {
	var a AddressInfo
	ep, err := ReadEndpoint(r)
	if err != nil {
		return a, fmt.Errorf("address_info.remote_endpoint: %w", err)
	}
	a.RemoteEndpoint = ep
	if a.LastSeenTime, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("address_info.last_seen_time: %w", err)
	}
	if a.LatencyUs, err = r.ReadUint64(); err != nil {
		return a, fmt.Errorf("address_info.latency: %w", err)
	}
	if err := r.ReadInto(a.NodeID[:]); err != nil {
		return a, fmt.Errorf("address_info.node_id: %w", err)
	}
	d, err := r.ReadByte()
	if err != nil {
		return a, fmt.Errorf("address_info.direction: %w", err)
	}
	a.Direction = PeerConnectionDirection(d)
	f, err := r.ReadByte()
	if err != nil {
		return a, fmt.Errorf("address_info.firewalled: %w", err)
	}
	a.Firewalled = FirewalledState(f)
	return a, nil
}

// AddressMessage mirrors graphene::net::address_message.
//
// FC_REFLECT order:
//
//	addresses  vector<address_info>
type AddressMessage struct {
	Addresses []AddressInfo
}

// Type returns the message type tag.
func (AddressMessage) Type() MessageType { return AddressMessageType }

// Marshal serializes the body.
func (m *AddressMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteVarUint(uint32(len(m.Addresses)))
	out := w.Bytes()
	for _, a := range m.Addresses {
		out = AppendAddressInfo(out, a)
	}
	return out, nil
}

// Unmarshal parses src into m.
func (m *AddressMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	count, err := r.ReadVarUint()
	if err != nil {
		return fmt.Errorf("address_message.count: %w", err)
	}
	if err := r.CheckCount(count, 8); err != nil {
		return fmt.Errorf("address_message: %w", err)
	}
	m.Addresses = make([]AddressInfo, count)
	for i := range m.Addresses {
		if m.Addresses[i], err = ReadAddressInfo(r); err != nil {
			return fmt.Errorf("address_message[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return fmt.Errorf("address_message: %d trailing bytes", r.Remaining())
	}
	return nil
}

// AddressRequestMessage is empty (FC_REFLECT_EMPTY).
type AddressRequestMessage struct{}

// Type returns the message type tag.
func (AddressRequestMessage) Type() MessageType { return AddressRequestMessageType }

// Marshal of an empty message.
func (AddressRequestMessage) Marshal() ([]byte, error) { return []byte{}, nil }

// Unmarshal asserts the body is empty.
func (*AddressRequestMessage) Unmarshal(src []byte) error {
	if len(src) != 0 {
		return fmt.Errorf("address_request: expected empty body, got %d bytes", len(src))
	}
	return nil
}

// ClosingConnectionMessage mirrors graphene::net::closing_connection_message.
//
// FC_REFLECT order:
//
//	reason_for_closing     string
//	closing_due_to_error   bool
//	error                  fc::oexception   (1-byte present-flag, then optional payload)
//
// fc::oexception is fc::optional<fc::exception>. fc::exception is
// pack-encoded as int64 code + string name + string what + log_messages
// vector. Hived almost never sends a non-empty error in practice, and the
// log_messages vector itself contains nested fc::variant payloads which the
// p2p code only emits via fc::raw::pack of the std::shared_ptr<exception>
// the throw site captured.
//
// For testnet purposes we expose the present-flag as a bool plus an opaque
// byte slice carrying the encoded exception, so a node can pass a hived-
// originated error through unchanged. ErrorPresent=false → the flag byte is
// emitted as 0 and ErrorBytes is ignored.
type ClosingConnectionMessage struct {
	ReasonForClosing  string
	ClosingDueToError bool
	ErrorPresent      bool
	ErrorBytes        []byte // raw fc::exception body when ErrorPresent
}

// Type returns the message type tag.
func (ClosingConnectionMessage) Type() MessageType { return ClosingConnectionMessageType }

// Marshal serializes the body.
func (m *ClosingConnectionMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteString(m.ReasonForClosing)
	w.WriteBool(m.ClosingDueToError)
	if m.ErrorPresent {
		w.WriteUint8(1)
		w.WriteRaw(m.ErrorBytes)
	} else {
		w.WriteUint8(0)
	}
	return w.Bytes(), nil
}

// Unmarshal parses src into m. If the optional fc::exception is present, its
// bytes are captured verbatim into ErrorBytes (i.e. parsing past the optional
// flag is left as a TODO since hived rarely emits one in practice).
func (m *ClosingConnectionMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.ReasonForClosing, err = r.ReadString(); err != nil {
		return fmt.Errorf("closing_connection.reason_for_closing: %w", err)
	}
	if m.ClosingDueToError, err = r.ReadBool(); err != nil {
		return fmt.Errorf("closing_connection.closing_due_to_error: %w", err)
	}
	flag, err := r.ReadByte()
	if err != nil {
		return fmt.Errorf("closing_connection.error.flag: %w", err)
	}
	switch flag {
	case 0:
		m.ErrorPresent = false
	case 1:
		m.ErrorPresent = true
		// Capture remaining bytes as the opaque exception payload. A full
		// fc::exception decoder is out of scope for the testnet handshake;
		// preserving the original bytes lets us round-trip-test envelopes
		// produced elsewhere.
		m.ErrorBytes = append([]byte(nil), src[r.Pos():]...)
		// Advance the reader to the end so the trailing-bytes check passes.
		if _, err := r.ReadRawAlias(r.Remaining()); err != nil {
			return err
		}
	default:
		return fmt.Errorf("closing_connection.error.flag: invalid byte 0x%02x", flag)
	}
	if !r.AtEnd() {
		return fmt.Errorf("closing_connection: %d trailing bytes", r.Remaining())
	}
	return nil
}

// CurrentTimeRequestMessage is graphene::net::current_time_request_message.
//
// FC_REFLECT: (request_sent_time)  → fc::time_point (uint64 LE microseconds)
type CurrentTimeRequestMessage struct {
	RequestSentTime uint64 // microseconds since epoch
}

// Type returns the message type tag.
func (CurrentTimeRequestMessage) Type() MessageType { return CurrentTimeRequestMessageType }

// Marshal serializes the body.
func (m *CurrentTimeRequestMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteUint64(m.RequestSentTime)
	return w.Bytes(), nil
}

// Unmarshal parses src into m.
func (m *CurrentTimeRequestMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	t, err := r.ReadUint64()
	if err != nil {
		return fmt.Errorf("current_time_request.request_sent_time: %w", err)
	}
	m.RequestSentTime = t
	if !r.AtEnd() {
		return fmt.Errorf("current_time_request: %d trailing bytes", r.Remaining())
	}
	return nil
}

// CurrentTimeReplyMessage is graphene::net::current_time_reply_message.
//
// FC_REFLECT order:
//
//	request_sent_time         fc::time_point (uint64 LE us)
//	request_received_time     fc::time_point (uint64 LE us)
//	reply_transmitted_time    fc::time_point (uint64 LE us)
type CurrentTimeReplyMessage struct {
	RequestSentTime      uint64
	RequestReceivedTime  uint64
	ReplyTransmittedTime uint64
}

// Type returns the message type tag.
func (CurrentTimeReplyMessage) Type() MessageType { return CurrentTimeReplyMessageType }

// Marshal serializes the body.
func (m *CurrentTimeReplyMessage) Marshal() ([]byte, error) {
	w := fcraw.NewWriter()
	w.WriteUint64(m.RequestSentTime)
	w.WriteUint64(m.RequestReceivedTime)
	w.WriteUint64(m.ReplyTransmittedTime)
	return w.Bytes(), nil
}

// Unmarshal parses src into m.
func (m *CurrentTimeReplyMessage) Unmarshal(src []byte) error {
	r := fcraw.NewReader(src)
	var err error
	if m.RequestSentTime, err = r.ReadUint64(); err != nil {
		return fmt.Errorf("current_time_reply.request_sent_time: %w", err)
	}
	if m.RequestReceivedTime, err = r.ReadUint64(); err != nil {
		return fmt.Errorf("current_time_reply.request_received_time: %w", err)
	}
	if m.ReplyTransmittedTime, err = r.ReadUint64(); err != nil {
		return fmt.Errorf("current_time_reply.reply_transmitted_time: %w", err)
	}
	if !r.AtEnd() {
		return fmt.Errorf("current_time_reply: %d trailing bytes", r.Remaining())
	}
	return nil
}

// ErrEmptyBody is returned when an empty-body message is given non-zero data.
var ErrEmptyBody = errors.New("proto: empty-body message expected zero-length body")
