package main

import (
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/protocol/block"
)

func main() {
	r, _ := blocklog.Open(os.Args[1])
	defer r.Close()
	from, _ := strconv.Atoi(os.Args[2])
	to, _ := strconv.Atoi(os.Args[3])
	target := os.Args[4]
	for n := uint32(from); n <= uint32(to); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		hdr, _, _ := block.UnmarshalSignedHeader(raw)
		if string(hdr.Witness) == target {
			fmt.Printf("block %d witness=%s\n", n, hdr.Witness)
		}
	}
}
