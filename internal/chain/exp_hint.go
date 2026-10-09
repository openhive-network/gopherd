package chain

import (
	"gopherd/internal/state"
)

// expHintLowerBound turns the Database's "earliest pending expiration" hint
// for (table, index) into a RangeFrom lower bound: the big-endian time that
// every row of a by-time index starts with. nil (scan from the start) when no
// hint is recorded, e.g. right after a restart, so the first sweep rebuilds it.
//
// Safe because the hint is never above a live row's time: every Put that
// inserts into these indexes lowers it (NudgeExpHint), each sweep sets it to
// the first row it left behind, and a lower-than-necessary hint only costs
// a few tombstones.
func expHintLowerBound(s *state.Session, table state.TableID, index state.IndexID) []byte {
	hint, ok := s.GetExpHint(table, index)
	if !ok || hint == 0 {
		return nil
	}
	return state.AppendUint32BE(nil, hint)
}
