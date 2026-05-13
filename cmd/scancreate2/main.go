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
	r, _ := blocklog.Open(os.Args[1])
	defer r.Close()
	startBlock, _ := strconv.Atoi(os.Args[2])
	endBlock, _ := strconv.Atoi(os.Args[3])
	count := 0
	for n := uint32(startBlock); n <= uint32(endBlock); n++ {
		raw, err := r.Block(n)
		if err != nil { continue }
		_, hdrLen, _ := block.UnmarshalSignedHeader(raw)
		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rd.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rd)
			if err != nil { continue }
			for _, o := range txRes.SignedTx.Operations {
				if c2, ok := o.Operation().(op.LimitOrderCreate2); ok {
					count++
					if count <= 10 {
						fmt.Printf("block %d CREATE2 owner=%s sell=%d/%d rate=%d/%d/%d/%d\n",
							n, c2.Owner, c2.AmountToSell.Amount, c2.AmountToSell.Symbol.Num,
							c2.ExchangeRate.Base.Amount, c2.ExchangeRate.Base.Symbol.Num,
							c2.ExchangeRate.Quote.Amount, c2.ExchangeRate.Quote.Symbol.Num)
					}
				}
			}
		}
	}
	fmt.Printf("total create2 ops in range %d-%d: %d\n", startBlock, endBlock, count)
}
