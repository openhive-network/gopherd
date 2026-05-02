package objects

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// LimitOrder is the chain-level limit_order_object — an on-book offer
// from `Owner` to sell `ForSaleAmount` of `ForSaleSymbol` at the price
// implied by `SellPriceBaseAmount` / `MinReceiveAmt` (in `MinReceiveSym`).
//
// Schema mirrors hived's split between MUTABLE remaining-quantity
// (`for_sale`) and the IMMUTABLE original price ratio (`sell_price`):
//
//	ForSaleAmount        — current remaining amount available to fill
//	ForSaleSymbol        — sell_price.base.symbol
//	SellPriceBaseAmount  — sell_price.base.amount  (original for_sale)
//	MinReceiveAmt        — sell_price.quote.amount (original min_to_receive)
//	MinReceiveSym        — sell_price.quote.symbol
//
// At creation, ForSaleAmount == SellPriceBaseAmount; partial fills
// reduce ForSaleAmount but never the SellPrice components, so the
// price ratio (and therefore later matches) stays consistent.
type LimitOrder struct {
	Owner               auth.AccountName
	OrderID             uint32
	Created             uint32
	Expiration          uint32
	ForSaleAmount       int64
	ForSaleSymbol       uint32 // legacy NAI form (HiveSymbol/HbdSymbol)
	MinReceiveAmt       int64
	MinReceiveSym       uint32
	SellPriceBaseAmount int64 // immutable; sell_price.base.amount
	// ID is the auto-incrementing object_id assigned at creation,
	// mirroring C++'s limit_order_object::id. Used as the tie-break in
	// the by_price index (composite_key<sell_price desc, id asc> at
	// limit_order_object_multiindex.hpp). Without this, ties resolve by
	// (owner, user_orderid) which differs from hived and produces
	// different match orderings — visible at block 2,890,560 where Go
	// and C++ pick different makers for the same taker, drifting
	// individual account balances by 1+ satoshi over subsequent fills.
	ID uint64
}

// LimitOrderTable holds the orders.
const LimitOrderTable state.TableID = 6

// LimitOrderByExpirationIndex orders entries by (expiration, owner,
// orderID) so clear_expired_orders can iterate in time order. Key:
// [expiration_BE_4][owner_padded_16][orderid_BE_4].
const LimitOrderByExpirationIndex state.IndexID = 1

func limitOrderByExpirationKey(expiration uint32, owner auth.AccountName, orderID uint32) []byte {
	out := state.AppendUint32BE(nil, expiration)
	out = state.AppendAccountName(out, owner)
	return state.AppendUint32BE(out, orderID)
}

// LimitOrderByPriceIndex mirrors hived's by_price index
// (limit_order_object_multiindex.hpp): orders are sorted by symbol-pair
// first, then by sell_price DESCENDING (best price first), then by
// (owner, orderID) ASCENDING as the tie-break.
//
// Key layout: [base_sym_BE_4][quote_sym_BE_4][price_descending_16]
//             [owner_padded_16][orderid_BE_4] (44 bytes total).
//
// price_descending_16 encodes the sell_price ratio in a 16-byte form
// where lex-byte order matches DESCENDING numeric price. We encode
// price as a uint128 fixed-point with HIVE_PRICE_FIXED_SCALE digits of
// fractional precision, then bitwise-invert each byte so larger prices
// sort first. apply_order iterates this prefix forward and stops at
// first entry whose price falls below max_price (= ~new.sell_price).
const LimitOrderByPriceIndex state.IndexID = 2

// hivePriceFixedScale is the multiplier used when encoding price ratios
// into the by_price secondary key. 10^18 gives 18 decimal digits of
// precision, ample for HIVE/HBD ratios in any conceivable range, while
// still fitting safely in 128 bits when paired with up to ~10^20-satoshi
// amounts (mainnet caps are well below that).
const hivePriceFixedScale = 1_000_000_000_000_000_000

// limitOrderByPriceKey encodes (sell_price, id, owner, orderID). The
// price ratio is sell_price.base.amount / sell_price.quote.amount. id
// is the auto-incrementing limit_order_object::id and serves as the
// tie-break, matching C++'s composite_key<sell_price desc, id asc>.
// Owner+orderID is appended after id so iteration callers can recover
// the primary key directly from the index entry without a second
// lookup.
//
// Layout: [base_sym:4][quote_sym:4][price_descending:16][id:8]
//         [owner_padded:16][orderid:4] — 52 bytes total.
func limitOrderByPriceKey(baseSym uint32, quoteSym uint32, baseAmount, quoteAmount int64, id uint64, owner auth.AccountName, orderID uint32) []byte {
	out := state.AppendUint32BE(nil, baseSym)
	out = state.AppendUint32BE(out, quoteSym)
	out = append(out, encodePriceDescending(baseAmount, quoteAmount)...)
	out = state.AppendUint64BE(out, id)
	out = state.AppendAccountName(out, owner)
	return state.AppendUint32BE(out, orderID)
}

// encodePriceDescending returns a 16-byte big-endian representation of
// floor((base / quote) × 10^18), with each byte bit-flipped so byte-wise
// lex order is the inverse of numeric order — the largest price sorts
// first, matching hived's `composite_key_compare< std::greater<price>,
// std::less<id> >`. Uses math/big for the 128-bit math; this is called
// once per insert/delete on the secondary index, never in tight loops.
func encodePriceDescending(base, quote int64) []byte {
	out := make([]byte, 16)
	if base <= 0 || quote <= 0 {
		// Degenerate price — sort to the end (invert of zero is all 0xff).
		for i := range out {
			out[i] = 0xff
		}
		return out
	}
	num := new(big.Int).Mul(big.NewInt(base), big.NewInt(hivePriceFixedScale))
	num.Quo(num, big.NewInt(quote))
	src := num.Bytes()
	if len(src) > 16 {
		// Saturate at uint128_max if the truncation would overflow —
		// happens only if base × 10^18 / quote > 2^128, which requires
		// extreme amount mismatches we don't expect in practice.
		for i := range out {
			out[i] = 0
		}
	} else {
		copy(out[16-len(src):], src)
	}
	for i := range out {
		out[i] ^= 0xff
	}
	return out
}

// limitOrderKey is owner(16 bytes) || orderID(4 bytes BE).
func limitOrderKey(owner auth.AccountName, orderID uint32) []byte {
	out := state.AppendAccountName(nil, owner)
	return state.AppendUint32BE(out, orderID)
}

// AppendLimitOrder serializes lo.
func AppendLimitOrder(dst []byte, lo *LimitOrder) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(lo.Owner)))
	dst = append(dst, lo.Owner...)
	dst = appendU32LE(dst, lo.OrderID)
	dst = appendU32LE(dst, lo.Created)
	dst = appendU32LE(dst, lo.Expiration)
	dst = appendI64LE(dst, lo.ForSaleAmount)
	dst = appendU32LE(dst, lo.ForSaleSymbol)
	dst = appendI64LE(dst, lo.MinReceiveAmt)
	dst = appendU32LE(dst, lo.MinReceiveSym)
	dst = appendI64LE(dst, lo.SellPriceBaseAmount)
	dst = appendU64LE(dst, lo.ID)
	return dst
}

// ReadLimitOrder decodes a LimitOrder.
func ReadLimitOrder(src []byte) (LimitOrder, error) {
	r := fcraw.NewReader(src)
	var lo LimitOrder
	owner, err := r.ReadString()
	if err != nil {
		return lo, fmt.Errorf("limit_order.owner: %w", err)
	}
	lo.Owner = owner
	if lo.OrderID, err = r.ReadUint32(); err != nil {
		return lo, err
	}
	if lo.Created, err = r.ReadUint32(); err != nil {
		return lo, err
	}
	if lo.Expiration, err = r.ReadUint32(); err != nil {
		return lo, err
	}
	if lo.ForSaleAmount, err = r.ReadInt64(); err != nil {
		return lo, err
	}
	if lo.ForSaleSymbol, err = r.ReadUint32(); err != nil {
		return lo, err
	}
	if lo.MinReceiveAmt, err = r.ReadInt64(); err != nil {
		return lo, err
	}
	if lo.MinReceiveSym, err = r.ReadUint32(); err != nil {
		return lo, err
	}
	// SellPriceBaseAmount was added when the matching engine landed.
	// Legacy rows (created before that point) store nothing here; we
	// fall back to ForSaleAmount, which equals the original at create
	// time when no partial fills have happened.
	if r.AtEnd() {
		lo.SellPriceBaseAmount = lo.ForSaleAmount
		return lo, nil
	}
	if lo.SellPriceBaseAmount, err = r.ReadInt64(); err != nil {
		return lo, err
	}
	if r.AtEnd() {
		return lo, nil
	}
	if lo.ID, err = r.ReadUint64(); err != nil {
		return lo, fmt.Errorf("limit_order.id: %w", err)
	}
	if !r.AtEnd() {
		return lo, errors.New("limit_order: trailing bytes")
	}
	return lo, nil
}

// PutLimitOrder stores lo and updates both secondary indices
// (by-expiration for the per-block expirer, by-price for the matching
// engine). The price components are immutable, so the by-price index
// row stays valid across partial fills.
func PutLimitOrder(s *state.Session, lo *LimitOrder) {
	s.Put(LimitOrderTable, state.PrimaryIndex, limitOrderKey(lo.Owner, lo.OrderID), AppendLimitOrder(nil, lo))
	s.Put(LimitOrderTable, LimitOrderByExpirationIndex, limitOrderByExpirationKey(lo.Expiration, lo.Owner, lo.OrderID), nil)
	s.Put(LimitOrderTable, LimitOrderByPriceIndex,
		limitOrderByPriceKey(lo.ForSaleSymbol, lo.MinReceiveSym, lo.SellPriceBaseAmount, lo.MinReceiveAmt, lo.ID, lo.Owner, lo.OrderID), nil)
	if lo.Expiration != 0 {
		s.NudgeExpHint(LimitOrderTable, LimitOrderByExpirationIndex, lo.Expiration)
	}
}

// GetLimitOrder fetches the order (owner, orderID).
func GetLimitOrder(s *state.Session, owner auth.AccountName, orderID uint32) (LimitOrder, error) {
	v, err := s.Get(LimitOrderTable, state.PrimaryIndex, limitOrderKey(owner, orderID))
	if err != nil {
		return LimitOrder{}, err
	}
	return ReadLimitOrder(v)
}

// HasLimitOrder reports whether the order exists.
func HasLimitOrder(s *state.Session, owner auth.AccountName, orderID uint32) (bool, error) {
	return s.Has(LimitOrderTable, state.PrimaryIndex, limitOrderKey(owner, orderID))
}

// DeleteLimitOrder removes the order. Caller must already know the
// expiration to also clear the secondary entry — most call sites have
// the order in hand and use DeleteLimitOrderFull.
func DeleteLimitOrder(s *state.Session, owner auth.AccountName, orderID uint32) {
	s.Delete(LimitOrderTable, state.PrimaryIndex, limitOrderKey(owner, orderID))
}

// DeleteLimitOrderFull deletes the primary entry plus both secondary
// index entries (by-expiration + by-price) in one shot.
func DeleteLimitOrderFull(s *state.Session, lo *LimitOrder) {
	s.Delete(LimitOrderTable, state.PrimaryIndex, limitOrderKey(lo.Owner, lo.OrderID))
	s.Delete(LimitOrderTable, LimitOrderByExpirationIndex, limitOrderByExpirationKey(lo.Expiration, lo.Owner, lo.OrderID))
	s.Delete(LimitOrderTable, LimitOrderByPriceIndex,
		limitOrderByPriceKey(lo.ForSaleSymbol, lo.MinReceiveSym, lo.SellPriceBaseAmount, lo.MinReceiveAmt, lo.ID, lo.Owner, lo.OrderID))
}

// silence unused-import on encoding/binary (kept available for future
// composite-key extensions).
var _ = binary.BigEndian
