package main

// Repair mode: reconstruct EVERY creator's true pending_claimed_accounts
// (claims - creates since HF20) from the block_log, compare with a snapshot's
// account_store.bin, and rewrite the store with corrected values. The
// historical uint16 field wrapped at 65,536 for heavy creators (ocdb et al.),
// so snapshots written by pre-fix binaries store `true - k*65536`.
//
// Usage: claimcount -repair <block_log> <end_block> <snapshot_dir>
//
// Scans (HF20_BLOCK, end_block], patches <snapshot_dir>/subsystems/
// account_store.bin IN A SIBLING "-fixed" COPY of the snapshot dir
// (comment_store.bin etc. hardlinked), and rewrites manifest checksums.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

const hf20Block = 26256743

func scanAllCreators(path string, from, to uint32, workers int) map[string]int64 {
	shards := make([]map[string]int64, workers)
	span := to - from + 1
	step := span / uint32(workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := from + uint32(w)*step
		hi := lo + step - 1
		if w == workers-1 {
			hi = to
		}
		wg.Add(1)
		go func(w int, lo, hi uint32) {
			defer wg.Done()
			m := make(map[string]int64)
			// local reader per worker
			r, err := blocklog.Open(path)
			if err != nil {
				panic(err)
			}
			defer r.Close()
			var raw, out []byte
			for n := lo; n <= hi; n++ {
				blk, newRaw, err := r.BlockReuse(n, raw, out)
				raw = newRaw
				if err != nil {
					panic(fmt.Sprintf("block %d: %v", n, err))
				}
				out = blk
				_, hdrLen, err := block.UnmarshalSignedHeader(blk)
				if err != nil {
					panic(fmt.Sprintf("block %d hdr: %v", n, err))
				}
				rd := fcraw.NewReader(blk[hdrLen:])
				txCount, _ := rd.ReadVarUint()
				for i := uint32(0); i < txCount; i++ {
					txRes, err := tx.ReadSignedTransaction(rd)
					if err != nil {
						break
					}
					for _, o := range txRes.SignedTx.Operations {
						switch o.Tag {
						case op.TagClaimAccount:
							m[string(o.AsClaimAccount().Creator)]++
						case op.TagCreateClaimedAccount:
							m[string(o.AsCreateClaimedAccount().Creator)]--
						}
					}
				}
			}
			shards[w] = m
		}(w, lo, hi)
	}
	wg.Wait()
	total := make(map[string]int64)
	for _, m := range shards {
		for k, v := range m {
			total[k] += v
		}
	}
	return total
}

func repairMain(blockLog string, endBlock uint32, snapDir string) {
	fmt.Printf("scanning claim ops (%d, %d]...\n", hf20Block, endBlock)
	truth := scanAllCreators(blockLog, hf20Block+1, endBlock, 10)

	// Load the snapshot's account store.
	storePath := filepath.Join(snapDir, "subsystems", "account_store.bin")
	f, err := os.Open(storePath)
	if err != nil {
		panic(err)
	}
	boot := state.NewSession(state.OpenMem())
	st := objects.EnsureAccountStore(boot)
	if err := st.UnmarshalSnapshot(f); err != nil {
		panic(err)
	}
	f.Close()

	// Compare + patch.
	type fix struct {
		name       string
		stored, tr int64
	}
	var fixes []fix
	for name, tr := range truth {
		a, ok := st.Get(auth.AccountName(name))
		if !ok {
			if tr != 0 {
				fmt.Printf("WARN: creator %q not in snapshot (true pending %d)\n", name, tr)
			}
			continue
		}
		if a.PendingClaimedAccounts != tr {
			fixes = append(fixes, fix{name, a.PendingClaimedAccounts, tr})
		}
	}
	sort.Slice(fixes, func(i, j int) bool { return fixes[i].name < fixes[j].name })
	for _, fx := range fixes {
		delta := fx.tr - fx.stored
		if delta%65536 != 0 {
			fmt.Printf("SUSPICIOUS %s: stored=%d true=%d delta=%d (NOT a wrap multiple)\n", fx.name, fx.stored, fx.tr, delta)
		} else {
			fmt.Printf("fix %s: stored=%d true=%d (+%d wraps)\n", fx.name, fx.stored, fx.tr, delta/65536)
		}
		fxLocal := fx
		st.Mutate(auth.AccountName(fxLocal.name), func(a *objects.Account) { a.PendingClaimedAccounts = fxLocal.tr })
	}
	if len(fixes) == 0 {
		fmt.Println("no fixes needed")
		return
	}

	// Write the -fixed sibling snapshot: hardlink everything, replace
	// account_store.bin, rewrite manifest checksums for the changed file.
	fixedDir := snapDir + "-fixed"
	if err := os.RemoveAll(fixedDir); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Join(fixedDir, "subsystems"), 0o755); err != nil {
		panic(err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		panic(err)
	}
	var man map[string]any
	if err := json.Unmarshal(manifestRaw, &man); err != nil {
		panic(err)
	}
	if err := os.Link(filepath.Join(snapDir, "mem.bin"), filepath.Join(fixedDir, "mem.bin")); err != nil {
		panic(err)
	}
	subs, _ := os.ReadDir(filepath.Join(snapDir, "subsystems"))
	for _, e := range subs {
		if e.Name() == "account_store.bin" {
			continue
		}
		if err := os.Link(filepath.Join(snapDir, "subsystems", e.Name()), filepath.Join(fixedDir, "subsystems", e.Name())); err != nil {
			panic(err)
		}
	}
	// Serialize the patched store, hashing with the manifest's algo.
	algo, _ := man["hash_algo"].(string)
	out, err := os.Create(filepath.Join(fixedDir, "subsystems", "account_store.bin"))
	if err != nil {
		panic(err)
	}
	h := newHash(algo)
	mw := io.MultiWriter(out, h)
	if err := st.MarshalSnapshot(mw); err != nil {
		panic(err)
	}
	if err := out.Close(); err != nil {
		panic(err)
	}
	newSum := hex.EncodeToString(h.Sum(nil))
	subsList := man["subsystems"].([]any)
	for _, s := range subsList {
		entry := s.(map[string]any)
		if entry["name"] == "account_store" {
			entry["sha256"] = newSum
		}
	}
	outMan, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(fixedDir, "manifest.json"), outMan, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s (%d accounts fixed)\n", fixedDir, len(fixes))
}

func newHash(algo string) hash.Hash {
	if algo == "crc32c" {
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	}
	return sha256.New()
}
