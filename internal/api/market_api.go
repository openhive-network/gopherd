package api

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

type obEntry struct {
	price   float64
	hive    int64
	hbd     int64
	created uint32
	base    asset.Asset
	quote   asset.Asset
}

func (e obEntry) toJSON(legacy bool) map[string]interface{} {
	op := map[string]interface{}{}
	if legacy {
		op["base"] = legacyAsset(e.base)
		op["quote"] = legacyAsset(e.quote)
	} else {
		op["base"] = e.base
		op["quote"] = e.quote
	}
	return map[string]interface{}{
		"order_price": op,
		"real_price":  strconv.FormatFloat(e.price, 'f', 6, 64),
		"hive":        e.hive,
		"hbd":         e.hbd,
		"created":     timeString(e.created),
	}
}

// mGetOrderBook: condenser get_order_book [limit] → {bids, asks} for the
// HIVE/HBD internal market, best-first, capped at limit (≤500).
func (s *Server) mGetOrderBook(legacy bool) jsonrpc.Handler {
	hiveNum, hbdNum := uint32(asset.HiveSymbol), uint32(asset.HbdSymbol)
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		limit := 500
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
			_ = json.Unmarshal(arr[0], &limit)
		}
		if limit <= 0 || limit > 500 {
			limit = 500
		}
		var bids, asks []obEntry
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			objects.EnsureLimitOrderStore(sess).RangeOrders(func(lo *objects.LimitOrder) bool {
				base := asset.Asset{Amount: lo.ForSaleAmount, Symbol: asset.Symbol{Num: asset.AssetNum(lo.ForSaleSymbol)}}
				quote := asset.Asset{Amount: lo.MinReceiveAmt, Symbol: asset.Symbol{Num: asset.AssetNum(lo.MinReceiveSym)}}
				switch {
				case lo.ForSaleSymbol == hbdNum && lo.MinReceiveSym == hiveNum:
					// selling HBD for HIVE → a BID for HIVE
					h, d := lo.MinReceiveAmt, lo.ForSaleAmount
					if h > 0 {
						bids = append(bids, obEntry{price: float64(d) / float64(h), hive: h, hbd: d, created: lo.Created, base: base, quote: quote})
					}
				case lo.ForSaleSymbol == hiveNum && lo.MinReceiveSym == hbdNum:
					// selling HIVE for HBD → an ASK
					h, d := lo.ForSaleAmount, lo.MinReceiveAmt
					if h > 0 {
						asks = append(asks, obEntry{price: float64(d) / float64(h), hive: h, hbd: d, created: lo.Created, base: base, quote: quote})
					}
				}
				return true
			})
			return nil
		})
		sort.Slice(bids, func(i, j int) bool { return bids[i].price > bids[j].price }) // highest bid first
		sort.Slice(asks, func(i, j int) bool { return asks[i].price < asks[j].price }) // lowest ask first
		trim := func(e []obEntry) []map[string]interface{} {
			if len(e) > limit {
				e = e[:limit]
			}
			out := make([]map[string]interface{}, 0, len(e))
			for _, x := range e {
				out = append(out, x.toJSON(legacy))
			}
			return out
		}
		return map[string]interface{}{"bids": trim(bids), "asks": trim(asks)}, nil
	}
}
