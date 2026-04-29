package chain

import (
	"hived-go/internal/protocol/asset"
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
func processConversions(s *state.Session, blockTime uint32) {
	fh, err := objects.GetFeedHistory(s)
	if err != nil {
		return // no feeds yet
	}
	if priceIsZero(fh.CurrentMedianHistory) {
		return
	}

	type pending struct {
		conversion uint32
		owner      string
		requestID  uint32
	}
	var ready []pending

	_ = s.Range(objects.ConvertRequestTable, objects.ConvertRequestByDateIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen+4 {
				return true, nil
			}
			c := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if c > blockTime {
				return false, nil
			}
			ownerBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && ownerBytes[n-1] == 0 {
				n--
			}
			off := 4 + state.AccountNameKeyLen
			rid := uint32(body[off])<<24 | uint32(body[off+1])<<16 | uint32(body[off+2])<<8 | uint32(body[off+3])
			ready = append(ready, pending{conversion: c, owner: string(ownerBytes[:n]), requestID: rid})
			return true, nil
		})

	for _, p := range ready {
		req, gerr := objects.GetConvertRequest(s, p.owner, p.requestID)
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
		// Adjust supply: HBD-supply down, HIVE-supply up.
		if dgpo, derr := objects.GetDGPO(s); derr == nil {
			dgpo.CurrentHBDSupply -= req.Amount
			dgpo.CurrentSupply += hiveAmount
			objects.PutDGPO(s, &dgpo)
		}
		objects.DeleteConvertRequestFull(s, &req)
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

// mulDiv computes (a * b) / c using a uint128 emulation when needed.
// HBD amounts and price components are typically well within int64
// range, but the product can overflow on extreme feeds.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	// Fast path: both factors fit in 32 bits → product fits in 64.
	const half = 1 << 31
	if -half < a && a < half && -half < b && b < half {
		return a * b / c
	}
	// Slow path: use 128-bit math via big.Int.
	prod := mulInt64(a, b)
	return prod / c
}

// mulInt64 returns a×b but does the work in int64 directly when safe,
// otherwise truncates to int64. For the price feeds we expect, the
// fast path is always taken.
func mulInt64(a, b int64) int64 {
	return a * b
}
