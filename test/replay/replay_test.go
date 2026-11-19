//go:build replay

// Package replay is the opt-in regression tier: it replays the start of a
// mainnet block_log and checks chain totals at fixed checkpoints against the
// values recorded in checkpoints.json.
//
//	GOPHERD_BLOCK_LOG=/path/to/block_log make test-replay
//	GOPHERD_BLOCK_LOG=... GOPHERD_REPLAY_TO=20000000 make test-replay
//	GOPHERD_BLOCK_LOG=... go test -tags replay ./test/replay -run TestReplayCheckpoints -update
//
// At every checkpoint the conservation invariants (hived's
// validate_invariants) must hold, and the recorded totals (supplies, the
// vesting pool, the reward fund, the virtual supply) must match. The recorded
// values come from gopherd itself at a commit whose full replay agreed with
// hived (see docs/testing.md), so a mismatch means behaviour changed: either a
// regression, or a fix whose new values must be confirmed against hived before
// the file is updated with -update.
package replay

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"

	"gopherd/internal/blocklog"
	"gopherd/internal/chain"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

var update = flag.Bool("update", false, "rewrite checkpoints.json with this run's values")

const (
	checkpointEvery = 250_000
	defaultReplayTo = 10_000_000
	checkpointsFile = "checkpoints.json"
)

// Checkpoint is the chain state recorded after a block.
type Checkpoint struct {
	Block                uint32 `json:"block"`
	HeadBlockID          string `json:"head_block_id"`
	CurrentSupply        int64  `json:"current_supply"`
	CurrentHBDSupply     int64  `json:"current_hbd_supply"`
	VirtualSupply        int64  `json:"virtual_supply"`
	TotalVestingShares   int64  `json:"total_vesting_shares"`
	TotalVestingFundHive int64  `json:"total_vesting_fund_hive"`
	TotalRewardFundHive  int64  `json:"total_reward_fund_hive"`
	TotalRewardShares2   string `json:"total_reward_shares2"`
}

func TestReplayCheckpoints(t *testing.T) {
	path := os.Getenv("GOPHERD_BLOCK_LOG")
	if path == "" {
		t.Skip("set GOPHERD_BLOCK_LOG to a mainnet block_log")
	}
	to := uint32(defaultReplayTo)
	if v := os.Getenv("GOPHERD_REPLAY_TO"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			t.Fatalf("GOPHERD_REPLAY_TO=%q: %v", v, err)
		}
		to = uint32(n)
	}

	var want map[uint32]Checkpoint
	if !*update {
		want = loadCheckpoints(t)
	}

	log, err := blocklog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if log.Head() < to {
		t.Fatalf("block_log ends at %d, need %d", log.Head(), to)
	}

	db, err := state.Open(t.TempDir(), 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.ApplyForNetwork(db, nil); err != nil {
		t.Fatal(err)
	}
	cdb := chain.New(db)
	opts := chain.ApplyOptions{
		SkipTransactionSignatures: true,
		SkipBlockSignature:        true,
		SkipDedupAndTaPOS:         true,
		SkipTxSavepoints:          true,
	}

	var got []Checkpoint
	var stats chain.Stats
	for n := uint32(1); n <= to; n++ {
		raw, err := log.Block(n)
		if err != nil {
			t.Fatalf("block %d read: %v", n, err)
		}
		if _, err := cdb.ApplyBlock(raw, n, opts, &stats); err != nil {
			t.Fatalf("block %d apply: %v", n, err)
		}
		if n%checkpointEvery != 0 && n != to {
			continue
		}
		cp := checkpoint(t, db, n)
		got = append(got, cp)
		t.Logf("block %d: supply %d hbd %d vests %d", n, cp.CurrentSupply, cp.CurrentHBDSupply, cp.TotalVestingShares)
		if w, ok := want[n]; ok && w != cp {
			t.Fatalf("block %d differs from %s:\n got %+v\nwant %+v", n, checkpointsFile, cp, w)
		}
	}
	if *update {
		writeCheckpoints(t, got)
	}
}

// checkpoint records the state after block n, failing if hived's
// conservation invariants do not hold.
func checkpoint(t *testing.T, db *state.Database, n uint32) Checkpoint {
	t.Helper()
	s := state.NewSession(db)
	defer s.Close()
	inv, err := chain.CheckInvariants(s)
	if err != nil {
		t.Fatalf("block %d invariants: %v", n, err)
	}
	if !inv.OK() {
		t.Fatalf("block %d breaks conservation:%s", n, inv.FailString())
	}
	d, err := objects.GetDGPO(s)
	if err != nil {
		t.Fatal(err)
	}
	return Checkpoint{
		Block:                n,
		HeadBlockID:          hex.EncodeToString(d.HeadBlockID[:]),
		CurrentSupply:        d.CurrentSupply,
		CurrentHBDSupply:     d.CurrentHBDSupply,
		VirtualSupply:        d.VirtualSupply,
		TotalVestingShares:   d.TotalVestingShares,
		TotalVestingFundHive: d.TotalVestingFundHive,
		TotalRewardFundHive:  d.TotalRewardFundHive,
		TotalRewardShares2:   hex.EncodeToString(d.TotalRewardShares2[:]),
	}
}

func loadCheckpoints(t *testing.T) map[uint32]Checkpoint {
	t.Helper()
	raw, err := os.ReadFile(checkpointsFile)
	if err != nil {
		t.Fatalf("%v (run with -update to record)", err)
	}
	var list []Checkpoint
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	m := make(map[uint32]Checkpoint, len(list))
	for _, c := range list {
		m[c.Block] = c
	}
	return m
}

func writeCheckpoints(t *testing.T, list []Checkpoint) {
	t.Helper()
	if old, err := os.ReadFile(checkpointsFile); err == nil {
		var prev []Checkpoint
		if json.Unmarshal(old, &prev) == nil {
			seen := map[uint32]bool{}
			for _, c := range list {
				seen[c.Block] = true
			}
			for _, c := range prev {
				if !seen[c.Block] {
					list = append(list, c)
				}
			}
		}
	}
	slices.SortFunc(list, func(a, b Checkpoint) int { return cmp.Compare(a.Block, b.Block) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointsFile, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("wrote %d checkpoints to %s\n", len(list), checkpointsFile)
}
