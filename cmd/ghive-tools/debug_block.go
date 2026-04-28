package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/tx"
)

// debug-block <block_log_path> <block_num>: dumps detailed per-tx info
// for one block — tx_id, merkle digest, op tags, byte range. Useful for
// diagnosing merkle mismatches against API-reported tx_ids.
func cmdDebugBlock(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ghive-tools debug-block <block_log_path> <block_num>")
		return 2
	}
	num64, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug-block: bad block_num: %v\n", err)
		return 2
	}
	num := uint32(num64)

	r, err := blocklog.Open(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug-block: %v\n", err)
		return 1
	}
	defer r.Close()

	raw, err := r.Block(num)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug-block: %v\n", err)
		return 1
	}
	hdr, hdrLen, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug-block header: %v\n", err)
		return 1
	}
	fmt.Printf("block %d: total bytes=%d signed_header=%d\n", num, len(raw), hdrLen)
	fmt.Printf("  expected merkle_root: %s\n", hex.EncodeToString(hdr.TransactionMerkleRoot[:]))

	txReader := fcraw.NewReader(raw[hdrLen:])
	txCount, err := txReader.ReadVarUint()
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug-block: tx count: %v\n", err)
		return 1
	}
	fmt.Printf("  tx_count: %d\n", txCount)

	digests := make([]tx.Digest, 0, txCount)
	for i := uint32(0); i < txCount; i++ {
		res, err := tx.ReadSignedTransaction(txReader)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  tx %d parse: %v\n", i, err)
			return 1
		}
		unsigned := txReader.SourceRange(res.UnsignedStart, res.UnsignedEnd)
		signed := txReader.SourceRange(res.UnsignedStart, res.SignedEnd)
		txDigest := tx.DigestFromBytes(unsigned)
		txID := tx.IDFromDigest(txDigest)
		merkleDig := tx.MerkleDigestFromBytes(signed)
		digests = append(digests, merkleDig)

		fmt.Printf("  tx %d: bytes=%d signed_bytes=%d ops=%d sigs=%d tx_id=%s\n",
			i, len(unsigned), len(signed),
			len(res.SignedTx.Operations), len(res.SignedTx.Signatures),
			hex.EncodeToString(txID[:]))
		for j, op := range res.SignedTx.Operations {
			fmt.Printf("    op %d: %s\n", j, op.OpTag().Name())
		}
		fmt.Printf("    unsigned_first16: %x\n", unsigned[:min(16, len(unsigned))])
		fmt.Printf("    signed_last16:    %x\n", signed[max(0, len(signed)-16):])
		for j, sig := range res.SignedTx.Signatures {
			fmt.Printf("    sig %d: %s\n", j, hex.EncodeToString(sig[:]))
		}
	}

	gotMerkle := tx.MerkleRoot(digests)
	fmt.Printf("  computed merkle_root: %s\n", hex.EncodeToString(gotMerkle[:]))
	if gotMerkle == hdr.TransactionMerkleRoot {
		fmt.Println("  → MATCH")
	} else {
		fmt.Println("  → MISMATCH")
	}
	return 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
