package objects

import (
	"math/big"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/state"
)

// feedPriceIsZero is a local copy of chain.priceIsZero (we can't import
// chain from objects). A zero/null median means no feed has settled yet
// — treat as no HBD-side virtual supply contribution, matching C++'s
// `median_price.is_null()` short-circuit.
func feedPriceIsZero(p asset.Price) bool {
	return p.Base.Amount == 0 || p.Quote.Amount == 0
}

// hbdInterestVirtualHive converts a sat-HBD interest amount into the
// HIVE-equivalent virtual_supply contribution at the given median feed.
// Local mirror of chain.convertHBDToHive (objects can't import chain).
func hbdInterestVirtualHive(hbdInterest int64, median asset.Price) int64 {
	if median.Base.Amount <= 0 || median.Quote.Amount <= 0 {
		return 0
	}
	num := new(big.Int)
	if median.Base.Symbol.Num == asset.HbdSymbol && median.Quote.Symbol.Num == asset.HiveSymbol {
		// HBD-base / HIVE-quote: hive = hbd × quote / base.
		num.Mul(big.NewInt(hbdInterest), big.NewInt(median.Quote.Amount))
		num.Quo(num, big.NewInt(median.Base.Amount))
	} else if median.Base.Symbol.Num == asset.HiveSymbol && median.Quote.Symbol.Num == asset.HbdSymbol {
		// HIVE-base / HBD-quote: hive = hbd × base / quote.
		num.Mul(big.NewInt(hbdInterest), big.NewInt(median.Base.Amount))
		num.Quo(num, big.NewInt(median.Quote.Amount))
	} else {
		return 0
	}
	if num.IsInt64() {
		return num.Int64()
	}
	return 0
}

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

// rateForDGPO returns the HBD interest rate (basis points) straight from
// the dgpo, matching hived's get_hbd_interest_rate() (database.cpp:3214)
// which has NO fallback. dgpo.HBDInterestRate is seeded to 1000 at genesis
// (genesis.go) and tracked from the witness median every schedule update,
// so it is only 0 when witnesses have legitimately voted the rate to 0 —
// in which case hived pays ZERO interest. The earlier
// `if == 0 { return defaultHBDInterestRate (1000) }` fallback treated a
// valid 0 as "unseeded" and paid 10% APR interest that hived never paid,
// inflating current_hbd_supply → virtual_supply → new_hive → producer +
// vesting rewards (the +564M VESTS / ~272 sat-HIVE over-credit across all
// witnesses observed at block 11.30M; mainnet rate fell to 0 by ~11.3M).
func rateForDGPO(dgpo *DynamicGlobalProperty) int64 {
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
	dgpo, dgpoErr := GetDGPO(s)
	if dgpoErr != nil {
		a.HBDBalance += delta
		return
	}
	if AdjustHBDBalanceInto(s, a, blockTime, delta, &dgpo) {
		PutDGPO(s, &dgpo)
	}
}

// AdjustHBDBalanceInto is AdjustHBDBalance operating on the CALLER's dgpo
// pointer instead of doing its own GetDGPO/PutDGPO. Callers that already
// hold a working dgpo copy in the same block (e.g. process_funds routing
// the per-block DHF inflation into the treasury) MUST use this so the
// hbd_seconds accrual + interest payout share that copy — a direct
// `treasury.HBDBalance += x` skips the accrual and over-counts the
// treasury's hbd_seconds (end-of-interval balance charged for the whole
// interval), over-paying HBD interest (first visible as hbd_supply +1151
// at the hive.fund interest payout, block 52,107,629). Returns true if
// dgpo was modified (interest paid), so the caller can persist it.
func AdjustHBDBalanceInto(s *state.Session, a *Account, blockTime uint32, delta int64, dgpo *DynamicGlobalProperty) (dgpoChanged bool) {
	if a.HBDSecondsLastUpdate != blockTime {
		// C++ has_hardfork(HF25) is FALSE during the activation block's
		// ops — process_hardforks runs LAST in _apply_block. Go bumps
		// dgpo.HeadBlockNumber at the START of the block, so a `>=`
		// gate flips true one block too early. Use `>` to match the
		// off-by-one semantics that hardfork.SetForBlock(N).Has(HF25)
		// also obeys (per hardfork_test.go).
		hf25Active := dgpo.HeadBlockNumber > hf25LiquidHbdDisabledBlock
		if !hf25Active {
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
				interest := hbdInterestPayout(a.HBDSeconds, rateForDGPO(dgpo))
				if interest > 0 {
					a.HBDBalance += interest
					// Mirror C++ adjust_balance HBD branch (database.cpp:3350-3354):
					// virtual_supply += interest_paid × current_median_history.
					// Without this, Go's intra-block VS lags C++'s by
					// `interest × median` per HBD-interest payout. End-of-block
					// updateVirtualSupply reconciles, but intra-block readers
					// (process_funds, payCreateHbd's print_rate via hbd ratio)
					// see different VS values.
					dgpo.CurrentHBDSupply += interest
					if fh, ferr := GetFeedHistory(s); ferr == nil && !feedPriceIsZero(fh.CurrentMedianHistory) {
						dgpo.VirtualSupply += hbdInterestVirtualHive(interest, fh.CurrentMedianHistory)
					}
					dgpoChanged = true
				}
				a.HBDSeconds = [16]byte{}
				a.HBDLastInterestPayment = blockTime
			}
		}
	}
	a.HBDBalance += delta
	return dgpoChanged
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
					if fh, ferr := GetFeedHistory(s); ferr == nil && !feedPriceIsZero(fh.CurrentMedianHistory) {
						dgpo.VirtualSupply += hbdInterestVirtualHive(interest, fh.CurrentMedianHistory)
					}
					PutDGPO(s, &dgpo)
				}
				a.SavingsHBDSeconds = [16]byte{}
				a.SavingsHBDLastInterestPayment = blockTime
			}
		}
	}
	a.SavingsHBDBalance += delta
}
