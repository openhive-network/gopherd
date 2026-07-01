package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"hived-go/internal/state/objects"
)

// dgpodump decodes a snapshot dgpo_store.bin and prints the vesting pool + supply.
func main() {
	for _, path := range os.Args[1:] {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("%s: %v\n", path, err)
			continue
		}
		// magic(4) + version(4) + valid(1) + uvarint(len) + AppendDGPO
		if len(b) < 9 || string(b[0:4]) != "DGPO" {
			fmt.Printf("%s: bad magic\n", path)
			continue
		}
		p := 9
		_, n := binary.Uvarint(b[p:])
		p += n
		d, err := objects.ReadDGPO(b[p:])
		if err != nil {
			fmt.Printf("%s: decode: %v\n", path, err)
			continue
		}
		fmt.Printf("%s\n  F(fund_hive)=%d  S(vesting_shares)=%d  current_supply=%d  hbd_supply=%d  virtual_supply=%d\n",
			path, d.TotalVestingFundHive, d.TotalVestingShares, d.CurrentSupply, d.CurrentHBDSupply, d.VirtualSupply)
	}
}
