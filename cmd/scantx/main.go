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
	start, _ := strconv.Atoi(os.Args[2])
	end, _ := strconv.Atoi(os.Args[3])
	for n := uint32(start); n <= uint32(end); n++ {
		raw, err := r.Block(n)
		if err != nil { continue }
		_, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil { continue }
		rdr := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rdr.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rdr)
			if err != nil { break }
			hasPow := false
			for _, o := range txRes.SignedTx.Operations {
				if _, ok := o.Operation().(op.Pow2); ok { hasPow = true; break }
				if _, ok := o.Operation().(op.Pow); ok { hasPow = true; break }
			}
			if hasPow {
				fmt.Printf("block=%d tx=%d ops=%d:", n, i, len(txRes.SignedTx.Operations))
				for _, o := range txRes.SignedTx.Operations {
					fmt.Printf(" %s", o.Tag.Name())
				}
				fmt.Println()
			}
		}
	}
}
