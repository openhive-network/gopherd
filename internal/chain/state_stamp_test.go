package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
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

// TestStateStampRootIsHivedsMerkle pins calculate_merkle_root (hived
// libraries/protocol/merkle.cpp) on the shapes that matter: empty, single,
// even and odd layers, with the odd digest carried up unhashed.
func TestStateStampRootIsHivedsMerkle(t *testing.T) {
	a, b, c := sha256.Sum256([]byte("first")), sha256.Sum256([]byte("second")), sha256.Sum256([]byte("third"))
	pair := func(x, y [32]byte) [32]byte { return sha256.Sum256(append(x[:], y[:]...)) }
	if got := StateStampRoot(nil); got != ([20]byte{}) {
		t.Errorf("empty: %x, want zero", got)
	}
	if got, want := StateStampRoot([][32]byte{a}), crypto.RIPEMD160(a[:]); got != want {
		t.Errorf("single: %x, want %x", got, want)
	}
	ab := pair(a, b)
	if got, want := StateStampRoot([][32]byte{a, b}), crypto.RIPEMD160(ab[:]); got != want {
		t.Errorf("pair: %x, want %x", got, want)
	}
	abc := pair(ab, c)
	if got, want := StateStampRoot([][32]byte{a, b, c}), crypto.RIPEMD160(abc[:]); got != want {
		t.Errorf("odd: %x, want %x", got, want)
	}
	// Order matters: the root commits to publication order.
	if StateStampRoot([][32]byte{a, b}) == StateStampRoot([][32]byte{b, a}) {
		t.Error("root ignores order")
	}
}

func hf29Set() hardfork.Set {
	return hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF28]+1) | 1<<hardfork.HF29
}

func headerWithExtensions(exts ...block.HeaderExtension) *block.SignedHeader {
	return &block.SignedHeader{Header: block.Header{
		Witness:    "alice",
		Extensions: block.AppendHeaderExtensions(nil, exts),
	}}
}

func stampExt(root [20]byte) block.HeaderExtension {
	return block.HeaderExtension{Tag: block.HeaderExtStateStamp, StateMerkleRoot: root}
}

// TestStateStampExtensionHF29 mirrors hived's state_stamp_negative_tests
// (450441091) and the consume-on-accept half of state_stamp_positive:
// a stamp-carrying block is required exactly when stamps are pending, its
// root must be theirs, and acceptance resets the list.
func TestStateStampExtensionHF29(t *testing.T) {
	hf := hf29Set()
	pendingDigest := sha256.Sum256([]byte("diverge"))
	right := StateStampRoot([][32]byte{pendingDigest})
	wrong := crypto.RIPEMD160OfSHA256([]byte("wrong"))

	withPending := func() *state.Session {
		s := state.NewSession(state.OpenMem())
		objects.PutStateStampData(s, &objects.StateStampData{Stamps: [][32]byte{pendingDigest}})
		return s
	}
	empty := func() *state.Session {
		s := state.NewSession(state.OpenMem())
		objects.PutStateStampData(s, &objects.StateStampData{})
		return s
	}

	// reject_unexpected_stamp
	if err := processHeaderExtensions(empty(), headerWithExtensions(stampExt(wrong)), hf); err == nil ||
		!strings.Contains(err.Error(), "Unexpected state stamp") {
		t.Errorf("unexpected stamp: err = %v", err)
	}
	// Nothing pending, nothing carried: fine.
	if err := processHeaderExtensions(empty(), headerWithExtensions(), hf); err != nil {
		t.Errorf("empty block, nothing pending: %v", err)
	}
	// reject_missing_stamp
	if err := processHeaderExtensions(withPending(), headerWithExtensions(), hf); err == nil ||
		!strings.Contains(err.Error(), "missing the required state stamp") {
		t.Errorf("missing stamp: err = %v", err)
	}
	// A version vote alone does not stand in for the stamp.
	if err := processHeaderExtensions(withPending(), headerWithExtensions(
		block.HeaderExtension{Tag: block.HeaderExtVersion, Version: 0x011D0000}), hf); err == nil {
		t.Error("version-only block accepted while stamps are pending")
	}
	// reject_mismatched_stamp
	s := withPending()
	if err := processHeaderExtensions(s, headerWithExtensions(stampExt(wrong)), hf); err == nil ||
		!strings.Contains(err.Error(), "State stamp mismatch") {
		t.Errorf("mismatched stamp: err = %v", err)
	}
	if d := stampList(t, s); len(d.Stamps) != 1 {
		t.Errorf("a refused block consumed the list: %+v", d)
	}

	// The right root is accepted and consumes the list.
	s = withPending()
	if err := processHeaderExtensions(s, headerWithExtensions(stampExt(right)), hf); err != nil {
		t.Fatalf("correct stamp refused: %v", err)
	}
	if d := stampList(t, s); len(d.Stamps) != 0 || d.Incomplete {
		t.Fatalf("list not reset after a stamp-carrying block: %+v", d)
	}
	// ...so the next block must not carry one.
	if err := processHeaderExtensions(s, headerWithExtensions(stampExt(right)), hf); err == nil {
		t.Error("second stamp for the same list accepted")
	}

	// Two stamps on the wire: hived keeps the first (flat_set), so the first
	// decides, whatever the second says.
	twoStamps := func(first, second [20]byte) *block.SignedHeader {
		raw := []byte{0x02, 0x03}
		raw = append(raw, first[:]...)
		raw = append(raw, 0x03)
		raw = append(raw, second[:]...)
		return &block.SignedHeader{Header: block.Header{Witness: "alice", Extensions: raw}}
	}
	if err := processHeaderExtensions(withPending(), twoStamps(right, wrong), hf); err != nil {
		t.Errorf("right stamp followed by a duplicate refused: %v", err)
	}
	if err := processHeaderExtensions(withPending(), twoStamps(wrong, right), hf); err == nil {
		t.Error("wrong stamp followed by the right one accepted")
	}

	// Before HF29 a pending list is neither required nor consumed.
	pre := hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF28] + 1)
	if !pre.Has(hardfork.HF29) {
		s = withPending()
		if err := processHeaderExtensions(s, headerWithExtensions(), pre); err != nil {
			t.Errorf("pre-HF29 block without stamp: %v", err)
		}
		if d := stampList(t, s); len(d.Stamps) != 1 {
			t.Errorf("pre-HF29 block consumed the list: %+v", d)
		}
	}
}

// TestStateStampExtensionRefusesIncompleteList — a state missing the list,
// or holding one rebuilt after stamps had already been published, cannot
// validate an HF29 block; it says so instead of reporting a mismatch.
func TestStateStampExtensionRefusesIncompleteList(t *testing.T) {
	hf := hf29Set()
	for name, s := range map[string]*state.Session{
		"missing":    state.NewSession(state.OpenMem()),
		"incomplete": state.NewSession(state.OpenMem()),
	} {
		if name == "incomplete" {
			objects.PutStateStampData(s, &objects.StateStampData{Stamps: [][32]byte{{1}}, Incomplete: true})
		}
		for _, hdr := range []*block.SignedHeader{headerWithExtensions(), headerWithExtensions(stampExt([20]byte{1}))} {
			err := processHeaderExtensions(s, hdr, hf)
			if err == nil || !strings.Contains(err.Error(), "replay from before HF20") {
				t.Errorf("%s list: err = %v", name, err)
			}
		}
		if _, _, err := PendingStateStampRoot(s); err == nil {
			t.Errorf("%s list: PendingStateStampRoot succeeded", name)
		}
	}
}
