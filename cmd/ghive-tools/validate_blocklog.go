package main

// validate-blocklog walks every block in a block_log, parses every
// transaction, computes the block's transaction merkle root from the
// per-tx merkle digests, and compares it against the block header's
// transaction_merkle_root field. This is the strongest end-to-end
// validation possible without state (no evaluators / no signature
// verification).

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/tx"
)

func cmdValidateBlocklog(args []string) int {
	fs := flag.NewFlagSet("validate-blocklog", flag.ContinueOnError)
	startBlock := fs.Uint("start", 1, "first block to validate")
	count := fs.Uint("count", 0, "number of blocks to validate (0 = all up to head)")
	progressEvery := fs.Uint("progress-every", 100_000, "print progress every N blocks")
	maxFailures := fs.Uint("max-fails", 5, "abort after this many parse/merkle failures")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: ghive-tools validate-blocklog [flags] <block_log_path>")
		return 2
	}
	logPath := rest[0]

	r, err := blocklog.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate-blocklog: %v\n", err)
		return 1
	}
	defer r.Close()

	end := uint32(*startBlock) + uint32(*count) - 1
	if *count == 0 || end > r.Head() {
		end = r.Head()
	}
	fmt.Printf("validate-blocklog: %d..%d (head=%d)\n", *startBlock, end, r.Head())

	var (
		examined, txTotal, parseFails, merkleFails uint64
		opTagHisto                                 [256]uint64
		start                                      = time.Now()
	)

	for n := uint32(*startBlock); n <= end; n++ {
		raw, err := r.Block(n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d read: %v\n", n, err)
			parseFails++
			if parseFails >= uint64(*maxFailures) {
				return 1
			}
			continue
		}

		hdr, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d header: %v\n", n, err)
			parseFails++
			if parseFails >= uint64(*maxFailures) {
				return 1
			}
			continue
		}

		// Walk the transactions vector following the signed header.
		txReader := fcraw.NewReader(raw[hdrLen:])
		txCount, err := txReader.ReadVarUint()
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d tx count: %v\n", n, err)
			parseFails++
			if parseFails >= uint64(*maxFailures) {
				return 1
			}
			continue
		}

		digests := make([]tx.Digest, 0, txCount)
		var failedThisBlock bool
		for i := uint32(0); i < txCount; i++ {
			res, err := tx.ReadSignedTransaction(txReader)
			if err != nil {
				fmt.Fprintf(os.Stderr, "block %d tx %d: %v\n", n, i, err)
				failedThisBlock = true
				break
			}
			for _, op := range res.SignedTx.Operations {
				opTagHisto[op.OpTag()]++
			}
			signedRange := txReader.SourceRange(res.UnsignedStart, res.SignedEnd)
			digests = append(digests, tx.MerkleDigestFromBytes(signedRange))
			txTotal++
		}
		if failedThisBlock {
			parseFails++
			if parseFails >= uint64(*maxFailures) {
				return 1
			}
			continue
		}

		gotRoot := tx.MerkleRoot(digests)
		if gotRoot != hdr.TransactionMerkleRoot {
			merkleFails++
			fmt.Fprintf(os.Stderr,
				"block %d MERKLE MISMATCH:\n  computed: %s\n  expected: %s\n",
				n, hex.EncodeToString(gotRoot[:]), hex.EncodeToString(hdr.TransactionMerkleRoot[:]))
			if merkleFails >= uint64(*maxFailures) {
				return 1
			}
			continue
		}

		examined++
		if uint64(n)%uint64(*progressEvery) == 0 {
			elapsed := time.Since(start)
			rate := float64(examined) / elapsed.Seconds()
			fmt.Printf("  block %d ok — %.0f blocks/s, %d txs total\n", n, rate, txTotal)
		}
	}

	dur := time.Since(start)
	fmt.Println()
	fmt.Println("--- validate-blocklog summary ---")
	fmt.Printf("blocks examined: %d\n", examined)
	fmt.Printf("transactions:    %d\n", txTotal)
	fmt.Printf("parse fails:     %d\n", parseFails)
	fmt.Printf("merkle fails:    %d\n", merkleFails)
	fmt.Printf("elapsed:         %s\n", dur.Round(time.Millisecond))
	if examined > 0 {
		fmt.Printf("rate:            %.0f blocks/s\n", float64(examined)/dur.Seconds())
	}
	fmt.Println()
	fmt.Println("--- op tag histogram (top 20) ---")
	type histEntry struct {
		tag   uint8
		count uint64
	}
	entries := make([]histEntry, 0, 256)
	for i, c := range opTagHisto {
		if c > 0 {
			entries = append(entries, histEntry{tag: uint8(i), count: c})
		}
	}
	// Sort by count desc — small list, simple insertion sort is fine.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].count > entries[j-1].count; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	if len(entries) > 20 {
		entries = entries[:20]
	}
	for _, e := range entries {
		fmt.Printf("  %3d %-32s %d\n", e.tag, tagName(e.tag), e.count)
	}

	if parseFails == 0 && merkleFails == 0 {
		fmt.Println()
		fmt.Println("status: PASS — every transaction parsed and every merkle root matched.")
		return 0
	}
	return 1
}

// tagName looks up an op tag's friendly name for histogram display.
func tagName(tag uint8) string {
	// Minimal duplication: we could import op.Tag.Name but that adds a dep
	// we don't otherwise need here.
	names := []string{
		"vote", "comment", "transfer", "transfer_to_vesting", "withdraw_vesting",
		"limit_order_create", "limit_order_cancel", "feed_publish", "convert",
		"account_create", "account_update", "witness_update", "account_witness_vote",
		"account_witness_proxy", "pow", "custom", "witness_block_approve",
		"delete_comment", "custom_json", "comment_options",
		"set_withdraw_vesting_route", "limit_order_create2", "claim_account",
		"create_claimed_account", "request_account_recovery", "recover_account",
		"change_recovery_account", "escrow_transfer", "escrow_dispute",
		"escrow_release", "pow2", "escrow_approve", "transfer_to_savings",
		"transfer_from_savings", "cancel_transfer_from_savings", "custom_binary",
		"decline_voting_rights", "reset_account", "set_reset_account",
		"claim_reward_balance", "delegate_vesting_shares",
		"account_create_with_delegation", "witness_set_properties",
		"account_update2", "create_proposal", "update_proposal_votes",
		"remove_proposal", "update_proposal", "collateralized_convert",
		"recurrent_transfer",
	}
	if int(tag) < len(names) {
		return names[tag]
	}
	return fmt.Sprintf("tag%d", tag)
}
