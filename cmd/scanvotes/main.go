package main

import (
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Println("usage: scanvotes <block_log> <voter> <start_block> <end_block>")
		os.Exit(2)
	}
	r, _ := blocklog.Open(os.Args[1])
	defer r.Close()
	voter := os.Args[2]
	startBlock, _ := strconv.Atoi(os.Args[3])
	endBlock, _ := strconv.Atoi(os.Args[4])
	for n := uint32(startBlock); n <= uint32(endBlock); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		_, hdrLen, _ := block.UnmarshalSignedHeader(raw)
		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rd.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				continue
			}
			for _, o := range txRes.SignedTx.Operations {
				if v, ok := o.(op.Vote); ok && string(v.Voter) == voter {
					fmt.Printf("block %d vote: voter=%s author=%s permlink=%s weight=%d\n",
						n, v.Voter, v.Author, v.Permlink, v.Weight)
				}
			}
		}
	}
}
