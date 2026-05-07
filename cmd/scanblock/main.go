package main

import (
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/tx"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: scanblock <block_log> <block_num>")
		os.Exit(2)
	}
	r, _ := blocklog.Open(os.Args[1])
	defer r.Close()
	n, _ := strconv.Atoi(os.Args[2])
	raw, err := r.Block(uint32(n))
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	hdr, hdrLen, _ := block.UnmarshalSignedHeader(raw)
	fmt.Printf("block %d t=%d witness=%s\n", n, hdr.Timestamp, hdr.Witness)
	rd := fcraw.NewReader(raw[hdrLen:])
	txCount, _ := rd.ReadVarUint()
	for i := uint32(0); i < txCount; i++ {
		txRes, err := tx.ReadSignedTransaction(rd)
		if err != nil {
			fmt.Printf("tx %d: error %v\n", i, err)
			continue
		}
		for j, o := range txRes.SignedTx.Operations {
			fmt.Printf("block %d tx %d op %d: %T %+v\n", n, i, j, o, o)
		}
	}
}
