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
		fmt.Println("usage: scancomments <block_log> <author> <permlink> <end_block>")
		os.Exit(2)
	}
	r, _ := blocklog.Open(os.Args[1])
	defer r.Close()
	author := os.Args[2]
	permlink := os.Args[3]
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
			for _, o := range txRes.SignedTx.Operations {
				switch op2 := o.(type) {
				case op.Comment:
					if string(op2.Author) == author && op2.Permlink == permlink {
						fmt.Printf("block %d COMMENT author=%s permlink=%s parent=%s/%s\n", n, op2.Author, op2.Permlink, op2.ParentAuthor, op2.ParentPermlink)
					}
				case op.Vote:
					if string(op2.Author) == author && op2.Permlink == permlink {
						fmt.Printf("block %d VOTE voter=%s author=%s permlink=%s weight=%d\n", n, op2.Voter, op2.Author, op2.Permlink, op2.Weight)
					}
				case op.CommentOptions:
					if string(op2.Author) == author && op2.Permlink == permlink {
						fmt.Printf("block %d COMMENT_OPTIONS author=%s permlink=%s allowVotes=%t allowCuration=%t\n", n, op2.Author, op2.Permlink, op2.AllowVotes, op2.AllowCurationRewards)
					}
				case op.DeleteComment:
					if string(op2.Author) == author && op2.Permlink == permlink {
						fmt.Printf("block %d DELETE author=%s permlink=%s\n", n, op2.Author, op2.Permlink)
					}
				}
			}
		}
	}
}
