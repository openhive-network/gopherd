package main

// Loaders for ghive state. Two paths are supported:
//
//   1. --go-snapshot=PATH points at a snapshot dump produced by
//      `state.Database.SaveSnapshot` (already used by ghive-tools and
//      cmd/inspect-lp). Fast: no replay, just LoadSnapshot into an
//      in-memory database.
//
//   2. --go-replay=PATH points at a block_log; the tool replays from
//      genesis to --block. Slow but works without preexisting snapshots.
//
// Either way, the result is a `*state.Database` we can wrap in a
// `state.Session` and read all the state objects from.

import (
	"fmt"
	"os"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
	"hived-go/internal/state/snapshot"
)

// goState bundles a session and a few pre-fetched singletons. Multiple
// comparators read these fields, so cache them once.
type goState struct {
	db       *state.Database
	session  *state.Session
	blockNum uint32
}

// loadGoSnapshot opens the named snapshot. Auto-detects file (legacy
// single-file format) vs directory (the dir-based format produced by
// `ghive-tools replay --snapshot-every=N`). Returns the head block
// recorded in the snapshot header alongside the live state.
func loadGoSnapshot(path string) (*goState, error) {
	db := state.OpenMem()
	var headBN uint32
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		// Pre-register every typed subsystem on the freshly-opened DB
		// so Load matches the producing process's subsystem set.
		boot := state.NewSession(db)
		objects.EnsureWitnessStore(boot)
		objects.EnsureWitnessVoteStore(boot)
		objects.EnsureAccountStore(boot)
		objects.EnsureCommentStore(boot)
		objects.EnsureCommentVoteStore(boot)
		m, lerr := snapshot.Load(db, path)
		if lerr != nil {
			return nil, fmt.Errorf("load snapshot dir %s: %w", path, lerr)
		}
		headBN = m.BlockNum
	} else {
		bn, lerr := db.LoadSnapshot(path)
		if lerr != nil {
			return nil, fmt.Errorf("load snapshot %s: %w", path, lerr)
		}
		headBN = bn
	}
	return &goState{
		db:       db,
		session:  state.NewSession(db),
		blockNum: headBN,
	}, nil
}

// replayGoToBlock runs a fresh replay from genesis up to `target` using
// the block_log at `blockLogPath`. Slow path — only suitable when no
// snapshot is available.
func replayGoToBlock(blockLogPath string, target uint32) (*goState, error) {
	db := state.OpenMem()
	if err := genesis.Apply(db); err != nil {
		return nil, fmt.Errorf("apply genesis: %w", err)
	}
	cdb := chain.New(db)

	r, err := blocklog.Open(blockLogPath)
	if err != nil {
		return nil, fmt.Errorf("open block_log %s: %w", blockLogPath, err)
	}
	defer r.Close()

	for n := uint32(1); n <= target; n++ {
		raw, rerr := r.Block(n)
		if rerr != nil {
			// Missing block past the head — accept whatever we got so far.
			break
		}
		_, aerr := cdb.ApplyBlock(raw, n, chain.ApplyOptions{
			Sync:                      false,
			SkipTransactionSignatures: true,
			SkipBlockSignature:        true,
		}, nil)
		if aerr != nil {
			// Fail-fast: if Go can't apply the block, the state we'd
			// read is meaningless. Surface the error.
			return nil, fmt.Errorf("apply block %d: %w", n, aerr)
		}
	}
	return &goState{
		db:       db,
		session:  state.NewSession(db),
		blockNum: target,
	}, nil
}

// dgpo returns the parsed DGPO. Memoized — DGPO is read by ~6 comparators.
func (g *goState) dgpo() (objects.DynamicGlobalProperty, error) {
	return objects.GetDGPO(g.session)
}

// witnessSchedule returns the parsed witness_schedule.
func (g *goState) witnessSchedule() (objects.WitnessSchedule, error) {
	return objects.GetWitnessSchedule(g.session)
}

// feedHistory returns the parsed feed_history.
func (g *goState) feedHistory() (objects.FeedHistory, error) {
	return objects.GetFeedHistory(g.session)
}

// rewardFund returns a single named reward_fund.
func (g *goState) rewardFund(name string) (objects.RewardFund, error) {
	return objects.GetRewardFund(g.session, name)
}

// witnesses returns every witness in the primary index. Useful for
// per-witness comparisons (top 21 by votes, etc.).
func (g *goState) witnesses() ([]objects.Witness, error) {
	store := objects.EnsureWitnessStore(g.session)
	out := make([]objects.Witness, 0, 64)
	err := store.RangePrimary(func(w *objects.Witness) (bool, error) {
		out = append(out, *w)
		return true, nil
	})
	return out, err
}
