package main

// govexpscan reconstructs every account's LAST governance action time by
// scanning the block_log directly — the three ops whose evaluators call
// update_governance_vote_expiration_ts in C++ (account_witness_vote,
// account_witness_proxy, update_proposal_votes). Blocks only contain
// successfully applied transactions, so every occurrence updated the stamp.
//
// Output JSON: {"account": [lastBlock, lastBlockTimestamp], ...}
//
// IMPORTANT: to repair a snapshot taken at block N, run with to_block=N —
// the stamp must reflect the last action AT OR BELOW N. A scan past N
// collapses later actions into the map and mis-zeros accounts that acted
// both before and after N (a to=65.83M scan against a 59.03M snapshot
// stray-zeroed 6,422 accounts).
//
// Used to repair GovernanceVoteExpiration stamps in snapshots produced by
// the pre-300ba72 code (which froze the HF25 spread at the FIRST pre-HF25
// action instead of recomputing on every action like C++).
//
// Usage: govexpscan <block_log> <to_block> <out.json>

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

type last struct {
	Block uint32
	Time  uint32
}

func scanRange(path string, from, to uint32) map[string]last {
	r, err := blocklog.Open(path)
	if err != nil {
		panic(err)
	}
	defer r.Close()
	out := make(map[string]last, 1<<16)
	note := func(name string, blk, ts uint32) {
		if name == "" {
			return
		}
		if cur, ok := out[name]; !ok || blk > cur.Block {
			out[name] = last{Block: blk, Time: ts}
		}
	}
	// C++ evaluators stamp with head_block_time() which during tx
	// application is the PREVIOUS block's timestamp (dgpo.time advances
	// in update_global_dynamic_data AFTER the tx loop). Using the action
	// block's own timestamp made every stamp 3s late → every expiry fired
	// one block after hived's expired_account_notification vop.
	prevTs := func(n uint32) uint32 {
		if n <= 1 {
			return 0
		}
		praw, err := r.Block(n - 1)
		if err != nil {
			return 0
		}
		phdr, _, err := block.UnmarshalSignedHeader(praw)
		if err != nil {
			return 0
		}
		return phdr.Timestamp
	}
	for n := from; n <= to; n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		_, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			continue
		}

		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rd.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				break
			}
			for _, o := range txRes.SignedTx.Operations {
				switch v := o.Operation().(type) {
				case op.AccountWitnessVote:
					note(string(v.Account), n, prevTs(n))
				case op.AccountWitnessProxy:
					note(string(v.Account), n, prevTs(n))
				case op.UpdateProposalVotes:
					note(string(v.Voter), n, prevTs(n))
				}
			}
		}
	}
	return out
}

func cmdGovexpScan(args []string) int {
	os.Args = append([]string{"gopherd-govexp-scan"}, args...)

	path := os.Args[1]
	toN, err := strconv.ParseUint(os.Args[2], 10, 32)
	if err != nil {
		panic(err)
	}
	outPath := os.Args[3]
	to := uint32(toN)

	const workers = 12
	shard := to / workers
	results := make([]map[string]last, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		from := 1 + uint32(w)*shard
		hi := from + shard - 1
		if w == workers-1 {
			hi = to
		}
		wg.Add(1)
		go func(w int, from, hi uint32) {
			defer wg.Done()
			results[w] = scanRange(path, from, hi)
			fmt.Fprintf(os.Stderr, "worker %d done [%d..%d] accounts=%d\n", w, from, hi, len(results[w]))
		}(w, from, hi)
	}
	wg.Wait()

	merged := results[0]
	for _, m := range results[1:] {
		for name, l := range m {
			if cur, ok := merged[name]; !ok || l.Block > cur.Block {
				merged[name] = l
			}
		}
	}
	out := make(map[string][2]uint32, len(merged))
	for name, l := range merged {
		out[name] = [2]uint32{l.Block, l.Time}
	}
	f, err := os.Create(outPath)
	if err != nil {
		panic(err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(out); err != nil {
		panic(err)
	}
	f.Close()
	fmt.Printf("accounts with governance actions through %d: %d -> %s\n", to, len(out), outPath)
	return 0
}
