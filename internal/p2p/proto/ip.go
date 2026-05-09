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
		// fc::ipv4_address::addr is a uint32 in host byte order; on little-
		// endian hosts that places the low octet first. The C++ code
		// constructs the uint32 from a string like "127.0.0.1" by shifting
		// octets so that octet[0] occupies the low byte; on the wire that
		// emerges as <oct0,oct1,oct2,oct3>. We honour that here directly.
		b := a.As4()
		dst = append(dst, b[0], b[1], b[2], b[3])
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
		return netip.AddrFrom4(b), nil
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
	return append(dst, b[0], b[1], b[2], b[3]), nil
}

// ReadLegacyAddress decodes an fc::ip::legacy_address.
func ReadLegacyAddress(r *fcraw.Reader) (netip.Addr, error) {
	var b [4]byte
	if err := r.ReadInto(b[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("legacy_address: %w", err)
	}
	return netip.AddrFrom4(b), nil
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
