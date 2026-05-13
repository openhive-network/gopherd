package chain

import (
	"math/bits"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// processConversions mirrors libraries/chain/database_conversion.cpp:34.
// For every convert_request whose conversion_date has passed:
//
//	hive_amount_to_issue = hbd_amount * feed_history.current_median_history
//	credit owner.balance += hive_amount_to_issue
//	dgpo.current_supply  -= hbd_amount  (HBD burn)
//	dgpo.current_supply  += hive_amount_to_issue
//
// Hived early-returns when the median history is null
// (current_median_history.amount == 0); that's the case before the
// chain has accumulated HIVE_MIN_FEEDS valid feeds (~block 933,600 in
// real Hive history).
func processConversions(s *state.Session, blockTime uint32, hooks *blockHookScratch) {
	store := objects.EnsureConvertRequestStore(s)
	fh, err := objects.GetFeedHistory(s)
	if err != nil {
		return // no feeds yet
	}
	if priceIsZero(fh.CurrentMedianHistory) {
		return
	}

	hooks.conversionPending = hooks.conversionPending[:0]
	ready := &hooks.conversionPending
	if earliest, has := store.EarliestDue(); has && earliest <= blockTime {
		store.RangeDueBefore(blockTime, func(c objects.ConvertRequest) bool {
			*ready = append(*ready, blockHookConvertPending{conversion: c.Conversion, owner: string(c.Owner), requestID: c.RequestID})
			return true
		})
	}

	for _, p := range *ready {
		req, gerr := objects.GetConvertRequest(s, auth.AccountName(p.owner), p.requestID)
		if gerr != nil {
			continue
		}
		// hbd_amount * feed_price = hive_amount_to_issue.
		// median is HBD/HIVE: base.symbol = HBD, quote.symbol = HIVE
		// (post-HF20 normalization). hive_amount = hbd × quote / base.
		hiveAmount := convertHBDToHive(req.Amount, fh.CurrentMedianHistory)
		if hiveAmount > 0 {
			a, aerr := objects.GetAccount(s, p.owner)
			if aerr == nil {
				a.Balance += hiveAmount
				objects.PutAccount(s, &a)
			}
		}
		// Mirror C++ database_conversion.cpp adjust_supply pair:
		// adjust_supply(-req.Amount HBD) → recompute virtual_supply, then
		// adjust_supply(+hiveAmount HIVE) → incremental virtual_supply +=.
		// Net VS change is ~0 (HBD removed equals HIVE added at median),
		// but doing each side correctly keeps intra-block readers
		// (process_funds, etc.) and the next-block percent_hbd ratio in
		// sync with C++.
		if dgpo, derr := objects.GetDGPO(s); derr == nil {
			adjustSupplyHbd(s, &dgpo, -req.Amount)
			adjustSupplyHive(s, &dgpo, hiveAmount)
			objects.PutDGPO(s, &dgpo)
		}
		objects.DeleteConvertRequestFull(s, &req)
	}

	// Collateralized requests. Two-pass walk: first collect matures
	// (RangeAll snapshots values), then settle. Per-row deletion during
	// Range would invalidate the iterator.
	var matured []objects.CollateralizedConvert
	_ = objects.RangeAllCollateralizedConvert(s, func(c objects.CollateralizedConvert) bool {
		if c.Conversion <= blockTime {
			matured = append(matured, c)
		}
		return true
	})
	for i := range matured {
		req := &matured[i]
		requiredHIVE, _, sErr := evaluator.SettleCollateralizedConvert(s, req, blockTime)
		if sErr != nil {
			continue
		}
		// current_supply -= requiredHIVE (the collateral that's burned);
		// virtual_supply -= requiredHIVE too. HBD was already minted at
		// op-apply, so HBD supply needs no further adjustment.
		if dgpo, derr := objects.GetDGPO(s); derr == nil {
			dgpo.CurrentSupply -= requiredHIVE
			dgpo.VirtualSupply -= requiredHIVE
			objects.PutDGPO(s, &dgpo)
		}
		objects.DeleteCollateralizedConvert(s, req.Owner, req.RequestID)
	}
}

// convertHBDToHive applies a HBD/HIVE price feed to an HBD amount:
//
//	if median has HBD as base: hive = hbd × quote.amount / base.amount
//	if median has HIVE as base: hive = hbd × base.amount / quote.amount
//
// The price-feed orientation is normalized post-HF20 to (HBD base, HIVE
// quote); pre-HF20 mainnet has 21 reversed feeds (per the comment in
// hived database.cpp:2433-2436), but the median-of-medians is the same
// across orientations so we accept either form here.
func convertHBDToHive(hbdAmount int64, p asset.Price) int64 {
	if p.Base.Amount <= 0 || p.Quote.Amount <= 0 {
		return 0
	}
	// HBD-base (typical): hbd × quote / base
	if p.Base.Symbol.Num == asset.HbdSymbol && p.Quote.Symbol.Num == asset.HiveSymbol {
		return mulDiv(hbdAmount, p.Quote.Amount, p.Base.Amount)
	}
	// HIVE-base (reversed feeds): hbd × base / quote
	if p.Base.Symbol.Num == asset.HiveSymbol && p.Quote.Symbol.Num == asset.HbdSymbol {
		return mulDiv(hbdAmount, p.Base.Amount, p.Quote.Amount)
	}
	return 0
}

// mulDiv computes (a * b) / c using big.Int for the slow path.
// HBD amounts × price.amount can hit ~10^19 at chain head — solidly
// past int64. The previous "slow path" silently overflowed because
// mulInt64 did not actually widen the multiply, producing wrong
// HBD↔HIVE conversion amounts and (transitively) wrong supply
// accounting.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	// Fast path: both factors fit in 32 bits → product fits in 64.
	const half = 1 << 31
	if -half < a && a < half && -half < b && b < half {
		return a * b / c
	}
	// Native uint128 path replaces a math/big triple-alloc on every
	// HBD↔HIVE conversion (process_conversions fires every block).
	// Inputs are non-negative int64 amounts in practice — sign-extend
	// path mirrors the math/big saturation for completeness.
	neg := false
	au, bu := uint64(a), uint64(b)
	if a < 0 {
		neg = !neg
		au = uint64(-a)
	}
	if b < 0 {
		neg = !neg
		bu = uint64(-b)
	}
	cu := uint64(c)
	if c < 0 {
		neg = !neg
		cu = uint64(-c)
	}
	hi, lo := bits.Mul64(au, bu)
	if hi >= cu {
		if neg {
			return -(1 << 62)
		}
		return 1<<63 - 1
	}
	q, _ := bits.Div64(hi, lo, cu)
	if q > 1<<63-1 {
		if neg {
			return -(1 << 62)
		}
		return 1<<63 - 1
	}
	r := int64(q)
	if neg {
		return -r
	}
	return r
}
