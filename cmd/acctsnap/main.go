package main

import (
	"fmt"
	"os"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Usage: acctsnap <account_store.bin> <account> [account...]
// Reads a snapshot's account_store.bin directly (no cold tier needed) and
// prints the named accounts' full records — for diffing checkpoint state.
func main() {
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	boot := state.NewSession(state.OpenMem())
	st := objects.EnsureAccountStore(boot)
	if err := st.UnmarshalSnapshot(f); err != nil {
		panic(err)
	}
	for _, name := range os.Args[2:] {
		a, ok := st.Get(auth.AccountName(name))
		if !ok {
			fmt.Printf("%s: NOT FOUND\n", name)
			continue
		}
		fmt.Printf("%s: %+v\n\n", name, a)
	}
}
