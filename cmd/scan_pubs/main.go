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

// Usage: scan_pubs <block_log> <up_to_block>
// Lists all feed_publish ops by publisher, showing: count, last_block, last_rate.
func main() {
	r, err := blocklog.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer r.Close()
	endBlock, _ := strconv.Atoi(os.Args[2])
	type lastFeed struct {
		count    int
		lastBlk  uint32
		lastBase int64
		lastQ    int64
	}
	pubs := map[string]*lastFeed{}
	for n := uint32(1); n <= uint32(endBlock); n++ {
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
				if fp, ok := o.(op.FeedPublish); ok {
					name := string(fp.Publisher)
					if _, exists := pubs[name]; !exists {
						pubs[name] = &lastFeed{}
					}
					pubs[name].count++
					pubs[name].lastBlk = n
					pubs[name].lastBase = fp.ExchangeRate.Base.Amount
					pubs[name].lastQ = fp.ExchangeRate.Quote.Amount
				}
			}
		}
	}
	fmt.Printf("name\tcount\tlast_blk\tlast_rate\n")
	for name, lf := range pubs {
		fmt.Printf("%s\t%d\t%d\t%d/%d\n", name, lf.count, lf.lastBlk, lf.lastBase, lf.lastQ)
	}
}
