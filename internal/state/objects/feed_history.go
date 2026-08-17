package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/state"
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

const feedHistoryAuxKey = "feed_history"

// PutFeedHistory stores h in the singleton slot. Registers a
// pre-commit hook to refresh the cross-Session parsed-object cache
// at commit time — deferring the refresh past Restore boundaries so
// rolled-back txs never poison the cross-block cache (their
// preCommit hook is dropped by Session.Restore). Mirrors the lazy-
// caching strategy used by PutWitnessSchedule.
func PutFeedHistory(s *state.Session, h *FeedHistory) {
	if s.Spec != nil {
		s.Spec.Taint()
		return
	}
	s.SerScratch = AppendFeedHistory(s.SerScratch[:0], h)
	s.Put(FeedHistoryTable, state.PrimaryIndex, feedHistorySingletonKey, s.SerScratch)
	hCopy := *h
	s.RegisterPreCommit(func() {
		s.DatabaseAuxStore(feedHistoryAuxKey, hCopy)
	})
}

// GetFeedHistory fetches the singleton feed_history (returning a zero
// value with state.ErrNotFound when no median has been computed yet).
// The parsed struct is cached at the Database level — feed_history
// only changes when updateMedianFeed actually shifts the median
// (~hourly), so most reads across blocks hit a warm cache without
// touching the backend or the deserializer.
func GetFeedHistory(s *state.Session) (FeedHistory, error) {
	if s.Spec != nil {
		s.Spec.Taint()
		return FeedHistory{}, state.ErrNotFound
	}
	// If this Session has written to the FH row, the overlay holds the
	// new value; bypass the cross-block cache and re-read from overlay.
	if !s.HasOverlayWrite(FeedHistoryTable, state.PrimaryIndex) {
		if cached, ok := s.DatabaseAux(feedHistoryAuxKey); ok {
			if h, ok := cached.(FeedHistory); ok {
				return h, nil
			}
		}
	}
	v, err := s.Get(FeedHistoryTable, state.PrimaryIndex, feedHistorySingletonKey)
	if err != nil {
		return FeedHistory{}, err
	}
	h, err := ReadFeedHistory(v)
	if err != nil {
		return FeedHistory{}, err
	}
	// Only cache if the read came from the backend (no overlay write).
	// Caching an overlay-derived value is unsafe: if the session is
	// later Undone (mid-block tx failure, test fixture rollback), the
	// cross-Session cache would retain the abandoned value and serve
	// it to subsequent sessions until the next backend write.
	if !s.HasOverlayWrite(FeedHistoryTable, state.PrimaryIndex) {
		s.DatabaseAuxStore(feedHistoryAuxKey, h)
	}
	return h, nil
}
