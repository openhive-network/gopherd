package main

import (
	"encoding/hex"
	"fmt"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
	"os"
	"sort"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: inspect-lp <snapshot> [name1 ...]   (no names: list ALL by weight)")
		os.Exit(2)
	}
	db := state.OpenMem()
	if _, err := db.LoadSnapshot(os.Args[1]); err != nil {
		panic(err)
	}
	s := state.NewSession(db)
	if len(os.Args) >= 3 {
		for _, name := range os.Args[2:] {
			b, err := objects.GetLiquidityRewardBalance(s, auth.AccountName(name))
			if err != nil {
				fmt.Printf("%s: NO_ROW (%v)\n", name, err)
				continue
			}
			fmt.Printf("%s: hive_volume=%d hbd_volume=%d weight=%s last_update=%d\n",
				name, b.HiveVolume, b.HBDVolume, hex.EncodeToString(b.Weight[:]), b.LastUpdate)
		}
		return
	}
	// List all rows by walking primary index.
	type row struct {
		owner  string
		hive   int64
		hbd    int64
		weight [16]byte
		last   uint32
	}
	var rows []row
	_ = s.Range(objects.LiquidityRewardBalanceTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			b, err := objects.ReadLiquidityRewardBalance(value)
			if err != nil {
				return true, nil
			}
			rows = append(rows, row{
				owner: string(b.Owner), hive: b.HiveVolume, hbd: b.HBDVolume,
				weight: b.Weight, last: b.LastUpdate,
			})
			return true, nil
		})
	sort.Slice(rows, func(i, j int) bool {
		// Sort by weight descending.
		for k := 0; k < 16; k++ {
			if rows[i].weight[k] != rows[j].weight[k] {
				return rows[i].weight[k] > rows[j].weight[k]
			}
		}
		return rows[i].owner < rows[j].owner
	})
	for i, r := range rows {
		if i >= 30 {
			break
		}
		fmt.Printf("%-20s hive=%-15d hbd=%-15d weight=%s last=%d\n",
			r.owner, r.hive, r.hbd, hex.EncodeToString(r.weight[:]), r.last)
	}
	fmt.Printf("(total rows: %d)\n", len(rows))
}
