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
		hdr, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil { continue }
		_ = hdr
		rdr := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rdr.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rdr)
			if err != nil { break }
			for _, o := range txRes.SignedTx.Operations {
				if pw, ok := o.Operation().(op.Pow2); ok {
					var worker string
					var prev string
					var summary uint32
					switch pw.WorkVariantTag {
					case 0:
						worker = pw.PowInput.WorkerAccount
						prev = fmt.Sprintf("%x", pw.PowInput.PrevBlock)
						summary = pw.PowSummary
					case 1:
						worker = pw.EquihashWork.Input.WorkerAccount
						prev = fmt.Sprintf("%x", pw.EquihashWork.PrevBlock)
						summary = pw.EquihashWork.PowSummary
					}
					var nokey string
					if pw.NewOwnerKey != nil { nokey = "set" } else { nokey = "nil" }
					fmt.Printf("block=%d variant=%d worker=%s prev=%s summary=%x props_max_blocksize=%d new_owner_key=%s\n",
						n, pw.WorkVariantTag, worker, prev, summary, pw.Props.MaximumBlockSize, nokey)
				}
			}
		}
	}
}
