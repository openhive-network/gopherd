package chain

import (
	"testing"

	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// TestValidateBlockChainContinuity ports the previous-id check from
// database.cpp::validate_block_header. A block whose `previous` does
// not match dgpo.head_block_id is rejected.
func TestValidateBlockChainContinuity(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	chain := New(db)

	// Apply block 1 cleanly.
	st := tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(op.Custom{ID: 1, RequiredAuths: []string{"initminer"}, Data: []byte{0}}),
		Expiration: network.GenesisTimeUnix + 60,
	}}
	raw1 := buildBlock(t, 1, "initminer", network.GenesisTimeUnix+3, []tx.SignedTransaction{st})
	if _, err := chain.ApplyBlock(raw1, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
		t.Fatalf("apply block 1: %v", err)
	}

	// Block 2 with INTENTIONALLY bad `previous` (all-zero).
	hdr := block.SignedHeader{
		Header: block.Header{
			Previous:   block.ID{}, // wrong — should be block-1 ID
			Timestamp:  network.GenesisTimeUnix + 6,
			Witness:    "initminer",
			Extensions: []byte{0},
		},
	}
	hdrBytes := block.MarshalSignedHeader(nil, &hdr)
	raw2 := append(hdrBytes, 0) // 0 transactions

	// Validation must fail (live-sync mode: SkipBlockSignature=false).
	if _, err := chain.ApplyBlock(raw2, 2, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: false}, nil); err == nil {
		t.Fatalf("expected previous-id mismatch rejection")
	}

	// Same block accepted in replay mode (SkipBlockSignature=true).
	if _, err := chain.ApplyBlock(raw2, 2, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
		t.Fatalf("replay-mode should accept: %v", err)
	}
}

// TestValidateBlockMerkleRootMismatch confirms validate_block detects
// a corrupted merkle root in a block header.
func TestValidateBlockMerkleRootMismatch(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	chain := New(db)

	// Build a valid block then corrupt the header's merkle root.
	st := tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(op.Custom{ID: 1, RequiredAuths: []string{"initminer"}, Data: []byte{0}}),
		Expiration: network.GenesisTimeUnix + 60,
	}}
	raw := buildBlock(t, 1, "initminer", network.GenesisTimeUnix+3, []tx.SignedTransaction{st})

	// Re-parse, corrupt merkle, re-marshal.
	hdr, _, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	hdr.TransactionMerkleRoot = [20]byte{0xff, 0xff, 0xff}
	hdrBytes := block.MarshalSignedHeader(nil, &hdr)
	// Reconstruct: corrupted header || original tx_count varint + body bytes.
	_, hdrLen, _ := block.UnmarshalSignedHeader(raw)
	corrupted := append(hdrBytes, raw[hdrLen:]...)

	// Live-sync: merkle mismatch must reject.
	if _, err := chain.ApplyBlock(corrupted, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: false}, nil); err == nil {
		t.Fatalf("expected merkle-root mismatch rejection")
	}
}

// TestValidateBlockSizeLimit confirms validate_block rejects blocks
// that exceed wso.MedianMaximumBlockSize.
func TestValidateBlockSizeLimit(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	// Tighten the cap to a tiny value so any non-empty block exceeds it.
	{
		s := state.NewSession(db)
		ws, _ := objects.GetWitnessSchedule(s)
		ws.MedianMaximumBlockSize = 100
		objects.PutWitnessSchedule(s, &ws)
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
	}

	chain := New(db)

	// A block with a custom op + JSON metadata easily exceeds 100 bytes.
	st := tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(op.CustomJSON{
			ID:                   "test",
			JSON:                 `{"some":"large enough json metadata payload to exceed our 100-byte cap easily"}`,
			RequiredPostingAuths: []string{"initminer"},
		}),
		Expiration: network.GenesisTimeUnix + 60,
	}}
	raw := buildBlock(t, 1, "initminer", network.GenesisTimeUnix+3, []tx.SignedTransaction{st})

	// Live-sync: block size > limit must reject.
	if _, err := chain.ApplyBlock(raw, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: false}, nil); err == nil {
		t.Fatalf("expected block-size rejection")
	}
}

// TestSlotNumForBlockTime covers the helper used by the witness
// scheduling assert (HIVE_BLOCK_INTERVAL = 3 seconds per slot).
func TestSlotNumForBlockTime(t *testing.T) {
	// Mirrors hived's get_slot_at_time: head_time is rounded DOWN to the
	// previous interval boundary before computing the next slot. With
	// HIVE_BLOCK_INTERVAL=3, head=100 rounds to head_slot_time=99, so
	// the first valid slot starts at 102 — not 103.
	cases := []struct {
		head, block uint32
		wantSlot    uint32
	}{
		{99, 102, 1}, // aligned head, contiguous
		{99, 105, 2}, // aligned head, one missed
		{99, 108, 3},
		{100, 102, 1}, // unaligned head rounds down to 99 → 102 is slot 1
		{100, 105, 2},
		{100, 100, 0}, // block_time < first_slot_time (102) → invalid
		{100, 101, 0},
		{100, 99, 0}, // backwards
	}
	for _, c := range cases {
		got := slotNumForBlockTime(c.head, c.block)
		if got != c.wantSlot {
			t.Errorf("slotNumForBlockTime(head=%d, block=%d) = %d, want %d",
				c.head, c.block, got, c.wantSlot)
		}
	}
}

// TestValidateBlockAcceptsKnownBadMerkle ensures the eight historical
// known-bad-merkle blocks pass even though the algorithmic merkle
// disagrees with the header (per shared_db_merkle.hpp).
func TestValidateBlockAcceptsKnownBadMerkle(t *testing.T) {
	for blockNum, knownMerkle := range block.KnownBadMerkleRoots {
		// MerkleRootAccepted should accept either the algorithmic
		// merkle (which equals knownMerkle for these blocks) or any
		// other value the header happened to record. We pass the
		// known-good algorithmic value as `merkle`; whatever the
		// header was at the time should be accepted.
		var fakeHeaderRoot [20]byte
		fakeHeaderRoot[0] = 0xab // known divergence in header
		if !block.MerkleRootAccepted(blockNum, knownMerkle, fakeHeaderRoot) {
			t.Errorf("block %d: MerkleRootAccepted returned false for the known-good algorithmic merkle",
				blockNum)
		}
	}
}

// TestHeaderHardforkVoteApplies confirms processHeaderExtensions writes
// the witness's HardforkVersionVote / HardforkTimeVote fields when the
// block carries the corresponding extension entry.
func TestHeaderHardforkVoteApplies(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	// Header extension: tag 2 = hardfork_version_vote (8-byte body).
	// flat_set count varint = 1, then varint tag = 2, then body.
	ext := []byte{
		0x01,                   // count = 1
		0x02,                   // tag = 2 (hardfork_version_vote)
		0x14, 0x00, 0x00, 0x01, // v_num = packed (1, 0, 20) — placeholder
		0x80, 0x96, 0x98, 0x00, // time_point_sec = 10000000
	}

	hdr := block.SignedHeader{
		Header: block.Header{
			Previous:   block.ID{},
			Timestamp:  network.GenesisTimeUnix + 3,
			Witness:    "initminer",
			Extensions: ext,
		},
	}
	hdrBytes := block.MarshalSignedHeader(nil, &hdr)
	raw := append(hdrBytes, 0) // empty txs vector

	chain := New(db)
	if _, err := chain.ApplyBlock(raw, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
		t.Fatalf("apply block 1: %v", err)
	}

	// Witness should now reflect the vote.
	w, err := objects.GetWitness(state.NewSession(db), "initminer")
	if err != nil {
		t.Fatal(err)
	}
	if w.HardforkVersionVote == 0 {
		t.Errorf("HardforkVersionVote not updated from header extension; got %d", w.HardforkVersionVote)
	}
	if w.HardforkTimeVote != 10000000 {
		t.Errorf("HardforkTimeVote: got %d, want 10000000", w.HardforkTimeVote)
	}
}

// TestRunningVersionUpdates confirms the version (tag 1) header
// extension propagates to witness.RunningVersion.
func TestRunningVersionUpdates(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	// flat_set with one version vote.
	ext := []byte{
		0x01,                   // count = 1
		0x01,                   // tag = 1 (version)
		0x42, 0x42, 0x42, 0x42, // v_num
	}
	hdr := block.SignedHeader{
		Header: block.Header{
			Timestamp:  network.GenesisTimeUnix + 3,
			Witness:    "initminer",
			Extensions: ext,
		},
	}
	raw := append(block.MarshalSignedHeader(nil, &hdr), 0)

	chain := New(db)
	if _, err := chain.ApplyBlock(raw, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	w, _ := objects.GetWitness(state.NewSession(db), "initminer")
	if w.RunningVersion != 0x42424242 {
		t.Errorf("RunningVersion: got 0x%x, want 0x42424242", w.RunningVersion)
	}
}

// TestUpdateLastIrreversibleBlock verifies the LIB recompute sets
// dgpo.LastIrreversibleBlockNum from the median of the active
// schedule's last_confirmed values.
func TestUpdateLastIrreversibleBlock(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	chain := New(db)
	for i := uint32(1); i <= 5; i++ {
		raw := buildBlock(t, i, "initminer", network.GenesisTimeUnix+i*3, nil)
		if _, err := chain.ApplyBlock(raw, i, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatal(err)
	}
	// With one witness in the schedule (initminer), LIB = initminer's
	// last_confirmed = 5 (last block they produced).
	if dgpo.LastIrreversibleBlockNum != 5 {
		t.Errorf("LIB: got %d, want 5 (median of single-witness schedule)",
			dgpo.LastIrreversibleBlockNum)
	}
}

// TestUnused holds references to imports so a test-only file with
// unconditional skips wouldn't fail to compile in earlier go versions.
// (Pure no-op now; kept for symmetry with C++ tests that group helpers.)
func TestUnused(t *testing.T) {
	_ = hardfork.HF20
	_ = ApplyOptions{}
}
