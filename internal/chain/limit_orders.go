package chain

import (
	"hived-go/internal/protocol/asset"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// clearExpiredOrders mirrors database::clear_expired_orders in
// libraries/chain/database_market.cpp:379. It walks the by_expiration
// secondary index and refunds + removes every order whose expiration
// time has passed.
//
// The secondary key encodes (expiration_BE, owner, orderid). We iterate
// from the front (smallest expiration first) and stop at the first
// entry whose expiration is in the future. Orders with expiration == 0
// (meaning "no expiry") sort to the very front but we skip them since
// hived's by_expiration index excludes them too via fc::time_point_sec
// max-value sentinel.
func clearExpiredOrders(s *state.Session, blockTime uint32) {
	type pending struct {
		expiration uint32
		owner      string
		orderID    uint32
	}
	var ready []pending

	_ = s.Range(objects.LimitOrderTable, objects.LimitOrderByExpirationIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen+4 {
				return true, nil
			}
			exp := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			// expiration == 0 is "never expires"; skip it. (hived models
			// time_point_sec maximum but the truncated wire form lands as 0
			// for some op encodings. Either way it shouldn't auto-cancel.)
			if exp == 0 {
				return true, nil
			}
			if exp >= blockTime {
				return false, nil
			}
			ownerBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && ownerBytes[n-1] == 0 {
				n--
			}
			off := 4 + state.AccountNameKeyLen
			orderID := uint32(body[off])<<24 | uint32(body[off+1])<<16 | uint32(body[off+2])<<8 | uint32(body[off+3])
			ready = append(ready, pending{
				expiration: exp,
				owner:      string(ownerBytes[:n]),
				orderID:    orderID,
			})
			return true, nil
		})

	for _, p := range ready {
		cancelExpiredOrder(s, p.owner, p.orderID)
	}
}

// cancelExpiredOrder refunds the order's for_sale amount to the seller
// and removes the order. Mirrors hived's cancel_order.
func cancelExpiredOrder(s *state.Session, owner string, orderID uint32) {
	order, err := objects.GetLimitOrder(s, owner, orderID)
	if err != nil {
		return
	}
	a, err := objects.GetAccount(s, owner)
	if err != nil {
		objects.DeleteLimitOrderFull(s, &order)
		return
	}
	switch asset.AssetNum(order.ForSaleSymbol) {
	case asset.HiveSymbol:
		a.Balance += order.ForSaleAmount
	case asset.HbdSymbol:
		a.HBDBalance += order.ForSaleAmount
	}
	objects.PutAccount(s, &a)
	objects.DeleteLimitOrderFull(s, &order)
}
