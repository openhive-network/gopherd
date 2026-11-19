// Package fuzzseed regenerates the checked-in fuzz seed corpora from a
// mainnet block_log. It is not part of the normal test run:
//
//	GOPHERD_FUZZSEED_BLOCK_LOG=/path/to/block_log go test ./internal/fuzzseed
//
// It samples blocks across the whole log and writes, in go-fuzz corpus format,
// one encoded operation per operation type it meets, one signed transaction
// per leading operation type, and one signed block header per million blocks
// (plus every header that carries extensions, up to a few per era). The fuzz
// targets then start from real encodings instead of random bytes, which gets
// them past the outer framing in seconds rather than hours.
package fuzzseed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gopherd/internal/blocklog"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
)

// sampleStep spreads ~62k samples over a 62M-block log; a prime keeps the
// samples from locking onto any periodic structure (witness rounds, days).
const sampleStep = 997

func TestGenerateSeeds(t *testing.T) {
	path := os.Getenv("GOPHERD_FUZZSEED_BLOCK_LOG")
	if path == "" {
		t.Skip("set GOPHERD_FUZZSEED_BLOCK_LOG to regenerate the fuzz seed corpora")
	}
	step := uint32(sampleStep)
	if s := os.Getenv("GOPHERD_FUZZSEED_STEP"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			t.Fatalf("GOPHERD_FUZZSEED_STEP=%q", s)
		}
		step = uint32(n)
	}
	r, err := blocklog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	root := filepath.Join("..", "..")
	ops := map[op.Tag][]byte{}
	txs := map[op.Tag][]byte{}
	var headers [][]byte
	extHeaders := map[uint32]int{} // per million blocks
	blocks := map[uint32][]byte{}  // one small block with transactions per 5M

	for n := uint32(1); n <= r.Head(); n += step {
		raw, err := r.Block(n)
		if err != nil {
			t.Fatalf("block %d: %v", n, err)
		}
		hdr, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			t.Fatalf("block %d header: %v", n, err)
		}
		era := n / 1_000_000
		if n%1_000_000 < step {
			headers = append(headers, raw[:hdrLen])
		}
		if len(hdr.Extensions) > 0 && extHeaders[era] < 2 {
			extHeaders[era]++
			headers = append(headers, raw[:hdrLen])
		}

		rd := fcraw.NewReader(raw[hdrLen:])
		count, err := tx.ReadBlockTxCount(rd)
		if err != nil {
			t.Fatalf("block %d tx count: %v", n, err)
		}
		if count > 0 && len(raw) < 4096 {
			if _, ok := blocks[n/5_000_000]; !ok {
				blocks[n/5_000_000] = raw
			}
		}
		for i := uint32(0); i < count; i++ {
			start := hdrLen + rd.Pos()
			res, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				t.Fatalf("block %d tx %d: %v", n, i, err)
			}
			end := hdrLen + rd.Pos()
			st := res.SignedTx
			if len(st.Operations) > 0 {
				lead := st.Operations[0].Tag
				if prev, ok := txs[lead]; !ok || end-start < len(prev) {
					txs[lead] = append([]byte(nil), raw[start:end]...)
				}
			}
			for _, o := range st.Operations {
				if _, ok := ops[o.Tag]; !ok {
					ops[o.Tag] = op.AppendOp(nil, o)
				}
			}
		}
	}

	write := func(dir, target string, inputs [][]byte) {
		d := filepath.Join(root, dir, "testdata", "fuzz", target)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, in := range inputs {
			sum := sha256.Sum256(in)
			body := fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n", in)
			if err := os.WriteFile(filepath.Join(d, hex.EncodeToString(sum[:8])), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("%s/%s: %d seeds", dir, target, len(inputs))
	}
	values := func(m map[op.Tag][]byte) [][]byte {
		out := make([][]byte, 0, len(m))
		for _, v := range m {
			out = append(out, v)
		}
		return out
	}
	var blockList [][]byte
	for _, b := range blocks {
		blockList = append(blockList, b)
	}
	write("internal/protocol/op", "FuzzReadOp", values(ops))
	write("internal/protocol/tx", "FuzzReadSignedTransaction", values(txs))
	write("internal/protocol/block", "FuzzUnmarshalSignedHeader", headers)
	write("internal/chain", "FuzzBlockBody", blockList)
}
