package main

import (
	"fmt"
	"os"
	"strconv"

	"gopherd/internal/blocklog"
	"gopherd/internal/protocol/block"
)

func cmdBlocklogTS(args []string) int {
	os.Args = append([]string{"gopherd-blocklog-ts"}, args...)

	r, err := blocklog.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer r.Close()
	for _, a := range os.Args[2:] {
		n, _ := strconv.ParseUint(a, 10, 32)
		raw, err := r.Block(uint32(n))
		if err != nil {
			fmt.Println(n, "ERR", err)
			continue
		}
		hdr, _, _ := block.UnmarshalSignedHeader(raw)
		fmt.Println(n, hdr.Timestamp)
	}
	return 0
}
