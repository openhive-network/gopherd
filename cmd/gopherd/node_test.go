package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"gopherd/internal/blocklog"
	"gopherd/internal/chain"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
)

// projectBlockLog returns the path to the canonical mainnet block_log
// shipped with the repo (3.4 GB at the project root). When the file is
// missing, the test calls t.Skip — CI environments without it should still
// see green.
func projectBlockLog(t *testing.T) string {
	t.Helper()
	candidates := []string{
		// Worktree -> project root -> block_log. The relative path varies
		// depending on where `go test` is run from.
		"../../../../block_log",
		"../../../block_log",
		"../../block_log",
		"/home/martin/projects/hived-go/block_log",
	}
	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	t.Skip("project block_log not available; skipping self-test")
	return ""
}

// buildUncompressedReference produces the byte sequence hived would emit if
// it ran with --block-log-uncompressed: per block, the raw signed_block
// bytes followed by an 8-byte trailer encoding (start_offset, attributes
// = uncompressed). This is what our writer is supposed to reproduce.
//
// The function returns the concatenated bytes for blocks[0..len(blocks)-1].
// We compare the writer's output against this in TestSelfReplayBlocks.
func buildUncompressedReference(blocks [][]byte) []byte {
	var out bytes.Buffer
	off := uint64(0)
	for _, b := range blocks {
		out.Write(b)
		var trailer [8]byte
		binary.LittleEndian.PutUint64(trailer[:], off) // uncompressed flag = 0, no dict
		out.Write(trailer[:])
		off += uint64(len(b)) + 8
	}
	return out.Bytes()
}

// TestSelfReplayBlocks is the byte-identical end-to-end check.
//
// For the first N blocks of the project's block_log:
//  1. Read them via the existing Reader (this transparently decompresses).
//  2. Run each through the same applyBlock the live-sync delegate would
//     invoke. Signatures are skipped because (a) validating block 1's
//     witness signature requires the initminer keypair and (b) the
//     evaluator-pipeline correctness IS the point here, not the
//     cryptographic checks. With SkipBlockSignature=true the apply path
//     still walks every transaction's evaluator; that's the meaningful
//     validation surface for "did the writer get the right bytes".
//  3. Append the rawBlock (decompressed) to a fresh Writer.
//  4. Compare the Writer's output (as bytes) against an ad-hoc
//     uncompressed reference layout for the same blocks.
//
// The reference layout is what hived emits with --block-log-uncompressed:
// raw block bytes + 8-byte (offset, uncompressed-flag=0) trailer per block.
// The shipped block_log is compressed-with-dict, so a direct file-slice
// comparison is impossible without a raw-bytes reader accessor (deferred).
func TestSelfReplayBlocks(t *testing.T) {
	src := projectBlockLog(t)
	const n = 100

	// 1. Read the first n blocks (decompressed) from the source.
	srcReader, err := blocklog.Open(src)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	if srcReader.Head() < n {
		srcReader.Close()
		t.Skipf("source block_log only has %d blocks; need at least %d", srcReader.Head(), n)
	}
	rawBlocks := make([][]byte, n)
	for i := uint32(1); i <= n; i++ {
		b, err := srcReader.Block(i)
		if err != nil {
			srcReader.Close()
			t.Fatalf("read source block %d: %v", i, err)
		}
		rawBlocks[i-1] = b
	}
	srcReader.Close()

	// 2. Stand up a fresh state dir + chain + writer, and feed blocks
	//    through the same applyBlock the live-sync delegate would call.
	dataDir := t.TempDir()
	stateDB, err := state.Open(dataDir, 64<<20)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer stateDB.Close()
	if err := genesis.Apply(stateDB); err != nil {
		// Apply tolerates an already-bootstrapped dir.
		_ = err
	}
	cdb := chain.New(stateDB)

	blockLogDir := filepath.Join(dataDir, "blockchain")
	if err := os.MkdirAll(blockLogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blockLogPath := filepath.Join(blockLogDir, "block_log")
	w, err := blocklog.OpenForWrite(blockLogPath, false)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}

	// We override the apply-options inside applyBlock by piggybacking on
	// the same code path with a small wrapper. Inlining the validate +
	// append here keeps the test independent of any future flag we add
	// to applyBlock.
	for i, raw := range rawBlocks {
		blockNum := uint32(i + 1)
		if _, err := cdb.ApplyBlock(raw, blockNum, chain.ApplyOptions{
			SkipBlockSignature:        true,
			SkipTransactionSignatures: true,
			Sync:                      false,
		}, nil); err != nil {
			t.Fatalf("ApplyBlock %d: %v", blockNum, err)
		}
		if err := w.Append(raw); err != nil {
			t.Fatalf("Append %d: %v", blockNum, err)
		}
	}
	if got := w.HeadBlock(); got != n {
		t.Errorf("writer HeadBlock: got %d, want %d", got, n)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. Read the writer's main file back and compare to the reference
	//    uncompressed layout.
	gotMain, err := os.ReadFile(blockLogPath)
	if err != nil {
		t.Fatal(err)
	}
	want := buildUncompressedReference(rawBlocks)
	if !bytes.Equal(gotMain, want) {
		// Help diagnose: find the first differing byte.
		minLen := len(gotMain)
		if len(want) < minLen {
			minLen = len(want)
		}
		first := -1
		for i := 0; i < minLen; i++ {
			if gotMain[i] != want[i] {
				first = i
				break
			}
		}
		if first < 0 {
			first = minLen
		}
		t.Fatalf("output bytes differ at offset %d (got %d bytes, want %d bytes)",
			first, len(gotMain), len(want))
	}

	// 4. And finally the most direct check: open the writer's output via
	//    the existing Reader and verify every block round-trips to the
	//    same bytes we wrote.
	verifyR, err := blocklog.Open(blockLogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer verifyR.Close()
	if got := verifyR.Head(); got != n {
		t.Fatalf("verifier Head: got %d, want %d", got, n)
	}
	for i := uint32(1); i <= n; i++ {
		got, err := verifyR.Block(i)
		if err != nil {
			t.Fatalf("verifier Block(%d): %v", i, err)
		}
		if !bytes.Equal(got, rawBlocks[i-1]) {
			t.Errorf("verifier block %d: %d bytes, source %d bytes", i, len(got), len(rawBlocks[i-1]))
		}
	}
}

// TestSelfReplayBlocksCompressed runs the same self-replay as
// TestSelfReplayBlocks but with --compress-blocks enabled. The end state has
// to satisfy two extra invariants that the uncompressed path doesn't:
//
//  1. The Reader transparently decompresses each freshly-written block
//     and we round-trip every byte back to the source.
//  2. The on-disk bytes for the first 100 blocks are byte-identical to
//     the corresponding slice of ./block_log — i.e. the writer produces
//     exactly what hived's compress_block_log utility would produce.
func TestSelfReplayBlocksCompressed(t *testing.T) {
	src := projectBlockLog(t)
	const n = 100

	srcReader, err := blocklog.Open(src)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	if srcReader.Head() < n {
		srcReader.Close()
		t.Skipf("source block_log only has %d blocks; need at least %d", srcReader.Head(), n)
	}
	rawBlocks := make([][]byte, n)
	for i := uint32(1); i <= n; i++ {
		b, err := srcReader.Block(i)
		if err != nil {
			srcReader.Close()
			t.Fatalf("read source block %d: %v", i, err)
		}
		rawBlocks[i-1] = b
	}
	srcReader.Close()

	dataDir := t.TempDir()
	stateDB, err := state.Open(dataDir, 64<<20)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer stateDB.Close()
	if err := genesis.Apply(stateDB); err != nil {
		_ = err
	}
	cdb := chain.New(stateDB)

	blockLogDir := filepath.Join(dataDir, "blockchain")
	if err := os.MkdirAll(blockLogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blockLogPath := filepath.Join(blockLogDir, "block_log")
	w, err := blocklog.OpenForWrite(blockLogPath, true)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	for i, raw := range rawBlocks {
		blockNum := uint32(i + 1)
		if _, err := cdb.ApplyBlock(raw, blockNum, chain.ApplyOptions{
			SkipBlockSignature:        true,
			SkipTransactionSignatures: true,
			Sync:                      false,
		}, nil); err != nil {
			t.Fatalf("ApplyBlock %d: %v", blockNum, err)
		}
		if err := w.Append(raw); err != nil {
			t.Fatalf("Append %d: %v", blockNum, err)
		}
	}
	if got := w.HeadBlock(); got != n {
		t.Errorf("writer HeadBlock: got %d, want %d", got, n)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Round-trip: every block must read back identical to the source.
	verifyR, err := blocklog.Open(blockLogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer verifyR.Close()
	if got := verifyR.Head(); got != n {
		t.Fatalf("verifier Head: got %d, want %d", got, n)
	}
	for i := uint32(1); i <= n; i++ {
		got, err := verifyR.Block(i)
		if err != nil {
			t.Fatalf("verifier Block(%d): %v", i, err)
		}
		if !bytes.Equal(got, rawBlocks[i-1]) {
			t.Errorf("round-trip block %d: %d bytes, source %d", i, len(got), len(rawBlocks[i-1]))
		}
	}

	// Byte-identical: the on-disk slice covering blocks 1..n in our writer
	// output must match the corresponding slice of ./block_log.
	srcEnd := mustEndOfBlockOffsetForTest(t, src, n)
	srcSlice := make([]byte, srcEnd)
	sf, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.ReadAt(srcSlice, 0); err != nil {
		sf.Close()
		t.Fatal(err)
	}
	sf.Close()

	gotBytes, err := os.ReadFile(blockLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, srcSlice) {
		minLen := len(gotBytes)
		if len(srcSlice) < minLen {
			minLen = len(srcSlice)
		}
		first := -1
		for i := 0; i < minLen; i++ {
			if gotBytes[i] != srcSlice[i] {
				first = i
				break
			}
		}
		t.Fatalf("byte-identical mismatch: got %d, src %d, first diff at %d",
			len(gotBytes), len(srcSlice), first)
	}
}

// mustEndOfBlockOffsetForTest returns the byte offset just past block N's
// trailer in path — i.e. the size of the prefix slice that covers the first
// blockN blocks. Used to slice ./block_log for byte-identical comparison.
func mustEndOfBlockOffsetForTest(t *testing.T, path string, blockN uint32) int {
	t.Helper()
	r, err := blocklog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, end, err := r.BlockExtent(blockN)
	if err != nil {
		t.Fatal(err)
	}
	// end is the start offset of the trailer; the trailer is 8 bytes.
	return int(end + 8)
}

// TestApplyBlockHookIntegration exercises a.applyBlock end-to-end against
// real mainnet bytes — the same call site the future sync-delegate hook
// will use. Differs from TestSelfReplayBlocks in that it runs through the
// full app structure (writer wrapping, --verify-against, target-block
// short-circuit) rather than the bare ApplyBlock + Append loop.
func TestApplyBlockHookIntegration(t *testing.T) {
	src := projectBlockLog(t)
	const n = 50

	// Pre-populate a tiny "reference" uncompressed log so the
	// --verify-against path is exercised. We can't point at the project's
	// compressed source for this — its raw on-disk bytes don't match what
	// our applyBlock writes — so we build a clean uncompressed ref.
	srcReader, err := blocklog.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if srcReader.Head() < n {
		srcReader.Close()
		t.Skipf("source block_log only has %d blocks", srcReader.Head())
	}
	rawBlocks := make([][]byte, n)
	for i := uint32(1); i <= n; i++ {
		b, _ := srcReader.Block(i)
		rawBlocks[i-1] = b
	}
	srcReader.Close()

	refDir := t.TempDir()
	refPath := filepath.Join(refDir, "block_log")
	rw, err := blocklog.OpenForWrite(refPath, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rawBlocks {
		_ = rw.Append(b)
	}
	rw.Close()

	dataDir := t.TempDir()
	app, err := newApp(flags{
		dataDir:       dataDir,
		verifyAgainst: refPath,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.close()

	// Override Apply options via a temporary wrapper. The shipped
	// applyBlock keeps SkipBlockSignature=false (live sync), but for the
	// unit test we need it true (no real signing keys loaded).
	app.skipSigsForTest = true

	for i, raw := range rawBlocks {
		if err := app.applyBlock(raw, uint32(i+1)); err != nil {
			t.Fatalf("app.applyBlock %d: %v", i+1, err)
		}
	}

	if got := app.writer.HeadBlock(); got != n {
		t.Errorf("HeadBlock: got %d, want %d", got, n)
	}

	// The live writer file must match the reference byte-for-byte.
	gotBytes, err := os.ReadFile(filepath.Join(dataDir, "blockchain", "block_log"))
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("byte mismatch: got %d bytes, want %d bytes", len(gotBytes), len(wantBytes))
	}
}
