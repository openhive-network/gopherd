package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Usage: kvsnap <mem.bin>
// Loads a dir-snapshot's raw K/V dump and ranges the vesting-delegation
// expiration by-date index — verifies secondary-index rows round-trip.
func cmdStateKV(args []string) int {
	os.Args = append([]string{"gopherd-state-kv"}, args...)

	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	db := state.OpenMem()
	br := bufio.NewReader(f)
	var hdr [12]byte // "GHIVEMEM" + u32 version
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		panic(err)
	}
	if string(hdr[:8]) != "GHIVEMEM" {
		panic(fmt.Sprintf("bad magic %q", hdr[:8]))
	}
	if err := state.ReadAllRows(db, br); err != nil {
		panic(err)
	}
	s := state.NewSession(db)
	n := 0
	_ = s.Range(objects.VestingDelegationExpirationTable, objects.VestingDelegationExpirationByDateIndex, nil,
		func(key, val []byte) (bool, error) {
			exp := binary.BigEndian.Uint32(key[0:4])
			name := key[4 : 4+state.AccountNameKeyLen]
			nn := state.AccountNameKeyLen
			for nn > 0 && name[nn-1] == 0 {
				nn--
			}
			n++
			if n <= 12 {
				fmt.Printf("exp=%d delegator=%s\n", exp, name[:nn])
			}
			return true, nil
		})
	fmt.Printf("total by-date expiration rows: %d\n", n)
	return 0
}
