package proto

import (
	"bytes"
	"net/netip"
	"testing"

	"gopherd/internal/fcraw"
)

// TestIPv4WireByteOrder asserts the EXACT bytes an IPv4 address occupies on the
// wire, in both the family-tagged and legacy encodings.
//
// This is deliberately not a round-trip test. The existing round-trip tests
// passed for months against a decoder that read the four bytes in the wrong
// order, because an encoder with the same error cancels it out. The bug only
// surfaced in production: peers advertise their NAT addresses, we decoded
// 192.168.18.2 as 2.18.168.192, and dialled unrelated public hosts — 769 failed
// dials in 40 minutes. Symmetrically, peers recorded OUR address reversed and
// could never dial back.
//
// Ground truth (libraries/fc/include/fc/network/ip.hpp + src/network/ip.cpp):
//
//	address(const string& s) -> ipv4_address(boost::asio::...to_v4().to_uint())
//	pack(ip::ipv4_address)   -> fc::raw::pack(s, v.addr)   // uint32, LITTLE endian
//
// boost's to_uint() places octet[0] in the HIGH byte (1.2.3.4 -> 0x01020304),
// so a little-endian uint32 emits the octets REVERSED: <4,3,2,1>.
func TestIPv4WireByteOrder(t *testing.T) {
	cases := []struct {
		addr string
		wire []byte // the 4 address bytes, after any family tag
	}{
		{"1.2.3.4", []byte{4, 3, 2, 1}},
		{"192.168.18.2", []byte{2, 18, 168, 192}}, // the real NAT address we mis-dialled
		{"127.0.0.1", []byte{1, 0, 0, 127}},
		{"172.31.14.10", []byte{10, 14, 31, 172}}, // decoded as 10.14.31.172 before the fix
		{"255.254.253.252", []byte{252, 253, 254, 255}},
	}

	for _, tc := range cases {
		a := netip.MustParseAddr(tc.addr)

		// legacy_address: bare uint32 LE.
		got, err := AppendLegacyAddress(nil, a)
		if err != nil {
			t.Fatalf("AppendLegacyAddress(%s): %v", tc.addr, err)
		}
		if !bytes.Equal(got, tc.wire) {
			t.Fatalf("legacy encode %s: got %v, want %v (fc packs the uint32 little-endian with octet[0] in the high byte)",
				tc.addr, got, tc.wire)
		}

		// address: family tag 0x04 + the same uint32 LE.
		gotT := AppendAddress(nil, a)
		wantT := append([]byte{ipFamilyV4}, tc.wire...)
		if !bytes.Equal(gotT, wantT) {
			t.Fatalf("tagged encode %s: got %v, want %v", tc.addr, gotT, wantT)
		}

		// Decoders must invert exactly, reading real hived-produced bytes.
		if dec, err := ReadLegacyAddress(fcraw.NewReader(tc.wire)); err != nil || dec != a {
			t.Fatalf("legacy decode %v: got %v (err=%v), want %s — this is the dial-garbage bug",
				tc.wire, dec, err, tc.addr)
		}
		if dec, err := ReadAddress(fcraw.NewReader(wantT)); err != nil || dec != a {
			t.Fatalf("tagged decode %v: got %v (err=%v), want %s", wantT, dec, err, tc.addr)
		}
	}
}

// TestIPv4PrivateAddressesDecodeAsPrivate is the property that would have caught
// this in the field without knowing fc's encoding at all: peers overwhelmingly
// advertise RFC1918 addresses, so if our decoder is byte-reversed those arrive
// looking like routable public IPs and we dial strangers.
func TestIPv4PrivateAddressesDecodeAsPrivate(t *testing.T) {
	for _, s := range []string{"192.168.1.5", "10.0.0.7", "172.16.4.9", "172.31.198.145"} {
		a := netip.MustParseAddr(s)
		wire, err := AppendLegacyAddress(nil, a)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := ReadLegacyAddress(fcraw.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		if !dec.IsPrivate() {
			t.Fatalf("%s round-tripped to %s, which is NOT private — a reversed decode turns peers' NAT addresses into public hosts we then dial", s, dec)
		}
	}
}
