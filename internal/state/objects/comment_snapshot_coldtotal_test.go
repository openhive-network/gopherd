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
// The precise drift source in the 109235509 save is NOT established. The
// obvious candidate — evict, promote back, evict again — is refuted: that path
// pairs its decrement correctly and does not drift (verified while writing this
// test). That uncertainty is the argument FOR the v2 format rather than for
// repairing the counter: v2 ends the stream with a terminator and demotes the
// counts to presize hints, so a drifted counter costs map rehashing instead of
// producing an unloadable file, whatever the cause turns out to be.
//
// The test therefore injects the drift directly rather than pantomiming a
// mechanism we have not proven. That is the exact condition v2 promises to
// survive, and it holds no matter which increment site is at fault.
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
