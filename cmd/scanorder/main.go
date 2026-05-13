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
		fmt.Println("usage: scan_order <block_log> <owner> <orderid> <end_block>")
		os.Exit(2)
	}
	r, _ := blocklog.Open(os.Args[1])
	defer r.Close()
	owner := os.Args[2]
	orderID, _ := strconv.ParseUint(os.Args[3], 10, 32)
	endBlock, _ := strconv.Atoi(os.Args[4])
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
			for opIdx, o := range txRes.SignedTx.Operations {
				switch o2 := o.Operation().(type) {
				case op.LimitOrderCreate:
					if string(o2.Owner) == owner && uint64(o2.OrderID) == orderID {
						fmt.Printf("block %d tx %d op %d LIMIT_ORDER_CREATE owner=%s orderid=%d sell=%d/%d recv=%d/%d exp=%d nops=%d\n",
							n, i, opIdx, o2.Owner, o2.OrderID, o2.AmountToSell.Amount, o2.AmountToSell.Symbol.Num, o2.MinToReceive.Amount, o2.MinToReceive.Symbol.Num, o2.Expiration, len(txRes.SignedTx.Operations))
					}
				case op.LimitOrderCreate2:
					if string(o2.Owner) == owner && uint64(o2.OrderID) == orderID {
						fmt.Printf("block %d tx %d op %d LIMIT_ORDER_CREATE2 owner=%s orderid=%d sell=%d/%d exp=%d\n",
							n, i, opIdx, o2.Owner, o2.OrderID, o2.AmountToSell.Amount, o2.AmountToSell.Symbol.Num, o2.Expiration)
					}
				case op.LimitOrderCancel:
					if string(o2.Owner) == owner && uint64(o2.OrderID) == orderID {
						fmt.Printf("block %d tx %d op %d LIMIT_ORDER_CANCEL owner=%s orderid=%d\n", n, i, opIdx, o2.Owner, o2.OrderID)
					}
				}
			}
		}
	}
}
