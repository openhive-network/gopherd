package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/state"
)

// FeedHistoryTable is the singleton table holding the price-feed
// history (rolling window of medians and the current median).
const FeedHistoryTable state.TableID = 20

// feedHistorySingletonKey marks the only valid primary key.
var feedHistorySingletonKey = []byte{0}

// FeedHistory mirrors hive::chain::feed_history_object
// (libraries/chain/include/hive/chain/detail/state/feed_history_object.hpp).
//
// PriceHistory is a deque of HBD/HIVE medians, one per
// HIVE_FEED_INTERVAL_BLOCKS. The CurrentMedianHistory is the median of
// PriceHistory entries — i.e. the median-of-medians used by everything
// that needs the chain's authoritative HIVE↔HBD price.
type FeedHistory struct {
	PriceHistory         []asset.Price
	CurrentMedianHistory asset.Price
	MarketMedianHistory  asset.Price
	CurrentMinHistory    asset.Price
	CurrentMaxHistory    asset.Price
}

// AppendFeedHistory serializes h.
func AppendFeedHistory(dst []byte, h *FeedHistory) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(h.PriceHistory)))
	for i := range h.PriceHistory {
		dst = asset.AppendPrice(dst, h.PriceHistory[i])
	}
	dst = asset.AppendPrice(dst, h.CurrentMedianHistory)
	dst = asset.AppendPrice(dst, h.MarketMedianHistory)
	dst = asset.AppendPrice(dst, h.CurrentMinHistory)
	dst = asset.AppendPrice(dst, h.CurrentMaxHistory)
	return dst
}

// ReadFeedHistory decodes a feed_history.
func ReadFeedHistory(src []byte) (FeedHistory, error) {
	r := fcraw.NewReader(src)
	var h FeedHistory
	n, err := r.ReadVarUint()
	if err != nil {
		return h, fmt.Errorf("feed_history.price_history.len: %w", err)
	}
	h.PriceHistory = make([]asset.Price, 0, n)
	for i := uint32(0); i < n; i++ {
		p, err := asset.ReadPrice(r)
		if err != nil {
			return h, fmt.Errorf("feed_history.price_history[%d]: %w", i, err)
		}
		h.PriceHistory = append(h.PriceHistory, p)
	}
	if h.CurrentMedianHistory, err = asset.ReadPrice(r); err != nil {
		return h, fmt.Errorf("feed_history.current_median_history: %w", err)
	}
	if h.MarketMedianHistory, err = asset.ReadPrice(r); err != nil {
		return h, fmt.Errorf("feed_history.market_median_history: %w", err)
	}
	if h.CurrentMinHistory, err = asset.ReadPrice(r); err != nil {
		return h, fmt.Errorf("feed_history.current_min_history: %w", err)
	}
	if h.CurrentMaxHistory, err = asset.ReadPrice(r); err != nil {
		return h, fmt.Errorf("feed_history.current_max_history: %w", err)
	}
	if !r.AtEnd() {
		return h, errors.New("feed_history: trailing bytes")
	}
	return h, nil
}

// PutFeedHistory stores h in the singleton slot.
func PutFeedHistory(s *state.Session, h *FeedHistory) {
	s.Put(FeedHistoryTable, state.PrimaryIndex, feedHistorySingletonKey, AppendFeedHistory(nil, h))
}

// GetFeedHistory fetches the singleton feed_history (returning a zero
// value with state.ErrNotFound when no median has been computed yet).
func GetFeedHistory(s *state.Session) (FeedHistory, error) {
	v, err := s.Get(FeedHistoryTable, state.PrimaryIndex, feedHistorySingletonKey)
	if err != nil {
		return FeedHistory{}, err
	}
	return ReadFeedHistory(v)
}
