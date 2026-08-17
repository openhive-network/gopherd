package main

// validate-mainnet fetches blocks from a public Hive JSON-RPC node and
// verifies that ghive's block_id computation matches the IDs the node
// reports. This is the closest substitute for "connect to P2P, sync the
// chain, validate" we can produce before the P2 P2P stack lands —
// it pulls real mainnet blocks over HTTP and runs them through the same
// MarshalSignedHeader → ConstructID pipeline that replay/validation will
// use.
//
// Network access required.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"gopherd/internal/protocol/block"
)

func cmdValidateMainnet(args []string) int {
	fs := flag.NewFlagSet("validate-mainnet", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "https://api.hive.blog", "Hive JSON-RPC endpoint")
	startBlock := fs.Uint("start", 1, "first block to validate (inclusive)")
	count := fs.Uint("count", 10000, "number of blocks to validate")
	batch := fs.Uint("batch", 100, "blocks per RPC request (max 1000)")
	progressEvery := fs.Uint("progress-every", 500, "print progress every N blocks")
	requestTimeout := fs.Duration("timeout", 30*time.Second, "per-request HTTP timeout")
	maxFails := fs.Uint("max-fails", 5, "abort after this many block-id mismatches")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *batch == 0 || *batch > 1000 {
		fmt.Fprintln(os.Stderr, "validate-mainnet: --batch must be in [1, 1000]")
		return 2
	}

	client := &hiveAPIClient{
		Endpoint: *endpoint,
		HTTP:     &http.Client{Timeout: *requestTimeout},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats := validatorStats{
		start:   time.Now(),
		startBN: uint32(*startBlock),
	}
	defer stats.report(os.Stdout)

	cur := uint32(*startBlock)
	end := cur + uint32(*count)
	fmt.Printf("validate-mainnet: endpoint=%s start=%d count=%d batch=%d\n",
		*endpoint, cur, *count, *batch)

	for cur < end {
		thisBatch := uint32(*batch)
		if cur+thisBatch > end {
			thisBatch = end - cur
		}
		blocks, err := client.GetBlockRange(ctx, cur, thisBatch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "validate-mainnet: fetch %d..%d: %v\n", cur, cur+thisBatch-1, err)
			return 1
		}
		if uint32(len(blocks)) != thisBatch {
			fmt.Fprintf(os.Stderr, "validate-mainnet: expected %d blocks, got %d (head reached?)\n",
				thisBatch, len(blocks))
			if len(blocks) == 0 {
				return 1
			}
		}

		for i, b := range blocks {
			blockNum := cur + uint32(i)
			id, mismatch, err := validateOne(blockNum, &b)
			stats.examined++
			if err != nil {
				stats.parseFails++
				fmt.Fprintf(os.Stderr, "block %d parse: %v\n", blockNum, err)
				if stats.parseFails >= uint32(*maxFails) {
					return 1
				}
				continue
			}
			if mismatch {
				stats.mismatches++
				fmt.Fprintf(os.Stderr,
					"block %d MISMATCH:\n  computed: %s\n  expected: %s\n",
					blockNum, hex.EncodeToString(id[:]), b.BlockID)
				if stats.mismatches >= uint32(*maxFails) {
					return 1
				}
				continue
			}
			stats.matched++
			if blockNum%uint32(*progressEvery) == 0 {
				elapsed := time.Since(stats.start)
				rate := float64(stats.examined) / elapsed.Seconds()
				fmt.Printf("  ...block %d ok (%.0f blocks/s, %d ok / %d total)\n",
					blockNum, rate, stats.matched, stats.examined)
			}
		}
		cur += uint32(len(blocks))
	}
	return 0
}

// validateOne re-marshals b into FC binary and compares the computed block ID
// to the API-reported one. Returns (computedID, mismatch?, error).
func validateOne(blockNum uint32, b *jsonBlock) (block.ID, bool, error) {
	hdr, err := b.toSignedHeader()
	if err != nil {
		return block.ID{}, false, err
	}
	wire := block.MarshalSignedHeader(nil, &hdr)
	got := block.ConstructID(wire, blockNum)

	want, err := decodeBlockIDHex(b.BlockID)
	if err != nil {
		return got, false, fmt.Errorf("invalid expected block_id: %w", err)
	}
	return got, got != want, nil
}

// --- HTTP / JSON-RPC plumbing ---------------------------------------------

type hiveAPIClient struct {
	Endpoint string
	HTTP     *http.Client
	id       int
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *hiveAPIClient) GetBlockRange(ctx context.Context, start, count uint32) ([]jsonBlock, error) {
	c.id++
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.id,
		Method:  "block_api.get_block_range",
		Params: map[string]uint32{
			"starting_block_num": start,
			"count":              count,
		},
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	var env rpcEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", env.Error.Code, env.Error.Message)
	}
	var result struct {
		Blocks []jsonBlock `json:"blocks"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	return result.Blocks, nil
}

// --- JSON block shape -----------------------------------------------------

// jsonBlock matches the relevant subset of the block JSON returned by
// block_api.get_block_range. We only deserialize fields that influence the
// block ID (header + signature); transactions are intentionally skipped.
type jsonBlock struct {
	Previous              string          `json:"previous"`
	Timestamp             string          `json:"timestamp"`
	Witness               string          `json:"witness"`
	TransactionMerkleRoot string          `json:"transaction_merkle_root"`
	Extensions            json.RawMessage `json:"extensions"`
	WitnessSignature      string          `json:"witness_signature"`
	BlockID               string          `json:"block_id"`
}

// toSignedHeader converts the JSON form into block.SignedHeader. Fails if
// extensions is non-empty (we don't yet implement any header extensions).
func (b *jsonBlock) toSignedHeader() (block.SignedHeader, error) {
	var hdr block.SignedHeader
	prev, err := decodeBlockIDHex(b.Previous)
	if err != nil {
		return hdr, fmt.Errorf("previous: %w", err)
	}
	hdr.Previous = prev

	ts, err := time.Parse("2006-01-02T15:04:05", b.Timestamp)
	if err != nil {
		return hdr, fmt.Errorf("timestamp %q: %w", b.Timestamp, err)
	}
	hdr.Timestamp = uint32(ts.UTC().Unix())

	hdr.Witness = b.Witness

	merkle, err := decodeFixed20(b.TransactionMerkleRoot)
	if err != nil {
		return hdr, fmt.Errorf("transaction_merkle_root: %w", err)
	}
	hdr.TransactionMerkleRoot = merkle

	// Extensions: must be empty array. Anything else aborts loudly so we
	// notice the day a block carries a real extension and we need to teach
	// the codec how to handle it.
	var extArr []json.RawMessage
	if len(b.Extensions) > 0 {
		if err := json.Unmarshal(b.Extensions, &extArr); err != nil {
			return hdr, fmt.Errorf("extensions: %w", err)
		}
	}
	if len(extArr) != 0 {
		return hdr, fmt.Errorf("non-empty extensions not yet supported (%d entries)", len(extArr))
	}
	hdr.Extensions = []byte{0x00} // varint(0)

	sig, err := hex.DecodeString(b.WitnessSignature)
	if err != nil {
		return hdr, fmt.Errorf("witness_signature: %w", err)
	}
	if len(sig) != block.SignatureSize {
		return hdr, fmt.Errorf("witness_signature must be %d bytes, got %d", block.SignatureSize, len(sig))
	}
	copy(hdr.WitnessSignature[:], sig)
	return hdr, nil
}

func decodeBlockIDHex(s string) (block.ID, error) {
	var id block.ID
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	if len(b) != block.IDSize {
		return id, fmt.Errorf("expected %d hex bytes, got %d", block.IDSize, len(b))
	}
	copy(id[:], b)
	return id, nil
}

func decodeFixed20(s string) ([20]byte, error) {
	var out [20]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 20 {
		return out, fmt.Errorf("expected 20 hex bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// --- stats ----------------------------------------------------------------

type validatorStats struct {
	start                         time.Time
	startBN                       uint32
	examined, matched, mismatches uint32
	parseFails                    uint32
}

func (s *validatorStats) report(w io.Writer) {
	dur := time.Since(s.start)
	rate := float64(s.examined) / dur.Seconds()
	fmt.Fprintf(w, "\n--- validate-mainnet summary ---\n")
	fmt.Fprintf(w, "examined:    %d\n", s.examined)
	fmt.Fprintf(w, "matched:     %d\n", s.matched)
	fmt.Fprintf(w, "mismatches:  %d\n", s.mismatches)
	fmt.Fprintf(w, "parse fails: %d\n", s.parseFails)
	fmt.Fprintf(w, "elapsed:     %s\n", dur.Round(time.Millisecond))
	fmt.Fprintf(w, "rate:        %.0f blocks/s\n", rate)
	if s.examined > 0 && s.mismatches == 0 && s.parseFails == 0 {
		fmt.Fprintf(w, "status:      PASS — all %d blocks match hived\n", s.examined)
	} else {
		fmt.Fprintf(w, "status:      FAIL\n")
	}
}
