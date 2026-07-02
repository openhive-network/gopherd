package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// Usage: scanfeeds_range <block_log> <from> <to>
// Print every feed_publish in [from,to]: block, publisher, base/quote.
func main() {
	r, err := blocklog.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer r.Close()
	from, _ := strconv.Atoi(os.Args[2])
	to, _ := strconv.Atoi(os.Args[3])
	for n := uint32(from); n <= uint32(to); n++ {
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
				break
			}
			for _, o := range txRes.SignedTx.Operations {
				if fp, ok := o.Operation().(op.FeedPublish); ok {
					fmt.Printf("%d\t%s\t%d/%d\tfeed_publish\n", n, fp.Publisher,
						fp.ExchangeRate.Base.Amount, fp.ExchangeRate.Quote.Amount)
				}
				if wsp, ok := o.Operation().(op.WitnessSetProperties); ok {
					for _, p := range wsp.Props {
						if (p.Key == "hbd_exchange_rate" || p.Key == "sbd_exchange_rate") && len(p.Value) >= 32 {
							base := int64(binary.LittleEndian.Uint64(p.Value[0:8]))
							quote := int64(binary.LittleEndian.Uint64(p.Value[16:24]))
							fmt.Printf("%d\t%s\t%d/%d\twsp\n", n, wsp.Owner, base, quote)
						}
					}
				}
			}
		}
	}
}
