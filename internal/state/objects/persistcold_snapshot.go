package objects

import (
	"bytes"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// snapColdDurableFlag marks a snapshot whose cold tier is DURABLE: the cold rows
// are NOT serialized into the snapshot (they persist in the cold tier across
// restart), and the count field's low 31 bits hold the RESIDENT count only. A
// snapshot with this flag can only be loaded with a matching durable cold tier
// attached, else it would silently drop the cold half of state.
const snapColdDurableFlag = uint32(1) << 31

// parseColdByRootRootKey extracts the ROOT (commentPK) from a cold by-root key:
//
//	[coldTableCommentByRoot] [rootAuthor: 16B zero-padded] [rootPermlink] [0x00] [child…]
//
// Index-only (no value decode), so rebuilding the per-root cold counts on a
// durable restart never faults a cold data page.
func parseColdByRootRootKey(key []byte) (commentPK, bool) {
	if len(key) < 1+state.AccountNameKeyLen+1 || key[0] != coldTableCommentByRoot {
		return commentPK{}, false
	}
	raw := key[1 : 1+state.AccountNameKeyLen]
	if z := bytes.IndexByte(raw, 0); z >= 0 {
		raw = raw[:z]
	}
	rootAuthor := string(raw)
	rest := key[1+state.AccountNameKeyLen:]
	z := bytes.IndexByte(rest, 0x00)
	if z < 0 {
		return commentPK{}, false
	}
	rootPermlink := string(rest[:z])
	return newCommentPK(auth.AccountName(rootAuthor), rootPermlink), true
}

// rebuildColdAccountingFromDurable reconstructs coldTotal and coldRoot from the
// durable cold tier's restored index, WITHOUT serializing/decoding any cold
// record. coldTotal is the primary-comment key count; coldRoot is the per-root
// by-root key count (present only when the by-root cold index was written —
// pre-HF17; post-HF17 it's empty and unused, which is correct).
func (s *CommentStore) rebuildColdAccountingFromDurable(d durableColdTier) {
	s.coldTotal = d.countTable(coldTableCommentPrimary)
	s.coldRoot = make(map[commentPK]int)
	d.rangeTableKeys(coldTableCommentByRoot, func(key []byte) bool {
		if rk, ok := parseColdByRootRootKey(key); ok {
			s.coldRoot[rk]++
		}
		return true
	})
	// Rebuild the cold bloom: a RAM miss probes the cold tier only when the
	// bloom says "maybe present", so without re-adding every cold comment's raw
	// key the cold half would be invisible after restart. The bloom key is the
	// cold primary key minus its leading table byte (== appendCommentKeyRaw).
	if s.bloom != nil {
		d.rangeTableKeys(coldTableCommentPrimary, func(key []byte) bool {
			if len(key) > 1 {
				s.bloom.add(key[1:])
			}
			return true
		})
	}
}

// durableColdAttached reports whether a durable cold tier is attached.
func (s *CommentStore) durableColdAttached() (durableColdTier, bool) {
	if s.cold == nil {
		return nil, false
	}
	d, ok := s.cold.(durableColdTier)
	return d, ok && d.durable()
}
