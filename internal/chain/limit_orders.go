package chain

import (
	"fmt"
	"os"

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
	// Skip the Pebble Range entirely when our cached "earliest pending
	// expiration" hint says nothing is due yet. The hint is maintained
	// by PutLimitOrder (insertions) and refreshed at the bottom of this
	// function (after a scan). When unset, we always scan.
	if hint, ok := s.GetExpHint(objects.LimitOrderTable, objects.LimitOrderByExpirationIndex); ok && hint > blockTime {
		return
	}

	type pending struct {
		expiration uint32
		owner      string
		orderID    uint32
	}
	var ready []pending
	var nextExp uint32

	_ = s.Range(objects.LimitOrderTable, objects.LimitOrderByExpirationIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen+4 {
				return true, nil
			}
			if envGhivediff != "" && len(body) >= 24 {
				owner := string(body[4 : 4+state.AccountNameKeyLen])
				if owner[:3] == "jsc" {
					fmt.Fprintf(os.Stderr, "GHIVEDIFF SCAN bodylen=%d hex=%x\n", len(body), body)
				}
			}
			exp := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			// expiration == 0 is "never expires"; skip it. (hived models
			// time_point_sec maximum but the truncated wire form lands as 0
			// for some op encodings. Either way it shouldn't auto-cancel.)
			if exp == 0 {
				return true, nil
			}
			// hived's clear_expired_orders cancels orders with
			// `expiration < now` (strictly less; database_market.cpp:384).
			// Stop at the first order whose expiration is at or past the
			// current block time.
			if exp >= blockTime {
				nextExp = exp
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
		cancelExpiredOrder(s, p.owner, p.orderID, blockTime)
	}

	// Refresh the hint with the first non-expired entry's time. When
	// the scan found nothing future, store a sentinel so the next
	// block skips the scan entirely; NudgeExpHint on insert correctly
	// lowers it.
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.LimitOrderTable, objects.LimitOrderByExpirationIndex, nextExp)
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
