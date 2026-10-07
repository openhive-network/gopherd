package evaluator

import (
	"errors"
	"fmt"
	"math/bits"

	"gopherd/internal/math128"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// HIVE_CONVERSION_DELAY: pre-HF16 it's 7 days; HF16+ is 3.5 days.
// (libraries/protocol/include/hive/protocol/config.hpp)
const (
	conversionDelaySecPreHF16 uint32 = 60 * 60 * 24 * 7     // 7 days
	conversionDelaySec        uint32 = 60 * 60 * 24 * 7 / 2 // 3.5 days
)

// Collateralized-convert constants — mirror libraries/protocol/config.hpp.
const (
	hive100Percent           = 10000
	hiveCollateralRatio      = 2 * hive100Percent // 200%
	hiveCollateralConvertFee = 5 * 100            // 5% (HIVE_1_PERCENT == 100)
)

func conversionDelay(hf hardfork.Set) uint32 {
	if hf.Has(hardfork.HF16) {
		return conversionDelaySec
	}
	return conversionDelaySecPreHF16
}

// Errors specific to convert.
var (
	ErrConvertExists = errors.New("convert request with this id already exists")
)

type convertEvaluator struct{}

func (convertEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagConvert {
		return fmt.Errorf("convertEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	c := opArg.AsConvert()
	if c.Amount.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: convert requires HBD", ErrUnsupportedSymbol)
	}
	if c.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	a, err := objects.GetAccount(ctx.DB, c.Owner)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: owner=%q", ErrAccountNotFound, c.Owner)
		}
		return err
	}
	if a.HBDBalance < c.Amount.Amount {
		return fmt.Errorf("%w: %q has %d HBD, convert %d", ErrInsufficientBalance, c.Owner, a.HBDBalance, c.Amount.Amount)
	}
	if has, err := objects.HasConvertRequest(ctx.DB, c.Owner, c.RequestID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrConvertExists, c.Owner, c.RequestID)
	}
	// Mirror C++ hive_evaluator_transfer.cpp:476-477 →
	// chain_validation.hpp:11 — reject convert when the median feed
	// is null. Pre-fix, gopherd silently queued the ConvertRequest
	// row; the settle would happen once a feed arrived via
	// processConversions, but the op-apply hash would diverge from
	// hived which rejects the entire tx.
	//
	// Zero mainnet impact in the pre-feed era (blocks 0..~933,600,
	// no convert ops submitted), but live-sync parity needs the
	// gate.
	if fh, ferr := objects.GetFeedHistory(ctx.DB); ferr != nil || priceIsZeroLocal(fh.CurrentMedianHistory) {
		return fmt.Errorf("convert HBD: price feed unavailable")
	}
	objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, -c.Amount.Amount)
	objects.PutAccount(ctx.DB, &a)
	objects.PutConvertRequest(ctx.DB, &objects.ConvertRequest{
		Owner:      c.Owner,
		RequestID:  c.RequestID,
		Amount:     c.Amount.Amount,
		Conversion: ctx.HeadBlockTime + conversionDelay(ctx.Hardforks),
	})
	return nil
}

type collateralizedConvertEvaluator struct{}

func (collateralizedConvertEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagCollateralizedConvert {
		return fmt.Errorf("collateralizedConvertEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	c := opArg.AsCollateralizedConvert()
	// HF1_25 — collateralized_convert is HF25+ only.
	if !ctx.Hardforks.Has(hardfork.HF25) {
		return fmt.Errorf("collateralized_convert: requires HF25")
	}
	if c.Amount.Symbol.Num != asset.HiveSymbol {
		return fmt.Errorf("%w: collateralized_convert requires HIVE collateral", ErrUnsupportedSymbol)
	}
	if c.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	a, err := objects.GetAccount(ctx.DB, c.Owner)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: owner=%q", ErrAccountNotFound, c.Owner)
		}
		return err
	}
	if a.Balance < c.Amount.Amount {
		return fmt.Errorf("%w: %q HIVE", ErrInsufficientBalance, c.Owner)
	}
	if has, err := objects.HasCollateralizedConvert(ctx.DB, c.Owner, c.RequestID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrConvertExists, c.Owner, c.RequestID)
	}

	// Need an active price feed and a positive print-rate. Mirrors
	// validate_price_feed_available + dgpo.hbd_print_rate > 0
	// (hive_evaluator_transfer.cpp:483-489).
	fh, ferr := objects.GetFeedHistory(ctx.DB)
	if ferr != nil || priceIsZeroLocal(fh.CurrentMinHistory) || priceIsZeroLocal(fh.CurrentMedianHistory) {
		return fmt.Errorf("collateralized_convert: price feed unavailable")
	}
	dgpo, derr := objects.GetDGPO(ctx.DB)
	if derr != nil {
		return derr
	}
	if dgpo.HBDPrintRate == 0 {
		return fmt.Errorf("collateralized_convert: hbd_print_rate is zero (global limit hit)")
	}

	// _amount = o.amount × 100% / HIVE_CONVERSION_COLLATERAL_RATIO (i.e. ÷2).
	immediateHIVE := mulDivCollateralized(c.Amount.Amount, hive100Percent, hiveCollateralRatio)
	// converted_amount = multiply_with_fee(immediateHIVE, current_min_history, fee, fee_on=HIVE).
	hbdOut := multiplyWithFeeHIVEToHBD(immediateHIVE, fh.CurrentMinHistory, hiveCollateralConvertFee)
	if hbdOut <= 0 {
		return fmt.Errorf("collateralized_convert: collateral too low - conversion gives no HBD")
	}

	a.Balance -= c.Amount.Amount

	// Credit HBD (accrues hbd_seconds — same path as ordinary HBD deposit).
	objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, hbdOut)
	objects.PutAccount(ctx.DB, &a)

	// Update DGPO: current_hbd_supply += hbdOut, virtual_supply += hbdOut × median.
	// HIVE supply will be corrected at settlement.
	dgpo.CurrentHBDSupply += hbdOut
	hbdAsHIVE := convertHBDToHiveLocal(hbdOut, fh.CurrentMedianHistory)
	dgpo.VirtualSupply += hbdAsHIVE

	// percent_hbd <= hbd_stop_percent (basis points). Use the freshly-
	// updated supplies — mirrors database::calculate_HBD_percent
	// (database.cpp:2878-2901) EXACTLY, like updateVirtualSupply's copy in
	// internal/chain/virtual_supply.go (validated 47.8M→55.2M):
	//   HF24+ excludes treasury HBD from the debt ratio (hbd_supply -=
	//   treasury.hbd_balance, floor 0; virtual_supply recomputed as
	//   hbd×median + current_supply) and HF21+ rounds half-up
	//   (num += virtual_supply/2). The old local calcHBDPercent had
	//   neither — with the treasury holding millions of HBD by 57M it
	//   overstated the ratio (1001 vs ≤1000) and bounced a legal
	//   collateralized_convert at block 57,065,738.
	percentHBD := calcHBDPercent(ctx, &dgpo, fh.CurrentMedianHistory)
	if percentHBD > int64(dgpo.HBDStopPercent) {
		return fmt.Errorf("collateralized_convert: percent_hbd %d > hbd_stop_percent %d", percentHBD, dgpo.HBDStopPercent)
	}
	objects.PutDGPO(ctx.DB, &dgpo)

	objects.PutCollateralizedConvert(ctx.DB, &objects.CollateralizedConvert{
		Owner:      c.Owner,
		RequestID:  c.RequestID,
		Collateral: c.Amount.Amount,
		HBDOut:     hbdOut,
		Conversion: ctx.HeadBlockTime + conversionDelay(ctx.Hardforks),
	})
	// hive_evaluator_transfer.cpp:532.
	ctx.EmitCollateralizedConvertImmediateConversion(string(c.Owner), c.RequestID, hbdOut)
	return nil
}

// priceIsZeroLocal — local helper (chain.priceIsZero exists in the
// chain pkg; duplicated here to avoid a cyclic import).
func priceIsZeroLocal(p asset.Price) bool {
	return p.Base.Amount <= 0 || p.Quote.Amount <= 0
}

// mulDivCollateralized = a × b / c using 128-bit intermediate.
func mulDivCollateralized(a, b, c int64) int64 {
	if c <= 0 || a < 0 {
		return 0
	}
	r := math128.Mul64Wide(uint64(a), uint64(b)).DivU64(uint64(c))
	return int64(r.Lo)
}

// multiplyWithFeeHIVEToHBD: HIVE (=quote of HBD_price) → HBD (=base),
// fee applied to HIVE (the quote symbol). Mirrors the C++
// "tiny_asset<QUOTE> * tiny_price → tiny_asset<BASE>" overload with
// _APPLY_FEE_TO == _QUOTE_SYMBOL:
//
//	result = a × base × 100% / (quote × (100% + fee))
//
// Uses U192-precision intermediates so the multiply does not overflow
// even when both base/quote are ~2^62 (wide_price test case).
func multiplyWithFeeHIVEToHBD(hiveAmount int64, p asset.Price, fee uint16) int64 {
	if hiveAmount <= 0 || priceIsZeroLocal(p) {
		return 0
	}
	baseHBD, quoteHIVE := p.Base.Amount, p.Quote.Amount
	if p.Base.Symbol.Num == asset.HiveSymbol && p.Quote.Symbol.Num == asset.HbdSymbol {
		baseHBD, quoteHIVE = p.Quote.Amount, p.Base.Amount
	}
	num128 := math128.Mul64Wide(uint64(hiveAmount), uint64(baseHBD))
	num192 := math128.Mul64x128(uint64(hive100Percent), num128)
	denom := math128.Mul64Wide(uint64(quoteHIVE), uint64(hive100Percent)+uint64(fee))
	q, _ := num192.QuoRem(denom)
	if q > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(q)
}

// multiplyWithFeeHBDToHIVE: HBD (=base) → HIVE (=quote), fee applied to
// HIVE. Mirrors "tiny_asset<BASE> * tiny_price → tiny_asset<QUOTE>"
// with _APPLY_FEE_TO == _QUOTE_SYMBOL:
//
//	result = a × quote × (100% + fee) / (base × 100%)
func multiplyWithFeeHBDToHIVE(hbdAmount int64, p asset.Price, fee uint16) int64 {
	if hbdAmount <= 0 || priceIsZeroLocal(p) {
		return 0
	}
	baseHBD, quoteHIVE := p.Base.Amount, p.Quote.Amount
	if p.Base.Symbol.Num == asset.HiveSymbol && p.Quote.Symbol.Num == asset.HbdSymbol {
		baseHBD, quoteHIVE = p.Quote.Amount, p.Base.Amount
	}
	num128 := math128.Mul64Wide(uint64(hbdAmount), uint64(quoteHIVE))
	num192 := math128.Mul64x128(uint64(hive100Percent)+uint64(fee), num128)
	denom := math128.Mul64Wide(uint64(baseHBD), uint64(hive100Percent))
	q, _ := num192.QuoRem(denom)
	if q > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(q)
}

// convertHBDToHiveLocal — HBD × price → HIVE, no fee. Same orientation
// handling as multiplyWithFee. Duplicated from chain.convertHBDToHive
// to avoid the import cycle.
func convertHBDToHiveLocal(hbdAmount int64, p asset.Price) int64 {
	if priceIsZeroLocal(p) {
		return 0
	}
	baseHBD, quoteHIVE := p.Base.Amount, p.Quote.Amount
	if p.Base.Symbol.Num == asset.HiveSymbol && p.Quote.Symbol.Num == asset.HbdSymbol {
		baseHBD, quoteHIVE = p.Quote.Amount, p.Base.Amount
	}
	num := math128.Mul64Wide(uint64(hbdAmount), uint64(quoteHIVE))
	q := num.DivU64(uint64(baseHBD))
	if q.Hi != 0 || q.Lo > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(q.Lo)
}

// calcHBDPercent mirrors database::calculate_HBD_percent:
//
//	percent_hbd = (hbd_supply × median_quote) × 10000 / (median_base × virtual_supply)
//
// (i.e. convert HBD → HIVE-equivalent then divide by virtual_supply,
// in basis points). Returns 0 when virtual_supply is non-positive.
func calcHBDPercent(ctx *Context, dgpo *objects.DynamicGlobalProperty, median asset.Price) int64 {
	hbdSupply := dgpo.CurrentHBDSupply
	virtualSupply := dgpo.VirtualSupply
	if ctx.Hardforks.Has(hardfork.HF24) {
		// Treasury HBD sits outside the open market — excluded from the
		// debt ratio, and virtual_supply recomputed from the reduced debt
		// (database.cpp:2887-2894). hive.fund is the treasury post-HF24.
		if t, terr := objects.GetAccount(ctx.DB, "hive.fund"); terr == nil {
			hbdSupply -= t.HBDBalance
			if hbdSupply < 0 {
				hbdSupply = 0
			}
			if hbdSupply != dgpo.CurrentHBDSupply {
				virtualSupply = convertHBDToHiveLocal(hbdSupply, median) + dgpo.CurrentSupply
			}
		}
	}
	if virtualSupply <= 0 {
		return 0
	}
	hbdInHIVE := convertHBDToHiveLocal(hbdSupply, median)
	num := math128.Mul64Wide(uint64(hbdInHIVE), uint64(hive100Percent))
	if ctx.Hardforks.Has(hardfork.HF21) {
		// Round half-up (database.cpp:2898-2899).
		half := uint64(virtualSupply / 2)
		lo, carry := bits.Add64(num.Lo, half, 0)
		num = math128.U128{Hi: num.Hi + carry, Lo: lo}
	}
	q := num.DivU64(uint64(virtualSupply))
	if q.Hi != 0 {
		return 1 << 31
	}
	return int64(q.Lo)
}

// SettleCollateralizedConvert performs the per-request settlement for a
// matured collateralized_convert_request_object. Returns
// (requiredHIVE, excessRefunded). The HBD was already credited at
// op-apply time, so this only refunds excess HIVE; caller is
// responsible for deleting the row and adjusting DGPO supply.
//
// Math mirrors database_conversion.cpp:82-117 (collateralized branch):
//
//	required_hive = multiply_with_fee( req.hbd_out, market_median, fee )
//	excess        = req.collateral - required_hive
//	if excess > 0: owner.balance += excess
//	if excess < 0: required_hive = req.collateral; excess = 0
func SettleCollateralizedConvert(s *state.Session, req *objects.CollateralizedConvert, blockTime uint32) (requiredHIVE, excess int64, _ error) {
	fh, err := objects.GetFeedHistory(s)
	if err != nil || priceIsZeroLocal(fh.MarketMedianHistory) {
		return 0, 0, fmt.Errorf("collateralized_convert settle: market median unavailable")
	}
	required := multiplyWithFeeHBDToHIVE(req.HBDOut, fh.MarketMedianHistory, hiveCollateralConvertFee)
	exc := req.Collateral - required
	if exc < 0 {
		// Insufficient collateral (rare): consume the full collateral
		// and refund nothing. C++ pushes a system_warning_operation; we
		// silently absorb.
		required = req.Collateral
		exc = 0
	} else if exc > 0 {
		// Refund excess to owner.
		owner, gerr := objects.GetAccount(s, req.Owner)
		if gerr == nil {
			owner.Balance += exc
			objects.PutAccount(s, &owner)
		}
	}
	return required, exc, nil
}

func init() {
	register(op.TagConvert, convertEvaluator{})
	register(op.TagCollateralizedConvert, collateralizedConvertEvaluator{})
}
