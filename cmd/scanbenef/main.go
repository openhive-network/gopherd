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

// Usage: scanbenef <block_log> <start> <end> <beneficiary_account>
// Print every comment_options op naming <beneficiary_account> as a beneficiary.
func main() {
	r, err := blocklog.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer r.Close()
	start, _ := strconv.Atoi(os.Args[2])
	end, _ := strconv.Atoi(os.Args[3])
	target := os.Args[4]
	for n := uint32(start); n <= uint32(end); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		hdr, hdrLen, _ := block.UnmarshalSignedHeader(raw)
		_ = hdr
		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rd.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				break
			}
			for _, o := range txRes.SignedTx.Operations {
				if o.Tag != op.TagCommentOptions {
					continue
				}
				co := o.AsCommentOptions()
				if target == "ALL" {
					if len(co.Beneficiaries) > 0 {
						fmt.Printf("%d\t%s/%s\tbenefs=%v\n", n, co.Author, co.Permlink, co.Beneficiaries)
					}
					continue
				}
				for _, b := range co.Beneficiaries {
					if string(b.Account) == target {
						fmt.Printf("%d\tauthor=%s\tperm=%s\tpercent_hbd=%d\tbenef=%s@%d\tnum_benef=%d\n",
							n, co.Author, co.Permlink, co.PercentHbd, b.Account, b.Weight, len(co.Beneficiaries))
					}
				}
			}
		}
	}
}
