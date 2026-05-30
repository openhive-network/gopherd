package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/protocol/block"
)

const usage = `ghive-tools — utilities for the ghive Go rewrite of hived

Usage:
  ghive-tools <command> [args]

Commands:
  blockid <block_log_path> <block_num>   compute the block ID for a given block
                                         (P0 milestone — must match hived)
  inspect <block_log_path>               summarize a block_log file
  validate-mainnet                       fetch real mainnet blocks via JSON-RPC
                                         and validate computed block IDs match
                                         (substitute for P2P sync until P2 lands)
  validate-blocklog <block_log_path>     parse every block + every transaction
                                         and validate computed merkle roots
                                         match each block header
  replay <block_log_path>                apply every block through the
                                         evaluator pipeline (state mutation)
  state-dump --data-dir=PATH             dump state objects as JSONL for
                                         comparison vs hived state_snapshot
  state-jsonl --data-dir=PATH            deterministic JSONL dump (sorted, stable
                                         field order) for line-diffable parity
  benchmark <block_log>                  cold-start replay benchmark — wall-clock,
                                         peak RSS, ops/sec
  snapshot-diff <a.jsonl> <b.jsonl>      compare two state-jsonl outputs line by
                                         line; report missing/extra/changed objects
  version                                print version
  help                                   show this message
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "blockid":
		os.Exit(cmdBlockID(os.Args[2:]))
	case "inspect":
		os.Exit(cmdInspect(os.Args[2:]))
	case "validate-mainnet":
		os.Exit(cmdValidateMainnet(os.Args[2:]))
	case "validate-blocklog":
		os.Exit(cmdValidateBlocklog(os.Args[2:]))
	case "debug-block":
		os.Exit(cmdDebugBlock(os.Args[2:]))
	case "replay":
		os.Exit(cmdReplay(os.Args[2:]))
	case "state-dump":
		os.Exit(cmdStateDump(os.Args[2:]))
	case "state-jsonl":
		os.Exit(cmdStateJSONL(os.Args[2:]))
	case "benchmark":
		os.Exit(cmdBenchmark(os.Args[2:]))
	case "snapshot-diff":
		os.Exit(cmdSnapshotDiff(os.Args[2:]))
	case "validate-state":
		os.Exit(cmdValidateState(os.Args[2:]))
	case "dumpvotes":
		runDumpVotes()
	case "witness-diff":
		os.Exit(cmdWitnessDiff(os.Args[2:]))
	case "version":
		fmt.Println("ghive-tools 0.0.1 (P0)")
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "ghive-tools: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// cmdBlockID prints the computed block ID for a single block, or — when
// invoked with --verify on a range — walks a stretch of blocks confirming
// chain self-consistency (each block's `previous` equals the prior block's
// computed ID).
func cmdBlockID(args []string) int {
	fs := flag.NewFlagSet("blockid", flag.ContinueOnError)
	verify := fs.Bool("verify", false, "verify chain consistency from block_num to block_num+count-1")
	count := fs.Uint("count", 1, "with --verify, number of blocks to walk")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ghive-tools blockid [--verify --count=N] <block_log_path> <block_num>")
		return 2
	}
	logPath := rest[0]
	startNum, err := strconv.ParseUint(rest[1], 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "blockid: invalid block_num %q: %v\n", rest[1], err)
		return 2
	}

	r, err := blocklog.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "blockid: %v\n", err)
		return 1
	}
	defer r.Close()

	if *verify {
		return runVerify(r, uint32(startNum), uint32(*count))
	}
	id, err := computeBlockID(r, uint32(startNum))
	if err != nil {
		fmt.Fprintf(os.Stderr, "blockid: %v\n", err)
		return 1
	}
	fmt.Printf("%08x  %d  %s\n", id.Num(), id.Num(), hex.EncodeToString(id[:]))
	return 0
}

func runVerify(r *blocklog.Reader, start, count uint32) int {
	if start < 1 {
		fmt.Fprintln(os.Stderr, "blockid --verify: block num must be >= 1")
		return 2
	}
	if start+count-1 > r.Head() {
		count = r.Head() - start + 1
	}
	var prevID block.ID
	if start > 1 {
		// We need block (start-1) to seed the chain check.
		seed, err := computeBlockID(r, start-1)
		if err != nil {
			fmt.Fprintf(os.Stderr, "blockid --verify: seed block %d: %v\n", start-1, err)
			return 1
		}
		prevID = seed
	}
	for i := uint32(0); i < count; i++ {
		num := start + i
		raw, err := r.Block(num)
		if err != nil {
			fmt.Fprintf(os.Stderr, "blockid --verify: read block %d: %v\n", num, err)
			return 1
		}
		hdr, signedLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "blockid --verify: parse block %d: %v\n", num, err)
			return 1
		}
		if num > 1 && hdr.Previous != prevID {
			fmt.Fprintf(os.Stderr,
				"blockid --verify: chain break at block %d:\n  block.previous = %s\n  computed prior = %s\n",
				num, hex.EncodeToString(hdr.Previous[:]), hex.EncodeToString(prevID[:]))
			return 1
		}
		id := block.ConstructID(raw[:signedLen], num)
		if id.Num() != num {
			fmt.Fprintf(os.Stderr, "blockid --verify: block %d: ID embeds num %d\n", num, id.Num())
			return 1
		}
		prevID = id
	}
	fmt.Printf("verified %d blocks (%d..%d), chain self-consistent\n", count, start, start+count-1)
	return 0
}

func computeBlockID(r *blocklog.Reader, num uint32) (block.ID, error) {
	raw, err := r.Block(num)
	if err != nil {
		return block.ID{}, err
	}
	_, signedLen, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		return block.ID{}, fmt.Errorf("parse block %d: %w", num, err)
	}
	return block.ConstructID(raw[:signedLen], num), nil
}

func cmdInspect(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: ghive-tools inspect <block_log_path>")
		return 2
	}
	r, err := blocklog.Open(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect: %v\n", err)
		return 1
	}
	defer r.Close()
	fmt.Printf("path:        %s\n", args[0])
	fmt.Printf("head_block:  %d\n", r.Head())
	if r.Head() >= 1 {
		id, err := computeBlockID(r, 1)
		if err == nil {
			fmt.Printf("block_1_id:  %s\n", hex.EncodeToString(id[:]))
		}
	}
	if r.Head() >= 1 {
		id, err := computeBlockID(r, r.Head())
		if err == nil {
			fmt.Printf("head_id:     %s\n", hex.EncodeToString(id[:]))
		}
	}
	return 0
}
