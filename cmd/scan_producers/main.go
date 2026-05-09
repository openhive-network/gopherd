package main

import (
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/protocol/block"
)

// Usage: scan_producers <block_log> <start_block> <count>
// Print witness names for [start_block, start_block+count-1].
func main() {
	r, err := blocklog.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer r.Close()
	start, _ := strconv.Atoi(os.Args[2])
	count, _ := strconv.Atoi(os.Args[3])
	for n := uint32(start); n < uint32(start)+uint32(count); n++ {
		raw, err := r.Block(n)
		if err != nil {
			fmt.Printf("%d\tERR\t%v\n", n, err)
			continue
		}
		hdr, _, _ := block.UnmarshalSignedHeader(raw)
		fmt.Printf("%d\t%s\n", n, hdr.Witness)
	}
}
