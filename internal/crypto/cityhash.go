package crypto

import (
	"encoding/binary"
)

// CityHash128 is a port of fc::city_hash128, which is in turn the original
// Google CityHash 1.0.x 128-bit variant (no CRC). hived's stcp_socket uses
// this to derive the 128-bit IV for its AES-256-CBC stream:
//
//	shared = sha512(ECDH(priv, remote_pub).X)        // 64 bytes
//	key    = sha256(shared)                          // 32 bytes
//	iv128  = city_hash_crc_128(shared, sizeof shared)
//
// For inputs <= 900 bytes (and our SHA-512 is 64), city_hash_crc_128
// delegates to city_hash128 (no SSE4.2 CRC tail), so this single function
// covers all our needs.
//
// The result is returned as (low, high) 64-bit halves — matching how
// libfc's `to_uint128(hi, lo)` constructs the value (low at bytes 0..7,
// high at bytes 8..15 in little-endian memory).
func CityHash128(s []byte) (lo, hi uint64) {
	if len(s) >= 16 {
		// to_uint128(hi=Fetch64(s), lo=Fetch64(s+8)+k0)
		seedLow := fetch64(s[8:]) + chK0
		seedHigh := fetch64(s)
		return cityHash128WithSeed(s[16:], seedLow, seedHigh)
	}
	// to_uint128(hi=k0, lo=k1)
	return cityHash128WithSeed(s, chK1, chK0)
}

// CityHashIVBytes returns the IV exactly as hived's aes_encoder receives it.
//
// The C++ does: _init_value = to_uint128(low_bits(iv), high_bits(iv));
// then casts the resulting __uint128_t to bytes (little-endian on x86_64).
// In native uint128 layout, low 8 bytes sit at offset 0 and high 8 bytes
// at offset 8. Therefore the bytes the AES init sees are:
//
//	bytes[0..8]  = original_high (LE)   — was high_bits(iv)
//	bytes[8..16] = original_low  (LE)   — was low_bits(iv)
//
// i.e. the two 64-bit halves of the original uint128 are byte-swapped.
func CityHashIVBytes(s []byte) [16]byte {
	lo, hi := CityHash128(s)
	var out [16]byte
	binary.LittleEndian.PutUint64(out[0:8], hi)
	binary.LittleEndian.PutUint64(out[8:16], lo)
	return out
}

// --- internal port of CityHash 1.0 ---

const (
	chK0 uint64 = 0xc3a5c85c97cb3127
	chK1 uint64 = 0xb492b66fbe98f273
	chK2 uint64 = 0x9ae16a3b2f90404f
)

func fetch64(s []byte) uint64 { return binary.LittleEndian.Uint64(s) }

func rotate(val uint64, shift uint) uint64 {
	if shift == 0 {
		return val
	}
	return (val >> shift) | (val << (64 - shift))
}

func shiftMix(v uint64) uint64 { return v ^ (v >> 47) }

// hashLen16 = Hash128to64(to_uint128(u, v)). Note: this is NOT the same
// as the 3-arg HashLen16(u, v, mul). The 2-arg variant computes
//
//	a = (low ^ high) * kMul; a ^= a>>47
//	b = (high ^ a)   * kMul; b ^= b>>47; b *= kMul
//
// where to_uint128(u, v) puts u in the high half and v in the low half,
// so the second line is `(u ^ a) * kMul` — NOT `(v ^ a) * kMul` like the
// 3-arg version uses.
func hashLen16(u, v uint64) uint64 {
	const kMul uint64 = 0x9ddfea08eb382d69
	a := (v ^ u) * kMul // (low ^ high) * kMul
	a ^= a >> 47
	b := (u ^ a) * kMul // (high ^ a) * kMul
	b ^= b >> 47
	b *= kMul
	return b
}

// CityMurmur — the small-input fallback used by CityHash128WithSeed.
func cityMurmur(s []byte, seedLow, seedHigh uint64) (lo, hi uint64) {
	a := seedLow
	b := seedHigh
	var c, d uint64
	l := int64(len(s)) - 16
	if l <= 0 {
		a = shiftMix(a*chK1) * chK1
		c = b*chK1 + hashLen0to16(s)
		if len(s) >= 8 {
			d = shiftMix(a + fetch64(s))
		} else {
			d = shiftMix(a + c)
		}
	} else {
		c = hashLen16fast(fetch64(s[len(s)-8:])+chK1, a)
		d = hashLen16fast(b+uint64(len(s)), c+fetch64(s[len(s)-16:]))
		a += d
		off := 0
		for l > 0 {
			a ^= shiftMix(fetch64(s[off:])*chK1) * chK1
			a *= chK1
			b ^= a
			c ^= shiftMix(fetch64(s[off+8:])*chK1) * chK1
			c *= chK1
			d ^= c
			off += 16
			l -= 16
		}
	}
	a = hashLen16fast(a, c)
	b = hashLen16fast(d, b)
	// C++: to_uint128(a ^ b, HashLen16(b, a)) = (hi=a^b, lo=HashLen16(b,a))
	lo = hashLen16fast(b, a)
	hi = a ^ b
	return
}

// hashLen16fast is the same as hashLen16 — kept separate to make audit
// easier against the C++ which uses a single name.
func hashLen16fast(u, v uint64) uint64 { return hashLen16(u, v) }

// HashLen0to16 from the C++.
func hashLen0to16(s []byte) uint64 {
	n := len(s)
	if n >= 8 {
		mul := chK2 + uint64(n)*2
		a := fetch64(s) + chK2
		b := fetch64(s[n-8:])
		c := rotate(b, 37)*mul + a
		d := (rotate(a, 25) + b) * mul
		return hashLen16Mul(c, d, mul)
	}
	if n >= 4 {
		mul := chK2 + uint64(n)*2
		a := uint64(binary.LittleEndian.Uint32(s))
		return hashLen16Mul(uint64(n)+(a<<3), uint64(binary.LittleEndian.Uint32(s[n-4:])), mul)
	}
	if n > 0 {
		a := uint8(s[0])
		b := uint8(s[n>>1])
		c := uint8(s[n-1])
		y := uint32(a) + (uint32(b) << 8)
		z := uint32(n) + (uint32(c) << 2)
		return shiftMix(uint64(y)*chK2^uint64(z)*chK0) * chK2
	}
	return chK2
}

func hashLen16Mul(u, v, mul uint64) uint64 {
	a := (u ^ v) * mul
	a ^= a >> 47
	b := (v ^ a) * mul
	b ^= b >> 47
	b *= mul
	return b
}

// CityHash128WithSeed — for our use case (sha512 = 64 bytes) the chain is
// city_hash128 -> CityHash128WithSeed(s+16, 48, ...) -> CityMurmur(s, 48,
// seed) since 48 < 128. We keep the >= 128 path implemented for completeness
// in case future callers need longer inputs.
func cityHash128WithSeed(s []byte, seedLow, seedHigh uint64) (lo, hi uint64) {
	if len(s) < 128 {
		return cityMurmur(s, seedLow, seedHigh)
	}
	// Long-input path (CityHash 1.0 layout). Untested in our handshake but
	// mirrors the C++ exactly so future callers (e.g. msg framing) can rely
	// on it.
	x := seedLow
	y := seedHigh
	z := uint64(len(s)) * chK1
	v0 := rotate(y^chK1, 49)*chK1 + fetch64(s)
	v1 := rotate(v0, 42)*chK1 + fetch64(s[8:])
	w0 := rotate(y+z, 35)*chK1 + x
	w1 := rotate(x+fetch64(s[88:]), 53) * chK1
	off := 0
	rem := len(s)
	for {
		x = rotate(x+y+v0+fetch64(s[off+8:]), 37) * chK1
		y = rotate(y+v1+fetch64(s[off+48:]), 42) * chK1
		x ^= w1
		y += v0 + fetch64(s[off+40:])
		z = rotate(z+w0, 33) * chK1
		v0, v1 = weakHashLen32WithSeeds(s[off:off+32], v1*chK1, x+w0)
		w0, w1 = weakHashLen32WithSeeds(s[off+32:off+64], z+w1, y+fetch64(s[off+16:]))
		z, x = x, z
		off += 64
		x = rotate(x+y+v0+fetch64(s[off+8:]), 37) * chK1
		y = rotate(y+v1+fetch64(s[off+48:]), 42) * chK1
		x ^= w1
		y += v0 + fetch64(s[off+40:])
		z = rotate(z+w0, 33) * chK1
		v0, v1 = weakHashLen32WithSeeds(s[off:off+32], v1*chK1, x+w0)
		w0, w1 = weakHashLen32WithSeeds(s[off+32:off+64], z+w1, y+fetch64(s[off+16:]))
		z, x = x, z
		off += 64
		rem -= 128
		if rem < 128 {
			break
		}
	}
	x += rotate(v0+z, 49) * chK0
	y = y*chK0 + rotate(w1, 37)
	z = z*chK0 + rotate(w0, 27)
	w0 *= 9
	v0 *= chK0
	tail := s[off:]
	for tailDone := 0; tailDone < rem; {
		tailDone += 32
		y = rotate(x+y, 42)*chK0 + v1
		w0 += fetch64(tail[rem-tailDone+16:])
		x = x*chK0 + w0
		z += w1 + fetch64(tail[rem-tailDone:])
		w1 += v0
		v0, v1 = weakHashLen32WithSeeds(tail[rem-tailDone:rem-tailDone+32], v0+z, v1)
		v0 *= chK0
	}
	x = hashLen16fast(x, v0)
	y = hashLen16fast(y+z, w0)
	// C++: to_uint128(HashLen16(x+v.second, w.second)+y,
	//                 HashLen16(x+w.second, y+v.second))
	//   = (hi=first, lo=second)
	hi = hashLen16fast(x+v1, w1) + y
	lo = hashLen16fast(x+w1, y+v1)
	return
}

func weakHashLen32WithSeeds(s []byte, a, b uint64) (uint64, uint64) {
	w := fetch64(s)
	x := fetch64(s[8:])
	y := fetch64(s[16:])
	z := fetch64(s[24:])
	a += w
	b = rotate(b+a+z, 21)
	c := a
	a += x
	a += y
	b += rotate(a, 44)
	return a + z, b + c
}
