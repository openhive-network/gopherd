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
	HF28: 100000000,
}

// Set is a bitmap of activated hardforks. The zero value has only HF0 active
// (genesis is always implicitly active).
type Set uint32

// SetForBlock returns the set of hardforks active at the given mainnet block
// number. Activation is "block_num >= activation" (matches C++ semantics).
func SetForBlock(blockNum uint32) Set {
	var s Set
	// HF0 always active.
	s |= 1
	for i := ID(1); i <= NumHardforks; i++ {
		if blockNum >= MainnetActivationBlock[i] {
			s |= 1 << i
		}
	}
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
