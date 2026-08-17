package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

func runDumpVotes() {
	fs := flag.NewFlagSet("dumpvotes", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Pebble data dir")
	account := fs.String("account", "linouxis9", "voter account name")
	fs.Parse(os.Args[2:])
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "dumpvotes: --data-dir is required")
		os.Exit(2)
	}
	db, err := state.Open(*dataDir, 256<<20)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	s := state.NewSession(db)
	a, err := objects.GetAccount(s, *account)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("account: %s\n", *account)
	fmt.Printf("witnesses_voted_for (counter): %d\n", a.WitnessesVotedFor)

	prefix := state.AppendAccountName(nil, *account)
	var votes []string
	_ = s.Range(objects.WitnessVoteTable, state.PrimaryIndex, prefix,
		func(body, _ []byte) (bool, error) {
			if len(body) < 2*state.AccountNameKeyLen {
				return true, nil
			}
			wb := body[state.AccountNameKeyLen : 2*state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && wb[n-1] == 0 {
				n--
			}
			votes = append(votes, string(wb[:n]))
			return true, nil
		})
	sort.Strings(votes)
	fmt.Printf("actual rows: %d\n", len(votes))
	for _, w := range votes {
		fmt.Println("  ", w)
	}
}
