package chain

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
		owner   string
		orderID uint32
	}
	var ready []pending

	store := objects.EnsureLimitOrderStore(s)
	_ = store.RangeByExpiration(func(lo *objects.LimitOrder) (bool, error) {
		// expiration == 0 is "never expires"; skip it.
		if lo.Expiration == 0 {
			return true, nil
		}
		// hived cancels orders with `expiration < now` (strictly less).
		if lo.Expiration >= blockTime {
			return false, nil
		}
		ready = append(ready, pending{owner: string(lo.Owner), orderID: lo.OrderID})
		return true, nil
	})

	for _, p := range ready {
		cancelExpiredOrder(s, p.owner, p.orderID, blockTime)
	}
}

// cancelExpiredOrder refunds the order's for_sale amount to the seller
// and removes the order. Mirrors hived's cancel_order.
func cancelExpiredOrder(s *state.Session, owner string, orderID uint32, blockTime uint32) {
	order, err := objects.GetLimitOrder(s, owner, orderID)
	if err != nil {
		return
	}
	if owner == "jsc" && envGhivediff != "" {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF EXPIRE block-t=%d owner=%s orderid=%d for_sale=%d exp=%d\n",
			blockTime, owner, orderID, order.ForSaleAmount, order.Expiration)
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
		objects.AdjustHBDBalance(s, &a, blockTime, order.ForSaleAmount)
	}
	objects.PutAccount(s, &a)
	objects.DeleteLimitOrderFull(s, &order)
}
