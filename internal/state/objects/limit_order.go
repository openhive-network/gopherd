package objects

import (
	"encoding/binary"
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// LimitOrder is the chain-level limit_order_object — the on-book
// representation of an offer to sell `for_sale_amount` of one asset for
// at-least the price implied by `min_to_receive` of another.
//
// The full hived object also indexes by sell_price for matching; that
// lands with the matching engine. For now we only support primary
// lookup by (owner, orderid) so create/cancel can refund.
type LimitOrder struct {
	Owner          auth.AccountName
	OrderID        uint32
	Created        uint32
	Expiration     uint32
	ForSaleAmount  int64
	ForSaleSymbol  uint32 // legacy NAI form (HiveSymbol/HbdSymbol)
	MinReceiveAmt  int64
	MinReceiveSym  uint32
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
	if !r.AtEnd() {
		return lo, errors.New("limit_order: trailing bytes")
	}
	return lo, nil
}

// PutLimitOrder stores lo and updates the secondary by-expiration index.
// The expiration value is permitted to be 0 (no expiry) — those orders
// sort to the front of the secondary index but clear_expired_orders
// guards against acting on expiration == 0.
func PutLimitOrder(s *state.Session, lo *LimitOrder) {
	s.Put(LimitOrderTable, state.PrimaryIndex, limitOrderKey(lo.Owner, lo.OrderID), AppendLimitOrder(nil, lo))
	s.Put(LimitOrderTable, LimitOrderByExpirationIndex, limitOrderByExpirationKey(lo.Expiration, lo.Owner, lo.OrderID), nil)
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

// DeleteLimitOrderFull deletes the primary entry AND the secondary
// by-expiration entry in one shot.
func DeleteLimitOrderFull(s *state.Session, lo *LimitOrder) {
	s.Delete(LimitOrderTable, state.PrimaryIndex, limitOrderKey(lo.Owner, lo.OrderID))
	s.Delete(LimitOrderTable, LimitOrderByExpirationIndex, limitOrderByExpirationKey(lo.Expiration, lo.Owner, lo.OrderID))
}

// silence unused-import on encoding/binary (kept available for future
// composite-key extensions).
var _ = binary.BigEndian
