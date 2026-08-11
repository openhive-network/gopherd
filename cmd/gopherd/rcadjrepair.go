package main

// rcadjrepair writes each account's RCAdjustment into a directory snapshot from
// ground truth exported out of a hived node.
//
// WHY THIS EXISTS. hived sets an account's rc_adjustment at CREATION, from the
// creation fee converted to VESTS
// (hive_evaluator_account.cpp:159-164 -> account_object.hpp:38). We never did:
// rc_adjustment was only ever assigned by the HF20 initialisation loop, which
// touches accounts that existed AT HF20. Every account created afterwards
// carried rc_adjustment=0, so one with little or no VESTS computed
// max_rc = 0 and we refused every transaction it signed — while mainnet mined
// them all day. Measured cost: ~1 dropped transaction per block we build, and
// 100% of "we held it and dropped it" cases traced to exactly this.
//
// The code fix (7aad154) only covers accounts created from here on. The value
// cannot be recomputed for EXISTING accounts: it depends on the creation fee
// and the vesting price at that historical moment, neither of which survives in
// current state. A from-genesis replay would produce it — this tool is the
// cheap alternative, because rc_adjustment is IMMUTABLE once set. hived writes
// it in exactly two places (the HF20 loop and the account constructor) and
// never again, so a hived state of ANY vintage holds the permanently-correct
// value for every account that existed by then.
//
// WHAT IT DELIBERATELY DOES NOT TOUCH: rc_manabar.current_mana. That is a
// point-in-time quantity, and the exporting hived state is weeks old — copying
// it would install stale mana. Setting RCAdjustment is enough: MaximumRC()
// becomes correct immediately, and RegenerateRCMana refills the bar from the
// account's own last_update on first use.
//
// Usage: rcadjrepair <snapshot_dir> <rc_adjustments.json>
//
//	rc_adjustments.json: {"account":"<int64 adjustment>", ...}
//
// Writes <snapshot_dir>-rc with a patched account_store.bin (everything else
// hardlinked, manifest checksum updated).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
	"hived-go/internal/state/snapshot"
)

func cmdSnapshotRepairRCAdj(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rcadjrepair <snapshot_dir> <rc_adjustments.json>")
		return 2
	}
	snapDir, adjJSON := args[0], args[1]

	raw, err := os.ReadFile(adjJSON)
	if err != nil {
		panic(err)
	}
	// Values arrive as strings: rc_adjustment is an int64 that JSON numbers
	// would silently round through float64.
	var adjRaw map[string]string
	if err := json.Unmarshal(raw, &adjRaw); err != nil {
		panic(err)
	}
	adj := make(map[string]int64, len(adjRaw))
	for name, v := range adjRaw {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			panic(fmt.Errorf("account %q: bad adjustment %q: %w", name, v, perr))
		}
		adj[name] = n
	}
	fmt.Printf("loaded %d rc_adjustment values\n", len(adj))

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

	var names []string
	_ = as.RangePrimaryUnordered(func(a *objects.Account) (bool, error) {
		names = append(names, string(a.Name))
		return true, nil
	})

	var total, matched, patched, absent, wouldZero int
	var gainedRC int
	for _, name := range names {
		total++
		want, ok := adj[name]
		if !ok {
			// Created after the exporting hived state was taken. Leave it
			// alone: 0 may be wrong, but inventing a value would be worse.
			absent++
			continue
		}
		as.Mutate(auth.AccountName(name), func(a *objects.Account) {
			if a.RCAdjustment == want {
				matched++
				return
			}
			// Never REMOVE an adjustment we already have. If the export says 0
			// and we hold a positive value, the export is the stale one.
			if want == 0 && a.RCAdjustment != 0 {
				wouldZero++
				return
			}
			before := a.MaximumRC()
			a.RCAdjustment = want
			// LastMaxRC is a cached max; refresh it so the next regeneration
			// starts from the corrected ceiling. current_mana is left alone on
			// purpose — see the file comment.
			a.LastMaxRC = a.MaximumRC()
			if before == 0 && a.LastMaxRC > 0 {
				gainedRC++
			}
			patched++
		})
	}
	fmt.Printf("accounts=%d matched=%d patched=%d absent-from-export=%d stale-zero-skipped=%d\n",
		total, matched, patched, absent, wouldZero)
	fmt.Printf("accounts that went from max_rc=0 to a POSITIVE ceiling: %d\n", gainedRC)

	outDir := snapDir + "-rc"
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
