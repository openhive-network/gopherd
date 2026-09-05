package objects

import (
	"bytes"
	"encoding/binary"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// Regression test for the silently-corrupt shutdown snapshot of 2026-09-05.
//
// A snapshot saved at mainnet block 109235509 reported success, exited 0 and
// renamed .tmp to final — then failed to load, because its header declared
// 135,610,353 comment records while the file held 135,595,979.
//
// Root cause: v1 wrote `len(s.primary) + s.coldTotal` as an EXACT record
// contract, but coldTotal counts cold WRITES and bookkeeping calls, not rows.
// Every increment site does `s.cold.bset(key, blob)` (an UPSERT) followed by an
// unconditional `s.coldTotal++`, and spillToColdReuse increments without
// writing at all — it trusts that a warm-reused cold dir already holds the row.
// MarshalSnapshot streams distinct ROWS via rangePrimaryByOffset, so any
// increment not backed by a distinct row leaves the header ahead of the stream
// and the loader hits EOF partway through.
//
// The specific source for the 109235509 save is RE-FREEZING over a cold dir
// that is ahead of the snapshot — see
// TestColdTotalDriftsWhenColdDirIsAheadOfSnapshot, which reproduces it at
// 1 drift per re-freeze. (The obvious candidate, evict/promote/evict, is
// refuted: it pairs its decrement correctly.)
//
// This test deliberately does NOT depend on that. It injects the drift
// directly, because what v2 guarantees is a property of the FORMAT — a header
// count that overstates the stream must not make the file unloadable — and that
// has to hold for drift sources nobody has found yet, not just the one we did.
func TestSnapshotSurvivesColdTotalDrift(t *testing.T) {
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	s, cs := commentFixture(t, cold)

	if !cs.Evict("replyer-a", "re-post") {
		t.Fatal("evict replyer-a failed")
	}
	if !cs.Evict("mid", "re-post3") {
		t.Fatal("evict mid failed")
	}
	if _, err := GetComment(s, auth.AccountName("replyer-a"), "re-post"); err != nil {
		t.Fatalf("cold read back: %v", err)
	}

	// Inject the drift: the counter now claims more cold rows than the tier
	// holds, exactly as the mainnet save did (by 14,374 there).
	cs.coldTotal += 3

	var buf bytes.Buffer
	if err := cs.MarshalSnapshot(&buf); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The header's count is now KNOWN to overstate the stream. Under v1 this is
	// exactly what made the file unloadable; under v2 it must not matter.
	raw := buf.Bytes()
	residentHint := binary.LittleEndian.Uint32(raw[8:12]) &^ snapColdDurableFlag
	coldHint := binary.LittleEndian.Uint32(raw[12:16])
	recs, ok := snapshotRecords(t, raw)
	if !ok {
		t.Fatal("snapshot did not parse")
	}
	if uint32(len(recs)) >= residentHint+coldHint {
		t.Fatalf("harness did not reproduce the drift: hints total %d, records %d — "+
			"the header must overstate the stream for this test to mean anything",
			residentHint+coldHint, len(recs))
	}

	// THE GATE: it loads anyway, with every record present.
	cold2, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold2.Close()
	db2 := openTempDB(t)
	s2 := state.NewSession(db2)
	cs2 := EnsureCommentStore(s2)
	cs2.SetColdTier(cold2)
	if err := cs2.UnmarshalSnapshot(bytes.NewReader(raw)); err != nil {
		t.Fatalf("load of a snapshot with a drifted count header failed: %v "+
			"(this is the 2026-09-05 mainnet failure)", err)
	}

	for _, want := range []struct{ a, p string }{
		{"root1", "post"}, {"replyer-a", "re-post"}, {"replyer-z", "re-post2"},
		{"mid", "re-post3"}, {"root2", "hello"}, {"bob", "re-hello"},
	} {
		if _, err := GetComment(s2, auth.AccountName(want.a), want.p); err != nil {
			t.Errorf("comment %s/%s missing after reload: %v", want.a, want.p, err)
		}
	}
}

// The v1 failure itself, reproduced: a counted snapshot whose header overstates
// its record stream is unloadable. This is the shape of the block-109235509
// file (declared 135,610,353, held 135,595,979), and it is what v2 makes
// impossible to produce.
//
// It also pins the other half of the contract: v1 must keep failing LOUDLY
// rather than being taught to stop at EOF. Silently accepting a short stream
// would turn a dead snapshot into a snapshot that is quietly missing comments —
// consensus state with holes in it, which is far worse than a failed boot.
func TestSnapshotV1OverstatedCountStillFails(t *testing.T) {
	_, cs := commentFixture(t, nil)

	var v2 bytes.Buffer
	if err := cs.MarshalSnapshot(&v2); err != nil {
		t.Fatal(err)
	}
	recs, ok := snapshotRecords(t, v2.Bytes())
	if !ok {
		t.Fatal("v2 snapshot did not parse")
	}

	// v1 layout, count claiming two more records than are present.
	v1 := encodeV1(recs, uint32(len(recs)+2))

	db2 := openTempDB(t)
	cs2 := EnsureCommentStore(state.NewSession(db2))
	err := cs2.UnmarshalSnapshot(bytes.NewReader(v1))
	if err == nil {
		t.Fatal("a v1 snapshot with an overstated count loaded silently — " +
			"that would restore consensus state with comments missing")
	}
	t.Logf("v1 overstated count correctly refused: %v", err)
}

// encodeV1 writes records in the legacy counted layout with an arbitrary count.
func encodeV1(recs [][]byte, count uint32) []byte {
	var b bytes.Buffer
	b.Write(commentStoreSnapshotMagic[:])
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], commentStoreSnapshotVersionCounted)
	binary.LittleEndian.PutUint32(hdr[4:8], count)
	b.Write(hdr[:])
	var varBuf [binary.MaxVarintLen64]byte
	for _, r := range recs {
		n := binary.PutUvarint(varBuf[:], uint64(len(r)))
		b.Write(varBuf[:n])
		b.Write(r)
	}
	return b.Bytes()
}

// A v1 snapshot (exact count, no terminator) must still load — the mainnet
// snapshots on disk are v1, and a node that cannot read them cannot restart.
func TestSnapshotV1StillLoads(t *testing.T) {
	s, cs := commentFixture(t, nil)
	_ = s

	var v2 bytes.Buffer
	if err := cs.MarshalSnapshot(&v2); err != nil {
		t.Fatal(err)
	}
	recs, ok := snapshotRecords(t, v2.Bytes())
	if !ok {
		t.Fatal("v2 snapshot did not parse")
	}

	// Re-encode those records in the v1 layout: magic | version=1 | exact count.
	v1 := encodeV1(recs, uint32(len(recs)))

	db2 := openTempDB(t)
	s2 := state.NewSession(db2)
	cs2 := EnsureCommentStore(s2)
	if err := cs2.UnmarshalSnapshot(bytes.NewReader(v1)); err != nil {
		t.Fatalf("v1 snapshot no longer loads: %v — every snapshot on disk is v1", err)
	}
	if cs2.Count() != len(recs) {
		t.Fatalf("v1 load recovered %d comments, want %d", cs2.Count(), len(recs))
	}
}

// TestColdTotalDriftsWhenColdDirIsAheadOfSnapshot identifies the drift source
// behind the block-109235509 snapshot (see TestSnapshotSurvivesColdTotalDrift,
// which deliberately does not depend on knowing it).
//
// The node runs GOPHERD_COLD_REUSE=1 against a cold dir that the PREVIOUS run
// advanced past the snapshot being loaded: cold2 held rows for comments frozen
// up to 109,547,208 while we reloaded a snapshot at 108,951,302 and re-applied
// forward. Every comment that froze again in that replayed window ALREADY had
// a cold row, so its eviction bset() overwrote in place — no new row, but an
// unconditional coldTotal++. The counter therefore counts re-freezes, and the
// snapshot header inherits the error.
//
// This models that directly: evict a comment, drop the resident-side
// bookkeeping as a snapshot reload would, then evict it again with the row
// still on disk.
func TestColdTotalDriftsWhenColdDirIsAheadOfSnapshot(t *testing.T) {
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	_, cs := commentFixture(t, cold)

	if !cs.Evict("replyer-a", "re-post") {
		t.Fatal("evict failed")
	}
	rowsOnDisk := countColdPrimaryRows(cs)
	if rowsOnDisk != 1 {
		t.Fatalf("expected 1 cold row after evict, got %d", rowsOnDisk)
	}

	// A snapshot reload rebuilds the resident set from the file; the cold dir
	// keeps its rows. Re-put the comment resident so it can be evicted again,
	// exactly as the replayed window does.
	sBack := state.NewSession(openTempDB(t))
	csBack := EnsureCommentStore(sBack)
	csBack.SetColdTier(cold)
	PutComment(sBack, &Comment{
		Author: auth.AccountName("replyer-a"), Permlink: "re-post",
		RootAuthor: auth.AccountName("root1"), RootPermlink: "post",
		NetRshares: 10, WasPaid: true,
	})
	if err := sBack.Commit(false); err != nil {
		t.Fatal(err)
	}
	before := csBack.coldTotal
	if !csBack.Evict("replyer-a", "re-post") {
		t.Fatal("re-evict failed")
	}

	rowsAfter := countColdPrimaryRows(csBack)
	drift := (csBack.coldTotal - before) - (rowsAfter - rowsOnDisk)
	if drift == 0 {
		t.Fatalf("no drift reproduced: coldTotal +%d, rows +%d",
			csBack.coldTotal-before, rowsAfter-rowsOnDisk)
	}
	t.Logf("REPRODUCED: re-freezing a comment whose cold row already exists "+
		"advanced coldTotal by %d while adding %d rows (drift %d per re-freeze)",
		csBack.coldTotal-before, rowsAfter-rowsOnDisk, drift)
}

// countColdPrimaryRows counts distinct primary rows actually present in the
// cold tier — the quantity MarshalSnapshot streams, as opposed to coldTotal.
func countColdPrimaryRows(cs *CommentStore) int {
	n := 0
	_ = cs.cold.rangePrimaryByOffset(tableKey(coldTableCommentPrimary, nil), func(v []byte) bool {
		n++
		return true
	})
	return n
}
