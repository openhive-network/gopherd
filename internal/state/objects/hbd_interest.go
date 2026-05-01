package objects

import (
	"math/big"

	"hived-go/internal/state"
)

// HIVE_HBD_INTEREST_COMPOUND_INTERVAL_SEC = 30 days. Interest accrues
// continuously into hbd_seconds; once a 30-day window passes, the next
// adjust_balance call pays out the accrued interest and resets seconds.
const hbdInterestCompoundIntervalSec uint32 = 60 * 60 * 24 * 30

// HIVE_SECONDS_PER_YEAR — 365 days × 86400. Hived uses an integer
// constant; we mirror it exactly.
const secondsPerYear int64 = 60 * 60 * 24 * 365

// HIVE_DEFAULT_HBD_INTEREST_RATE = 1000 = 10% APR. Until the witness
// schedule subsystem lands and computes the median, this is what we
// shadow into dgpo.HBDInterestRate.
const defaultHBDInterestRate uint16 = 1000

// rateForDGPO returns the HBD interest rate basis points to use, falling
// back to the genesis default when dgpo hasn't been seeded yet.
func rateForDGPO(dgpo *DynamicGlobalProperty) int64 {
	if dgpo.HBDInterestRate == 0 {
		return int64(defaultHBDInterestRate)
	}
	return int64(dgpo.HBDInterestRate)
}

// uint128AddProduct accrues `accum += hbd × seconds` into a 16-byte BE
// uint128. Returns the updated bytes; clamps at 2^128-1 defensively.
func uint128AddProduct(accum [16]byte, hbd int64, seconds uint32) [16]byte {
	if hbd <= 0 || seconds == 0 {
		return accum
	}
	cur := new(big.Int).SetBytes(accum[:])
	prod := new(big.Int).Mul(big.NewInt(hbd), big.NewInt(int64(seconds)))
	cur.Add(cur, prod)
	var out [16]byte
	b := cur.Bytes()
	if len(b) > 16 {
		copy(out[:], b[len(b)-16:])
	} else {
		copy(out[16-len(b):], b)
	}
	return out
}

func uint128IsZero(b [16]byte) bool {
	for _, by := range b {
		if by != 0 {
			return false
		}
	}
	return true
}

// hbdInterestPayout is the C++ formula
//
//	interest = hbd_seconds / SECONDS_PER_YEAR * rate / HIVE_100_PERCENT
//
// (in that order — the division-then-multiply prevents overflow even at
// the chain-wide totals seen post-hardfork inflation). Mirror of
// hive::protocol::hbd_interest::evaluate_hbd_interest.
func hbdInterestPayout(secondsAccum [16]byte, rateBP int64) int64 {
	cur := new(big.Int).SetBytes(secondsAccum[:])
	if cur.Sign() == 0 {
		return 0
	}
	cur.Quo(cur, big.NewInt(secondsPerYear))
	cur.Mul(cur, big.NewInt(rateBP))
	cur.Quo(cur, big.NewInt(10000))
	if !cur.IsInt64() {
		return 0
	}
	return cur.Int64()
}

// hf25LiquidHbdDisabledBlock — block at which HF25 fires on mainnet
// (libraries/protocol/include/hive/protocol/hardfork.hpp:
// HIVE_HARDFORK_1_25). Past this height, hived stops paying HBD
// interest on liquid balances; only savings_hbd accrues interest.
const hf25LiquidHbdDisabledBlock uint32 = 55_235_767

// AdjustHBDBalance is the HBD-balance counterpart of hived's
// database::adjust_balance(account, HBD_asset) (database.cpp:3206). It
// accrues hbd_seconds since the previous update, pays out accrued
// interest if the 30-day compounding window has passed, then applies
// `delta` to a.HBDBalance. The interest is also added to
// dgpo.CurrentHBDSupply.
//
// Pre-HF25: liquid HBD earns interest. HF25+: the seconds tracking
// is bypassed for liquid HBD (only savings_hbd accrues).
func AdjustHBDBalance(s *state.Session, a *Account, blockTime uint32, delta int64) {
	if a.HBDSecondsLastUpdate != blockTime {
		dgpo, dgpoErr := GetDGPO(s)
		hf25Active := dgpoErr == nil && dgpo.HeadBlockNumber >= hf25LiquidHbdDisabledBlock
		if !hf25Active && dgpoErr == nil {
			elapsed := blockTime - a.HBDSecondsLastUpdate
			a.HBDSeconds = uint128AddProduct(a.HBDSeconds, a.HBDBalance, elapsed)
			a.HBDSecondsLastUpdate = blockTime
			// Mirror hived's check: only pay interest when at least one
			// full compounding interval has elapsed since the last
			// payout. This includes the LastInterestPayment==0 case as
			// long as blockTime > 30 days since epoch (true for any
			// real mainnet block).
			doPayout := blockTime-a.HBDLastInterestPayment > hbdInterestCompoundIntervalSec
			if doPayout && !uint128IsZero(a.HBDSeconds) {
				interest := hbdInterestPayout(a.HBDSeconds, rateForDGPO(&dgpo))
				if interest > 0 {
					a.HBDBalance += interest
					dgpo.CurrentHBDSupply += interest
					PutDGPO(s, &dgpo)
				}
				a.HBDSeconds = [16]byte{}
				a.HBDLastInterestPayment = blockTime
			}
		}
	}
	a.HBDBalance += delta
}

// AdjustSavingsHBDBalance mirrors database::adjust_savings_balance(account,
// HBD_asset) (database.cpp:3265). Same accrual + payout mechanics as
// AdjustHBDBalance, but against the savings_hbd_balance / *_seconds
// fields. Always active.
func AdjustSavingsHBDBalance(s *state.Session, a *Account, blockTime uint32, delta int64) {
	if a.SavingsHBDSecondsLastUpdate != blockTime {
		dgpo, err := GetDGPO(s)
		if err == nil {
			elapsed := blockTime - a.SavingsHBDSecondsLastUpdate
			a.SavingsHBDSeconds = uint128AddProduct(a.SavingsHBDSeconds, a.SavingsHBDBalance, elapsed)
			a.SavingsHBDSecondsLastUpdate = blockTime
			doPayout := blockTime-a.SavingsHBDLastInterestPayment > hbdInterestCompoundIntervalSec
			if doPayout && !uint128IsZero(a.SavingsHBDSeconds) {
				interest := hbdInterestPayout(a.SavingsHBDSeconds, rateForDGPO(&dgpo))
				if interest > 0 {
					a.SavingsHBDBalance += interest
					dgpo.CurrentHBDSupply += interest
					PutDGPO(s, &dgpo)
				}
				a.SavingsHBDSeconds = [16]byte{}
				a.SavingsHBDLastInterestPayment = blockTime
			}
		}
	}
	a.SavingsHBDBalance += delta
}
