package chain

import (
	"math/big"
)

// uint128FromBytes decodes a 16-byte big-endian uint128 into a
// math/big.Int (non-negative). Used for VirtualScheduledTime arithmetic
// in update_witness_schedule_4 (everywhere C++ uses fc::uint128_t).
func uint128FromBytes(b [16]byte) *big.Int {
	return new(big.Int).SetBytes(b[:])
}

// uint128ToBytes serializes a non-negative big.Int as 16-byte big-endian
// (uint128 wire format). Higher bits beyond 128 are silently truncated;
// callers are responsible for ensuring v fits.
func uint128ToBytes(v *big.Int) [16]byte {
	var out [16]byte
	if v == nil || v.Sign() <= 0 {
		return out
	}
	src := v.Bytes()
	if len(src) > 16 {
		src = src[len(src)-16:]
	}
	copy(out[16-len(src):], src)
	return out
}

// uint128Max returns 2^128 - 1, used by reset_virtual_schedule_time and
// the pre-HF4 "lock out top witnesses" path (sets VST = max so they're
// never picked as timeshare).
func uint128Max() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
}

// uint128MaxBytes is the 16-byte big-endian representation of 2^128-1 —
// matches fc::uint128_max_value().
var uint128MaxBytes = [16]byte{
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
}

// hiveVirtualScheduleLapLength2 is HIVE_VIRTUAL_SCHEDULE_LAP_LENGTH2 =
// 2^128 - 1 — used by update_witness_schedule_4 (post-HF2) when
// advancing a timeshare witness's virtual_scheduled_time:
//
//	new_vst = current_virtual_time + LAP_LENGTH2 / (votes + 1)
//
// Higher votes → smaller advance → re-scheduled sooner.
func hiveVirtualScheduleLapLength2() *big.Int {
	return uint128Max()
}

// hiveVirtualScheduleLapLength is HIVE_VIRTUAL_SCHEDULE_LAP_LENGTH =
// 2^64 - 1 — the pre-HF2 lap length, only relevant in the legacy
// update_witness_schedule path before HARDFORK_0_2 fires.
func hiveVirtualScheduleLapLength() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
}
