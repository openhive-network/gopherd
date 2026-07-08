package main

// govexprepair rewrites every account's GovernanceVoteExpiration stamp in a
// directory snapshot from ground truth reconstructed by govexpscan (the last
// governance action ≤ the snapshot block), applying the exact C++ formula
// from account_object::update_governance_vote_expiration_ts:
//
//	exp = last_action + 365d
//	if exp < HF25_TIME+365d { exp = HF25_TIME+365d + (exp % (365d/2)) }
//
// Snapshots produced before commit 300ba72 froze the spread at each
// account's FIRST pre-HF25 action (and clamped regressions); this tool
// resets the stamps to what a from-genesis replay with the fixed code
// would have produced.
//
// Usage: govexprepair <snapshot_dir> <govactions.json> <cutoff_block>
// Writes <snapshot_dir>-gov with patched account_store.bin (other files
// hardlinked, manifest checksum updated).

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"fmt"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
	"hived-go/internal/state/snapshot"
)

const (
	periodSec  uint32 = 365 * 24 * 60 * 60
	hf25Time   uint32 = 1625061600
	firstExp   uint32 = hf25Time + periodSec
	spreadSize uint32 = periodSec / 2
)

func expected(lastAction uint32) uint32 {
	exp := lastAction + periodSec
	if exp < firstExp {
		exp = firstExp + (exp % spreadSize)
	}
	return exp
}

func newHash(algo string) hash.Hash {
	if algo == "crc32c" {
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	}
	return sha256.New()
}

func hashFile(path, algo string) string {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	h := newHash(algo)
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		panic(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cmdSnapshotRepairGovexp(args []string) int {
	os.Args = append([]string{"gopherd-snapshot-repair-govexp"}, args...)

	snapDir, actionsJSON := os.Args[1], os.Args[2]
	cutoff64, err := strconv.ParseUint(os.Args[3], 10, 32)
	if err != nil {
		panic(err)
	}
	cutoff := uint32(cutoff64)

	raw, err := os.ReadFile(actionsJSON)
	if err != nil {
		panic(err)
	}
	var actions map[string][2]uint32 // name -> [block, ts]
	if err := json.Unmarshal(raw, &actions); err != nil {
		panic(err)
	}

	db := state.OpenMem()
	boot := state.NewSession(db)
	as := objects.EnsureAccountStore(boot)
	f, err := os.Open(filepath.Join(snapDir, "subsystems", "account_store.bin"))
	if err != nil {
		panic(err)
	}
	r, decClose, err := snapshot.MaybeCompressedReader(f)
	if err != nil {
		panic(err)
	}
	if err := as.UnmarshalSnapshot(r); err != nil {
		panic(err)
	}
	decClose()
	f.Close()

	var patched, zeroedStray, matched, total int
	var names []string
	_ = as.RangePrimaryUnordered(func(a *objects.Account) (bool, error) {
		names = append(names, string(a.Name))
		return true, nil
	})
	for _, name := range names {
		total++
		var want uint32
		if l, ok := actions[name]; ok && l[0] <= cutoff {
			want = expected(l[1])
		}
		as.Mutate(auth.AccountName(name), func(a *objects.Account) {
			if a.GovernanceVoteExpiration == want {
				matched++
				return
			}
			if want == 0 {
				zeroedStray++
			}
			patched++
			a.GovernanceVoteExpiration = want
		})
	}
	fmt.Printf("accounts=%d matched=%d patched=%d (stray-zeroed=%d)\n", total, matched, patched, zeroedStray)

	outDir := snapDir + "-gov"
	if err := os.RemoveAll(outDir); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Join(outDir, "subsystems"), 0o755); err != nil {
		panic(err)
	}
	manRaw, err := os.ReadFile(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		panic(err)
	}
	var man map[string]any
	if err := json.Unmarshal(manRaw, &man); err != nil {
		panic(err)
	}
	algo, _ := man["hash_algo"].(string)

	if err := os.Link(filepath.Join(snapDir, "mem.bin"), filepath.Join(outDir, "mem.bin")); err != nil {
		panic(err)
	}
	subs, _ := os.ReadDir(filepath.Join(snapDir, "subsystems"))
	for _, e := range subs {
		if e.Name() == "account_store.bin" {
			continue
		}
		if err := os.Link(filepath.Join(snapDir, "subsystems", e.Name()), filepath.Join(outDir, "subsystems", e.Name())); err != nil {
			panic(err)
		}
	}
	af, err := os.Create(filepath.Join(outDir, "subsystems", "account_store.bin"))
	if err != nil {
		panic(err)
	}
	bw := bufio.NewWriterSize(af, 1<<20)
	if err := as.MarshalSnapshot(bw); err != nil {
		panic(err)
	}
	bw.Flush()
	af.Close()

	for _, sub := range man["subsystems"].([]any) {
		entry := sub.(map[string]any)
		if entry["name"] == "account_store" {
			entry["sha256"] = hashFile(filepath.Join(outDir, "subsystems", "account_store.bin"), algo)
		}
	}
	outMan, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), outMan, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s\n", outDir)
	return 0
}
