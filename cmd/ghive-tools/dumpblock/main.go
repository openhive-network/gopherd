package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/tx"
)

func main() {
	from := flag.Uint("from", 1, "from")
	to := flag.Uint("to", 1, "to")
	flag.Parse()
	r, err := blocklog.Open("block_log")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer r.Close()
	for n := uint32(*from); n <= uint32(*to); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		hdr, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			continue
		}
		fmt.Printf("block %d ts=%d witness=%s hdrLen=%d totalLen=%d\n", n, hdr.Timestamp, hdr.Witness, hdrLen, len(raw))
		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, err := rd.ReadVarUint()
		if err != nil {
			fmt.Printf("  tx_count err: %v\n", err)
			continue
		}
		fmt.Printf("  tx_count=%d\n", txCount)
		for i := uint32(0); i < txCount; i++ {
			res, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				fmt.Printf("  tx %d parse err: %v\n", i, err)
				break
			}
			fmt.Printf("  tx %d: ops=%d sigs=%d expiration=%d ref_block_num=%d ref_block_prefix=%d\n",
				i, len(res.SignedTx.Operations), len(res.SignedTx.Signatures),
				res.SignedTx.Expiration, res.SignedTx.RefBlockNum, res.SignedTx.RefBlockPrefix)
			for j, o := range res.SignedTx.Operations {
				fmt.Printf("    op %d: type=%T tag=%v\n", j, o, o.OpTag())
			}
			for j, sig := range res.SignedTx.Signatures {
				fmt.Printf("    sig %d: %s\n", j, hex.EncodeToString(sig[:]))
			}
		}
	}
}
