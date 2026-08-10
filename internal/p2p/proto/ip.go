package proto

import (
	"errors"
	"fmt"
	"net/netip"

	"hived-go/internal/fcraw"
)

// IP wire format
// -------------------------------------------------------------------
// `fc::ip::address` (the v6-aware variant used by every "non-legacy" message)
// is encoded as one family byte + the address bytes:
//
//   0x04  ipv4_address.addr  (4 bytes, LE uint32)
//   0x06  ipv6_address.addr  (16 raw bytes)
//
// (see libraries/fc/include/fc/network/ip.hpp around `fc::raw::pack`.)
//
// `fc::ip::endpoint` is just `address` followed by the 16-bit port LE.
//
// `fc::ip::legacy_address` and `fc::ip::legacy_endpoint` are the IPv4-only
// pre-existing wire format: legacy_address is a single uint32 LE; legacy_endpoint
// is uint32 LE (address) + uint16 LE (port).

const (
	ipFamilyV4 byte = 0x04
	ipFamilyV6 byte = 0x06
)

// IPv4 addresses go on the wire as a LITTLE-ENDIAN uint32 whose MOST
// significant byte is the FIRST octet — so the octets appear REVERSED in the
// byte stream. Both fc paths do this:
//
//	pack(ip::ipv4_address)  -> fc::raw::pack(s, v.addr)          // uint32 LE
//	pack(ip::address)       -> 0x04, then the same uint32 LE
//	address(const string&)  -> ipv4_address(boost .to_v4().to_uint())
//
// and boost's to_uint() returns the address with octet[0] in the HIGH byte
// (1.2.3.4 -> 0x01020304). Serialized little-endian that emits <4,3,2,1>.
//
// We used to read the four wire bytes straight as octets in both directions,
// which is invisible to a round-trip test (the error cancels itself) and was
// caught only in production: peers advertise their NAT/private addresses, we
// decoded 192.168.18.2 as 2.18.168.192, and then dialled that — 769 failed
// dials in 40 minutes, aimed at unrelated public hosts. The encode side was
// wrong symmetrically, so every peer we handshook with recorded OUR address
// reversed and could never dial us back.
func appendIPv4LE(dst []byte, b [4]byte) []byte {
	return append(dst, b[3], b[2], b[1], b[0])
}

func ipv4FromLE(b [4]byte) netip.Addr {
	return netip.AddrFrom4([4]byte{b[3], b[2], b[1], b[0]})
}

// Endpoint is a transport-level address: an IP plus a port. Wraps a
// netip.AddrPort so callers get standard-library address handling.
//
// Note: netip.Addr distinguishes IPv4 from IPv4-in-IPv6 (`::ffff:0.0.0.0/96`).
// For wire output we follow hived's rule (its `address::is_ipv4()` is true
// only for native IPv4), so we treat `Addr.Is4()` as v4 and everything else
// as v6 — including the IPv4-mapped form, which we emit as the 16-byte v6
// payload to match what hived would have produced after constructing an
// fc::ip::address from such bytes.
type Endpoint struct {
	Addr netip.AddrPort
}

// MakeEndpoint builds an Endpoint from an explicit address and port.
func MakeEndpoint(a netip.Addr, port uint16) Endpoint {
	return Endpoint{Addr: netip.AddrPortFrom(a, port)}
}

// AppendAddress encodes a fc::ip::address (v6-aware family-tagged form) to
// dst. Returns the extended slice.
func AppendAddress(dst []byte, a netip.Addr) []byte {
	if a.Is4() {
		dst = append(dst, ipFamilyV4)
		b := a.As4()
		dst = appendIPv4LE(dst, b)
		return dst
	}
	dst = append(dst, ipFamilyV6)
	b := a.As16()
	return append(dst, b[:]...)
}

// ReadAddress decodes a fc::ip::address from r. Returns the parsed address.
func ReadAddress(r *fcraw.Reader) (netip.Addr, error) {
	family, err := r.ReadByte()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("ip.address family: %w", err)
	}
	switch family {
	case ipFamilyV4:
		var b [4]byte
		if err := r.ReadInto(b[:]); err != nil {
			return netip.Addr{}, fmt.Errorf("ip.address v4 body: %w", err)
		}
		return ipv4FromLE(b), nil
	case ipFamilyV6:
		var b [16]byte
		if err := r.ReadInto(b[:]); err != nil {
			return netip.Addr{}, fmt.Errorf("ip.address v6 body: %w", err)
		}
		return netip.AddrFrom16(b), nil
	default:
		return netip.Addr{}, fmt.Errorf("ip.address: invalid family byte 0x%02x", family)
	}
}

// AppendEndpoint encodes a fc::ip::endpoint (address + uint16 port LE) to dst.
func AppendEndpoint(dst []byte, e Endpoint) []byte {
	dst = AppendAddress(dst, e.Addr.Addr())
	port := e.Addr.Port()
	return append(dst, byte(port), byte(port>>8))
}

// ReadEndpoint decodes a fc::ip::endpoint from r.
func ReadEndpoint(r *fcraw.Reader) (Endpoint, error) {
	addr, err := ReadAddress(r)
	if err != nil {
		return Endpoint{}, err
	}
	port, err := r.ReadUint16()
	if err != nil {
		return Endpoint{}, fmt.Errorf("ip.endpoint port: %w", err)
	}
	return Endpoint{Addr: netip.AddrPortFrom(addr, port)}, nil
}

// ErrNotIPv4 is returned when a v6-only address is fed into a legacy
// (IPv4-only) wire path.
var ErrNotIPv4 = errors.New("proto: legacy IP type only accepts IPv4 addresses")

// AppendLegacyAddress encodes an fc::ip::legacy_address (uint32 LE).
func AppendLegacyAddress(dst []byte, a netip.Addr) ([]byte, error) {
	if !a.Is4() {
		return dst, ErrNotIPv4
	}
	b := a.As4()
	return appendIPv4LE(dst, b), nil
}

// ReadLegacyAddress decodes an fc::ip::legacy_address.
func ReadLegacyAddress(r *fcraw.Reader) (netip.Addr, error) {
	var b [4]byte
	if err := r.ReadInto(b[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("legacy_address: %w", err)
	}
	return ipv4FromLE(b), nil
}

// AppendLegacyEndpoint encodes an fc::ip::legacy_endpoint (uint32 LE + uint16 LE).
func AppendLegacyEndpoint(dst []byte, e Endpoint) ([]byte, error) {
	dst, err := AppendLegacyAddress(dst, e.Addr.Addr())
	if err != nil {
		return dst, err
	}
	port := e.Addr.Port()
	return append(dst, byte(port), byte(port>>8)), nil
}

// ReadLegacyEndpoint decodes an fc::ip::legacy_endpoint.
func ReadLegacyEndpoint(r *fcraw.Reader) (Endpoint, error) {
	a, err := ReadLegacyAddress(r)
	if err != nil {
		return Endpoint{}, err
	}
	port, err := r.ReadUint16()
	if err != nil {
		return Endpoint{}, fmt.Errorf("legacy_endpoint port: %w", err)
	}
	return Endpoint{Addr: netip.AddrPortFrom(a, port)}, nil
}
