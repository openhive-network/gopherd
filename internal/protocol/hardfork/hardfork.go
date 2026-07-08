// Package hardfork defines Hive hardfork identifiers and their activation block
// numbers, ported verbatim from libraries/protocol/include/hive/protocol/hardfork_block.hpp.
//
// Evaluators check hardfork activation via a HardforkSet bitmap precomputed
// once per block. This replaces the C++ pattern
//
//	if (head_block_num >= HIVE_HARDFORK_X_BLOCK)
//
// with the cheaper and more testable
//
//	if hf.Has(HF20)
package hardfork

import "sync/atomic"

// ID identifies a Hive hardfork. The integer value matches the ordinal used
// throughout the C++ code (HF1 == 1, ... HF28 == 28).
type ID uint8

// All 28 mainnet hardforks. The numbering follows the C++ HIVE_HARDFORK_*_BLOCK
// macro names (HF_0_X is exposed as HFX; HF_1_X is HFX). HF0 is the genesis
// pseudo-hardfork (always active).
const (
	HF0  ID = 0 // genesis (always active)
	HF1  ID = 1
	HF2  ID = 2
	HF3  ID = 3
	HF4  ID = 4
	HF5  ID = 5
	HF6  ID = 6
	HF7  ID = 7
	HF8  ID = 8
	HF9  ID = 9
	HF10 ID = 10
	HF11 ID = 11
	HF12 ID = 12
	HF13 ID = 13
	HF14 ID = 14
	HF15 ID = 15
	HF16 ID = 16
	HF17 ID = 17
	HF18 ID = 18
	HF19 ID = 19
	HF20 ID = 20
	HF21 ID = 21
	HF22 ID = 22
	HF23 ID = 23
	HF24 ID = 24
	HF25 ID = 25
	HF26 ID = 26
	HF27 ID = 27
	HF28 ID = 28
)

// NumHardforks is the count of named hardforks excluding HF0 (genesis).
const NumHardforks = 28

// MainnetActivationBlock maps hardfork ID to the mainnet block number at which
// that hardfork activates. HF0 is implicit (block 1 / genesis). Source:
// hardfork_block.hpp.
var MainnetActivationBlock = [NumHardforks + 1]uint32{
	HF0:  0,
	HF1:  905693,
	HF2:  934585,
	HF3:  953363,
	HF4:  1041497,
	HF5:  1934236,
	HF6:  2790882,
	HF7:  2889019,
	HF8:  2889959,
	HF9:  3202773,
	HF10: 3239649,
	HF11: 3276913,
	HF12: 3533567,
	HF13: 4105155,
	HF14: 5137542,
	HF15: 6547932,
	HF16: 7353249,
	HF17: 10629455,
	HF18: 10629486,
	HF19: 12988978,
	HF20: 26256743,
	HF21: 35921786,
	HF22: 35974326,
	HF23: 41818752,
	HF24: 47797680,
	HF25: 55235767,
	HF26: 68676505,
	HF27: 69050084,
	// HF28 activated Nov 19, 2025 13:00 UTC (time 1763557200). Exact
	// mainnet block from C++ hardfork_block.hpp: HIVE_HARDFORK_1_28_BLOCK.
	HF28: 101319928,
}

// Set is a bitmap of activated hardforks. The zero value has only HF0 active
// (genesis is always implicitly active).
type Set uint32

// SetForBlock returns the set of hardforks active during the given mainnet
// block's apply pipeline. C++ runs process_hardforks at the END of apply_block
// (database.cpp:2368, after process_funds/process_comment_cashout), so during
// block N's evaluators and per-block hooks `has_hardfork(HFn)` is FALSE iff
// HFn activates at block N — its effects only land for block N+1 onward. We
// match with strict `>`: HFn is active iff blockNum > activation.
//
// Hot path: per-block hooks call this 20+ times with the same blockNum. We
// pack (blockNum, set) into a single atomic.Uint64 so the common case is one
// load + one compare instead of a 28-iteration scan. Replay is single-threaded
// but the API is reachable from goroutine-heavy server code, so we keep it
// race-free via atomics.
// setForBlockCache packs (blockNum+1)<<32 | set. The +1 offset ensures the
// zero value (atomic.Uint64 default) never matches a real blockNum lookup —
// otherwise SetForBlock(0) would always cache-hit on the zero value and
// return Set(0), missing HF0's always-on bit.
var setForBlockCache atomic.Uint64

func SetForBlock(blockNum uint32) Set {
	cached := setForBlockCache.Load()
	if uint32(cached>>32) == blockNum+1 {
		return Set(cached)
	}
	var s Set
	// HF0 always active.
	s |= 1
	for i := ID(1); i <= NumHardforks; i++ {
		if blockNum > MainnetActivationBlock[i] {
			s |= 1 << i
		}
	}
	setForBlockCache.Store(uint64(blockNum+1)<<32 | uint64(s))
	return s
}

// Has reports whether hardfork id is active in s.
func (s Set) Has(id ID) bool {
	if id > NumHardforks {
		return false
	}
	return s&(1<<id) != 0
}

// HighestActive returns the highest hardfork active in s, or HF0 if only
// genesis is active.
func (s Set) HighestActive() ID {
	for i := ID(NumHardforks); i > 0; i-- {
		if s.Has(i) {
			return i
		}
	}
	return HF0
}

// MainnetActivationTime maps hardfork ID to the canonical UNIX timestamp
// from libraries/protocol/hardfork.d/*.hf. Used by hardfork_property_object
// to populate processed_hardforks. Index 0 is genesis time
// (HIVE_GENESIS_TIME = 1458835200).
var MainnetActivationTime = [NumHardforks + 1]uint32{
	HF0:  1458835200,
	HF1:  1461605400,
	HF2:  1461693600,
	HF3:  1461762000,
	HF4:  1462028400,
	HF5:  1464714000,
	HF6:  1467295200,
	HF7:  1467590400,
	HF8:  1467594000,
	HF9:  1468454400,
	HF10: 1468584000,
	HF11: 1468767600,
	HF12: 1469545200,
	HF13: 1471269600,
	HF14: 1474383600,
	HF15: 1478620800,
	HF16: 1481040000,
	HF17: 1490886000,
	HF18: 1490886000,
	HF19: 1497970800,
	HF20: 1537887600,
	HF21: 1566918000,
	HF22: 1567090800,
	HF23: 1584712800,
	HF24: 1601992800,
	HF25: 1625061600,
	HF26: 1665489600,
	HF27: 1666612800,
	HF28: 1763557200, // Wed Nov 19 2025 13:00:00 UTC (next_hf_time() in database.cpp)
}

// PackedVersion returns the C++ hardfork_version v_num for an HF id.
// hardfork_version( major, minor ) packs as (major<<24) | (minor<<16).
// HF0..HF23 have major=0; HF24..HF28 have major=1 with minor=id.
func PackedVersion(id ID) uint32 {
	if id == HF0 {
		return 0
	}
	if id <= HF23 {
		return uint32(id) << 16
	}
	return (uint32(1) << 24) | (uint32(id) << 16)
}
