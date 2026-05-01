package main

import (
	"flag"
	"fmt"
	"os"

	"hived-go/internal/blocklog"
	"hived-go/internal/protocol/block"
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
		hdr, _, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			continue
		}
		fmt.Printf("block %d ts=%d witness=%s\n", n, hdr.Timestamp, hdr.Witness)
	}
}
