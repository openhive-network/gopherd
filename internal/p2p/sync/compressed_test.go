package sync

import (
	"testing"
	"time"

	blockzstd "gopherd/internal/blocklog/zstd"
	"gopherd/internal/p2p/proto"
	"gopherd/internal/protocol/block"
)

// buildTestSignedBlock builds the FC-binary form of a minimal signed_block
// (signed_block_header + 0 transactions). Returns the raw bytes plus the
// canonical block_id.
func buildTestSignedBlock(t *testing.T, blockNum uint32, witness string) ([]byte, block.ID) {
	t.Helper()
	if blockNum == 0 {
		t.Fatalf("blockNum must be >= 1")
	}
	var prev block.ID
	// Embed (blockNum-1) into the previous-id so the derived block_num is
	// blockNum (matches block.ConstructID's invariant on the receive side).
	prev[0] = byte((blockNum - 1) >> 24)
	prev[1] = byte((blockNum - 1) >> 16)
	prev[2] = byte((blockNum - 1) >> 8)
	prev[3] = byte(blockNum - 1)
	prev[4] = 0xfe

	sh := block.SignedHeader{
		Header: block.Header{
			Previous:              prev,
			Timestamp:             uint32(time.Now().Unix()),
			Witness:               witness,
			TransactionMerkleRoot: block.ID{},
			Extensions:            []byte{0x00},
		},
	}
	for i := range sh.WitnessSignature {
		sh.WitnessSignature[i] = byte(i + 1)
	}
	hdr := block.MarshalSignedHeader(nil, &sh)
	// Append transaction-count varint = 0.
	raw := append(hdr, 0x00)
	id := block.ConstructID(hdr, blockNum)
	return raw, id
}

func TestHandleCompressedBlockEnqueuesApplyJob(t *testing.T) {
	c := newStubChain()
	c.seed(1, []byte("genesis"))
	d := newWithChain(Config{ApplyQueueSize: 4}, c)

	raw, id := buildTestSignedBlock(t, 2, "alice")

	comp := blockzstd.NewCompressor()
	dictNum, _ := blockzstd.LastAvailableDictNumber()
	payload, err := comp.Compress(raw, dictNum)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	msg := proto.CompressedBlockMessage{
		Zstd:             true,
		HasDictionary:    true,
		DictionaryNumber: dictNum,
		CompressedBytes:  payload,
	}
	body, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	d.handleCompressedBlock(nil, body)

	// Expect one applyJob enqueued with the right id + raw.
	select {
	case j := <-d.applyQ:
		if j.id != id {
			t.Errorf("applyJob.id mismatch:\n got %x\nwant %x", j.id, id)
		}
		if j.blockNum != 2 {
			t.Errorf("applyJob.blockNum: got %d, want 2", j.blockNum)
		}
		if string(j.rawBlock) != string(raw) {
			t.Errorf("applyJob.rawBlock mismatch")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no applyJob enqueued")
	}

	st := d.Stats()
	if st.CompressedReceived != 1 {
		t.Errorf("CompressedReceived: got %d, want 1", st.CompressedReceived)
	}
	if st.CompressedDecompressed != 1 {
		t.Errorf("CompressedDecompressed: got %d, want 1", st.CompressedDecompressed)
	}
	if st.CompressedDecodeErrors != 0 {
		t.Errorf("CompressedDecodeErrors: got %d, want 0", st.CompressedDecodeErrors)
	}
}

func TestHandleCompressedBlockDedupRing(t *testing.T) {
	c := newStubChain()
	c.seed(1, []byte("genesis"))
	d := newWithChain(Config{ApplyQueueSize: 4}, c)

	raw, _ := buildTestSignedBlock(t, 2, "alice")
	comp := blockzstd.NewCompressor()
	dictNum, _ := blockzstd.LastAvailableDictNumber()
	payload, err := comp.Compress(raw, dictNum)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	msg := proto.CompressedBlockMessage{
		Zstd:             true,
		HasDictionary:    true,
		DictionaryNumber: dictNum,
		CompressedBytes:  payload,
	}
	body, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// First arrival: should enqueue.
	d.handleCompressedBlock(nil, body)
	select {
	case <-d.applyQ:
	case <-time.After(time.Second):
		t.Fatalf("first arrival: no applyJob")
	}

	// Second arrival of the same compressed payload: dedup ring catches it
	// AFTER decompress (the cache is keyed by block_id, which we only know
	// post-decompress). So decompressed counter goes up but enqueue does not.
	d.handleCompressedBlock(nil, body)
	select {
	case <-d.applyQ:
		t.Fatalf("second arrival should have been deduped, but enqueued")
	case <-time.After(50 * time.Millisecond):
	}
	st := d.Stats()
	if st.CompressedCacheHits != 1 {
		t.Errorf("CompressedCacheHits: got %d, want 1", st.CompressedCacheHits)
	}
	if st.CompressedReceived != 2 {
		t.Errorf("CompressedReceived: got %d, want 2", st.CompressedReceived)
	}
}

func TestHandleCompressedBlockBadFlags(t *testing.T) {
	c := newStubChain()
	d := newWithChain(Config{ApplyQueueSize: 4}, c)
	// 1-byte body — too short.
	d.handleCompressedBlock(nil, []byte{0x80})
	st := d.Stats()
	if st.CompressedDecodeErrors != 1 {
		t.Errorf("CompressedDecodeErrors: got %d, want 1", st.CompressedDecodeErrors)
	}
}
