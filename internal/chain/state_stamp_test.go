package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// firstRCReportBlock is the first RC stats report after HF20 (26,256,743) on
// mainnet: 912 * 28800.
const firstRCReportBlock uint32 = 26_265_600

func stampList(t *testing.T, s *state.Session) objects.StateStampData {
	t.Helper()
	d, ok, err := objects.GetStateStampData(s)
	if !ok || err != nil {
		t.Fatalf("state_stamp_data: ok=%v err=%v", ok, err)
	}
	return d
}

// TestRCStatsReportPublishesStamp — hived a5edbbf29: handle_auto_report
// publishes the closed RC stamp (SHA-256 over every payer's post-charge
// mana, little-endian int64) into state_stamp_data_object at every report
// block, before HF29 too, and nothing consumes the list before HF29.
func TestRCStatsReportPublishesStamp(t *testing.T) {
	s := state.NewSession(state.OpenMem())
	objects.PutStateStampData(s, &objects.StateStampData{})
	objects.PutRCStats(s, objects.RCStatsIDPending, &objects.RCStats{BlockNum: firstRCReportBlock - rcStatsReportFreqBlocks})

	h := sha256.New()
	for _, rc := range []int64{4_000_000_000_000, 100, -5} {
		objects.AddRCStats(s, &objects.RCTxInfo{Op: -1, RC: rc})
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(rc))
		h.Write(b[:])
	}
	var first [32]byte
	copy(first[:], h.Sum(nil))

	// Not a report block: nothing is published.
	finalizeRCStats(s, firstRCReportBlock-1)
	if d := stampList(t, s); len(d.Stamps) != 0 {
		t.Fatalf("published off the report boundary: %x", d.Stamps)
	}

	finalizeRCStats(s, firstRCReportBlock)
	if d := stampList(t, s); len(d.Stamps) != 1 || d.Stamps[0] != first || d.Incomplete {
		t.Fatalf("after first report: %+v, want [%x]", d, first)
	}
	if a, _ := objects.GetRCStats(s, objects.RCStatsIDArchive); a.Stamp != first {
		t.Fatalf("archived stamp %x differs from the published one %x", a.Stamp, first)
	}

	// A window without transactions still publishes: SHA-256 of nothing,
	// which is what a fresh restartable_sha256 finishes to.
	finalizeRCStats(s, firstRCReportBlock+rcStatsReportFreqBlocks)
	empty := sha256.Sum256(nil)
	d := stampList(t, s)
	if len(d.Stamps) != 2 || d.Stamps[0] != first || d.Stamps[1] != empty {
		t.Fatalf("after second report: %x, want [%x %x]", d.Stamps, first, empty)
	}
}

// TestStateStampListMissingRow — a state written before gopherd kept the
// list has no row. Created at the chain's first report nothing was missed;
// created at any later report the list lacks earlier stamps and is marked
// Incomplete so the HF29 check refuses it.
func TestStateStampListMissingRow(t *testing.T) {
	s := state.NewSession(state.OpenMem())
	finalizeRCStats(s, firstRCReportBlock)
	if d := stampList(t, s); d.Incomplete || len(d.Stamps) != 1 {
		t.Fatalf("first report without a row: %+v", d)
	}

	s = state.NewSession(state.OpenMem())
	finalizeRCStats(s, firstRCReportBlock+rcStatsReportFreqBlocks)
	if d := stampList(t, s); !d.Incomplete || len(d.Stamps) != 1 {
		t.Fatalf("later report without a row: %+v, want Incomplete", d)
	}
}
