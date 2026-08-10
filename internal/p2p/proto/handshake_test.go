package proto

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"testing"
)

func TestHelloMessageRoundtrip(t *testing.T) {
	o := NewVarObject()
	o.Set("platform", NewVariantString("linux"))
	o.Set("bitness", NewVariantUint64(64))

	m := HelloMessage{
		UserAgent:           "ghive/0.1",
		CoreProtocolVersion: CoreProtocolVersion,
		InboundAddress:      netip.MustParseAddr("203.0.113.42"),
		InboundPort:         2001,
		OutboundPort:        65000,
		UserData:            o,
	}
	for i := range m.NodePublicKey {
		m.NodePublicKey[i] = byte(i + 1)
	}
	for i := range m.SignedSharedSecret {
		m.SignedSharedSecret[i] = byte(0xff - i)
	}

	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got HelloMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.UserAgent != m.UserAgent {
		t.Errorf("UserAgent: got %q, want %q", got.UserAgent, m.UserAgent)
	}
	if got.CoreProtocolVersion != m.CoreProtocolVersion {
		t.Errorf("CoreProtocolVersion mismatch")
	}
	if got.InboundAddress != m.InboundAddress {
		t.Errorf("InboundAddress: got %v, want %v", got.InboundAddress, m.InboundAddress)
	}
	if got.InboundPort != m.InboundPort || got.OutboundPort != m.OutboundPort {
		t.Errorf("ports: got %d/%d, want %d/%d", got.InboundPort, got.OutboundPort, m.InboundPort, m.OutboundPort)
	}
	if got.NodePublicKey != m.NodePublicKey {
		t.Errorf("NodePublicKey mismatch")
	}
	if got.SignedSharedSecret != m.SignedSharedSecret {
		t.Errorf("SignedSharedSecret mismatch")
	}
	if !m.UserData.Equal(got.UserData) {
		t.Errorf("UserData mismatch")
	}

	body2, _ := got.Marshal()
	if !bytes.Equal(body, body2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", body2, body)
	}
}

// TestHelloMessageHexFixture decodes a hand-constructed hello_message body
// and verifies the field layout matches the FC_REFLECT order.
func TestHelloMessageHexFixture(t *testing.T) {
	// Constructed from the FC_REFLECT order in core_messages.hpp:
	//   user_agent              = "ua"
	//   core_protocol_version   = 106 (CoreProtocolVersion)
	//   inbound_address         = 127.0.0.1 (v4; on the wire as uint32 LE,
	//                             i.e. octets reversed — see TestIPv4WireByteOrder)
	//   inbound_port            = 0x1234
	//   outbound_port           = 0x5678
	//   node_public_key         = 33 bytes 00..20
	//   signed_shared_secret    = 65 bytes ff..bf
	//   user_data               = empty variant_object
	hexFixture := "" +
		"02" + "7561" + // user_agent: varuint(2) + "ua"
		"6a000000" + // core_protocol_version = 106 LE
		"04" + "0100007f" + // address: v4 family + 127.0.0.1 (uint32 LE -> octets reversed)
		"3412" + // inbound_port LE
		"7856" + // outbound_port LE
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" + // 33-byte pubkey
		"fffefdfcfbfaf9f8f7f6f5f4f3f2f1f0" +
		"efeeedecebeae9e8e7e6e5e4e3e2e1e0" +
		"dfdedddcdbdad9d8d7d6d5d4d3d2d1d0" +
		"cfcecdcccbcac9c8c7c6c5c4c3c2c1c0" +
		"bf" +
		"00" // empty variant_object: varuint(0)
	body, err := hex.DecodeString(hexFixture)
	if err != nil {
		t.Fatal(err)
	}
	var m HelloMessage
	if err := m.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal hex fixture: %v", err)
	}
	if m.UserAgent != "ua" {
		t.Errorf("UserAgent: got %q", m.UserAgent)
	}
	if m.CoreProtocolVersion != 106 {
		t.Errorf("CoreProtocolVersion: got %d", m.CoreProtocolVersion)
	}
	if m.InboundAddress != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("InboundAddress: got %v", m.InboundAddress)
	}
	if m.InboundPort != 0x1234 {
		t.Errorf("InboundPort: got %#x", m.InboundPort)
	}
	if m.OutboundPort != 0x5678 {
		t.Errorf("OutboundPort: got %#x", m.OutboundPort)
	}
	if m.NodePublicKey[0] != 0x00 || m.NodePublicKey[32] != 0x20 {
		t.Errorf("NodePublicKey: got %x", m.NodePublicKey)
	}
	if m.SignedSharedSecret[0] != 0xff || m.SignedSharedSecret[64] != 0xbf {
		t.Errorf("SignedSharedSecret: got %x", m.SignedSharedSecret)
	}
	if m.UserData == nil || m.UserData.Len() != 0 {
		t.Errorf("UserData: got %v, want empty", m.UserData)
	}

	body2, _ := m.Marshal()
	if !bytes.Equal(body, body2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", body2, body)
	}
}

func TestConnectionAcceptedMessage(t *testing.T) {
	var m ConnectionAcceptedMessage
	body, err := m.Marshal()
	if err != nil || len(body) != 0 {
		t.Fatalf("Marshal: body=%v err=%v", body, err)
	}
	if err := (&ConnectionAcceptedMessage{}).Unmarshal(nil); err != nil {
		t.Fatalf("Unmarshal nil: %v", err)
	}
	if err := (&ConnectionAcceptedMessage{}).Unmarshal([]byte{0xff}); err == nil {
		t.Fatal("expected error on non-empty body")
	}
}

func TestConnectionRejectedMessageRoundtrip(t *testing.T) {
	m := ConnectionRejectedMessage{
		UserAgent:           "hived/1.27",
		CoreProtocolVersion: 106,
		RemoteEndpoint:      MakeEndpoint(netip.MustParseAddr("198.51.100.7"), 2001),
		ReasonCode:          RejectClientTooOld,
		ReasonString:        "your version is too old",
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ConnectionRejectedMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.UserAgent != m.UserAgent || got.CoreProtocolVersion != m.CoreProtocolVersion ||
		got.RemoteEndpoint.Addr.Addr() != m.RemoteEndpoint.Addr.Addr() ||
		got.RemoteEndpoint.Addr.Port() != m.RemoteEndpoint.Addr.Port() ||
		got.ReasonCode != m.ReasonCode || got.ReasonString != m.ReasonString {
		t.Fatalf("connection_rejected mismatch: got %+v want %+v", got, m)
	}
	body2, _ := got.Marshal()
	if !bytes.Equal(body, body2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", body2, body)
	}
}

func TestAddressMessageRoundtrip(t *testing.T) {
	m := AddressMessage{
		Addresses: []AddressInfo{
			{
				RemoteEndpoint: MakeEndpoint(netip.MustParseAddr("1.2.3.4"), 1234),
				LastSeenTime:   1700000000,
				LatencyUs:      12345,
				Direction:      DirectionOutbound,
				Firewalled:     FirewalledNotFirewalled,
			},
			{
				RemoteEndpoint: MakeEndpoint(netip.MustParseAddr("2001:db8::42"), 4321),
				LastSeenTime:   1700001000,
				LatencyUs:      999,
				Direction:      DirectionInbound,
				Firewalled:     FirewalledYes,
			},
		},
	}
	for i := range m.Addresses[0].NodeID {
		m.Addresses[0].NodeID[i] = byte(i)
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got AddressMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.Addresses) != len(m.Addresses) {
		t.Fatalf("addresses count: got %d, want %d", len(got.Addresses), len(m.Addresses))
	}
	body2, _ := got.Marshal()
	if !bytes.Equal(body, body2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", body2, body)
	}
}

func TestAddressRequestMessage(t *testing.T) {
	body, err := AddressRequestMessage{}.Marshal()
	if err != nil || len(body) != 0 {
		t.Fatalf("Marshal: %v %v", body, err)
	}
	if err := (&AddressRequestMessage{}).Unmarshal(nil); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
}

func TestClosingConnectionMessageRoundtrip(t *testing.T) {
	m := ClosingConnectionMessage{
		ReasonForClosing:  "peer is shutting down",
		ClosingDueToError: false,
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ClosingConnectionMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ReasonForClosing != m.ReasonForClosing || got.ClosingDueToError != m.ClosingDueToError ||
		got.ErrorPresent != m.ErrorPresent {
		t.Fatalf("closing_connection mismatch: got %+v want %+v", got, m)
	}
	body2, _ := got.Marshal()
	if !bytes.Equal(body, body2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", body2, body)
	}
}

func TestClosingConnectionMessageWithErrorBytes(t *testing.T) {
	m := ClosingConnectionMessage{
		ReasonForClosing:  "kaboom",
		ClosingDueToError: true,
		ErrorPresent:      true,
		ErrorBytes:        []byte{0x01, 0x02, 0x03, 0x04, 0x05},
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ClosingConnectionMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ReasonForClosing != m.ReasonForClosing || got.ClosingDueToError != m.ClosingDueToError ||
		!got.ErrorPresent || !bytes.Equal(got.ErrorBytes, m.ErrorBytes) {
		t.Fatalf("closing_connection mismatch: got %+v want %+v", got, m)
	}
}

func TestCurrentTimeRequestRoundtrip(t *testing.T) {
	m := CurrentTimeRequestMessage{RequestSentTime: 1234567890123456}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got CurrentTimeRequestMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != m {
		t.Fatalf("current_time_request mismatch: got %+v want %+v", got, m)
	}
}

func TestCurrentTimeReplyRoundtrip(t *testing.T) {
	m := CurrentTimeReplyMessage{
		RequestSentTime:      1234567890123456,
		RequestReceivedTime:  1234567890200000,
		ReplyTransmittedTime: 1234567890300000,
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got CurrentTimeReplyMessage
	if err := got.Unmarshal(body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != m {
		t.Fatalf("current_time_reply mismatch: got %+v want %+v", got, m)
	}
}

// TestEndToEndHelloEnvelope encodes a hello body, wraps it in the envelope,
// pipes the bytes through DecodeMessage, and re-decodes the body. Proves
// the envelope and message layers compose end-to-end.
func TestEndToEndHelloEnvelope(t *testing.T) {
	o := NewVarObject()
	o.Set("platform", NewVariantString("linux"))

	m := HelloMessage{
		UserAgent:           "ghive/0.1",
		CoreProtocolVersion: CoreProtocolVersion,
		InboundAddress:      netip.MustParseAddr("127.0.0.1"),
		InboundPort:         2001,
		OutboundPort:        2001,
		UserData:            o,
	}
	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	enc, err := EncodeMessage(m.Type(), body)
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}

	gotType, gotBody, err := DecodeMessage(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if gotType != m.Type() {
		t.Fatalf("type: got %d, want %d", gotType, m.Type())
	}
	var got HelloMessage
	if err := got.Unmarshal(gotBody); err != nil {
		t.Fatalf("HelloMessage.Unmarshal: %v", err)
	}
	if got.UserAgent != m.UserAgent {
		t.Fatalf("end-to-end mismatch: %+v vs %+v", got, m)
	}
}
