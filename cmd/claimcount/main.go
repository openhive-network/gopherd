package main

// claimcount tallies claim_account / create_claimed_account ops for one
// creator over block ranges, in parallel. Ground truth for
// account.pending_claimed_accounts (= claims - creates since HF20), used to
// detect residues left by the historical uint16 wrap.
//
// Usage: claimcount <block_log> <creator> <boundary>... (ascending block numbers)

import (
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

type tally struct{ claims, creates int64 }

func scanRange(path string, from, to uint32, creator string) tally {
	r, err := blocklog.Open(path)
	if err != nil {
		panic(err)
	}
	defer r.Close()
	var t tally
	var raw, out []byte
	for n := from; n <= to; n++ {
		blk, newRaw, err := r.BlockReuse(n, raw, out)
		raw = newRaw
		if err != nil {
			panic(fmt.Sprintf("block %d: %v", n, err))
		}
		out = blk
		_, hdrLen, err := block.UnmarshalSignedHeader(blk)
		if err != nil {
			panic(fmt.Sprintf("block %d decode: %v", n, err))
		}
		rd := fcraw.NewReader(blk[hdrLen:])
		txCount, _ := rd.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				break
			}
			for _, o := range txRes.SignedTx.Operations {
				switch o.Tag {
				case op.TagClaimAccount:
					if string(o.AsClaimAccount().Creator) == creator {
						t.claims++
					}
				case op.TagCreateClaimedAccount:
					if string(o.AsCreateClaimedAccount().Creator) == creator {
						t.creates++
					}
				}
			}
		}
	}
	return t
}

func main() {
	if os.Args[1] == "-repair" {
		var end uint64
		fmt.Sscanf(os.Args[3], "%d", &end)
		repairMain(os.Args[2], uint32(end), os.Args[4])
		return
	}
	path, creator := os.Args[1], os.Args[2]
	var bounds []uint32
	for _, a := range os.Args[3:] {
		v, err := strconv.ParseUint(a, 10, 32)
		if err != nil {
			panic(err)
		}
		bounds = append(bounds, uint32(v))
	}
	// Each [bounds[i]+1, bounds[i+1]] segment is split into worker shards.
	const workers = 10
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i]+1, bounds[i+1]
		span := hi - lo + 1
		shard := span / workers
		results := make([]tally, workers)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			from := lo + uint32(w)*shard
			to := from + shard - 1
			if w == workers-1 {
				to = hi
			}
			wg.Add(1)
			go func(w int, from, to uint32) {
				defer wg.Done()
				results[w] = scanRange(path, from, to, creator)
			}(w, from, to)
		}
		wg.Wait()
		var t tally
		for _, r := range results {
			t.claims += r.claims
			t.creates += r.creates
		}
		fmt.Printf("range (%d, %d]: claims=%d creates=%d net=%+d\n", bounds[i], hi, t.claims, t.creates, t.claims-t.creates)
	}
}
