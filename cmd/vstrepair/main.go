package main

// vstrepair resyncs Go's witness virtual-scheduling state (and the feed
// history it contaminated) from a hived ground-truth dump taken at the SAME
// block. The witness VOTE totals match hived exactly (verified — this tool
// asserts it), but virtual_scheduled_time / virtual_position carry small
// inherited phase offsets from the snapshot lineage; those occasionally swap
// adjacent timeshare picks (first seen: qurator↔timcliff at 59,030,370),
// which corrupts one hourly feed-median entry and eventually a payout sat.
//
// Usage: vstrepair <snapshot_dir> <hived_all_witnesses.json> <hived_sched.json> <hived_feed.json>
//
// Writes <snapshot_dir>-vst with patched witness_store.bin + mem.bin
// (bulk files hardlinked, manifest checksums rewritten).

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math/big"
	"os"
	"path/filepath"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func u128FromDecimal(s string) (out [16]byte) {
	n := new(big.Int)
	if _, ok := n.SetString(s, 10); !ok {
		panic("bad u128 decimal: " + s)
	}
	b := n.Bytes()
	copy(out[16-len(b):], b)
	return
}

type hivedWitness struct {
	Owner                string `json:"owner"`
	Votes                json.Number `json:"votes"`
	VirtualLastUpdate    string `json:"virtual_last_update"`
	VirtualPosition      string `json:"virtual_position"`
	VirtualScheduledTime string `json:"virtual_scheduled_time"`
}

type amount struct {
	Amount string `json:"amount"`
	Nai    string `json:"nai"`
}
type hivedPrice struct {
	Base  amount `json:"base"`
	Quote amount `json:"quote"`
}

func toPrice(p hivedPrice) asset.Price {
	var base, quote int64
	fmt.Sscanf(p.Base.Amount, "%d", &base)
	fmt.Sscanf(p.Quote.Amount, "%d", &quote)
	baseSym, quoteSym := asset.Symbol{Num: asset.HbdSymbol}, asset.Symbol{Num: asset.HiveSymbol}
	if p.Base.Nai == "@@000000021" {
		baseSym, quoteSym = quoteSym, baseSym
	}
	return asset.Price{
		Base:  asset.Asset{Amount: base, Symbol: baseSym},
		Quote: asset.Asset{Amount: quote, Symbol: quoteSym},
	}
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

func main() {
	snapDir, witJSON, schedJSON, feedJSON := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	var hw []hivedWitness
	raw, err := os.ReadFile(witJSON)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(raw, &hw); err != nil {
		panic(err)
	}

	// --- Patch witness_store ---
	db := state.OpenMem()
	boot := state.NewSession(db)
	ws := objects.EnsureWitnessStore(boot)
	f, err := os.Open(filepath.Join(snapDir, "subsystems", "witness_store.bin"))
	if err != nil {
		panic(err)
	}
	if err := ws.UnmarshalSnapshot(bufio.NewReaderSize(f, 1<<20)); err != nil {
		panic(err)
	}
	f.Close()

	voteMismatch, patched, missing := 0, 0, 0
	for _, w := range hw {
		votes, verr := w.Votes.Int64()
		if verr != nil {
			panic(verr)
		}
		found := ws.Mutate(auth.AccountName(w.Owner), func(g *objects.Witness) {
			if g.TotalVotesVests != votes {
				voteMismatch++
				fmt.Printf("VOTE MISMATCH %s go=%d hived=%d\n", w.Owner, g.TotalVotesVests, votes)
			}
			g.VirtualLastUpdate = u128FromDecimal(w.VirtualLastUpdate)
			g.VirtualPosition = u128FromDecimal(w.VirtualPosition)
			g.VirtualScheduledTime = u128FromDecimal(w.VirtualScheduledTime)
		})
		if found {
			patched++
		} else {
			missing++
			fmt.Printf("MISSING in Go: %s\n", w.Owner)
		}
	}
	fmt.Printf("witnesses patched=%d missing=%d vote_mismatches=%d\n", patched, missing, voteMismatch)
	if voteMismatch > 0 || missing > 0 {
		fmt.Println("ABORT: state disagreement beyond VST phase")
		os.Exit(1)
	}

	// --- Patch mem.bin: WitnessSchedule.CurrentVirtualTime + FeedHistory ---
	memF, err := os.Open(filepath.Join(snapDir, "mem.bin"))
	if err != nil {
		panic(err)
	}
	br := bufio.NewReaderSize(memF, 1<<20)
	var magic [8]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || string(magic[:]) != "GHIVEMEM" {
		panic("bad mem.bin magic")
	}
	var verBuf [4]byte
	if _, err := io.ReadFull(br, verBuf[:]); err != nil {
		panic(err)
	}
	if err := state.ReadAllRows(db, br); err != nil {
		panic(err)
	}
	memF.Close()

	s := state.NewSession(db)
	var schedIn struct {
		Result struct {
			CurrentVirtualTime string `json:"current_virtual_time"`
		} `json:"result"`
	}
	raw, _ = os.ReadFile(schedJSON)
	if err := json.Unmarshal(raw, &schedIn); err != nil {
		panic(err)
	}
	// WitnessSchedule lives in its own typed store file, not mem.bin.
	wss := objects.EnsureWitnessScheduleStore(s)
	sf, err := os.Open(filepath.Join(snapDir, "subsystems", "witness_schedule_store.bin"))
	if err != nil {
		panic(err)
	}
	if err := wss.UnmarshalSnapshot(bufio.NewReaderSize(sf, 1<<20)); err != nil {
		panic(err)
	}
	sf.Close()
	sched, ok := wss.Get()
	if !ok {
		panic("witness schedule missing")
	}
	oldCVT := sched.CurrentVirtualTime
	sched.CurrentVirtualTime = u128FromDecimal(schedIn.Result.CurrentVirtualTime)
	wss.Put(&sched)
	fmt.Printf("current_virtual_time: %x -> %x\n", oldCVT, sched.CurrentVirtualTime)

	var feedIn struct {
		Result struct {
			CurrentMedianHistory hivedPrice   `json:"current_median_history"`
			MarketMedianHistory  hivedPrice   `json:"market_median_history"`
			CurrentMinHistory    hivedPrice   `json:"current_min_history"`
			CurrentMaxHistory    hivedPrice   `json:"current_max_history"`
			PriceHistory         []hivedPrice `json:"price_history"`
		} `json:"result"`
	}
	raw, _ = os.ReadFile(feedJSON)
	if err := json.Unmarshal(raw, &feedIn); err != nil {
		panic(err)
	}
	fh, err := objects.GetFeedHistory(s)
	if err != nil {
		panic(err)
	}
	diffEntries := 0
	newHist := make([]asset.Price, len(feedIn.Result.PriceHistory))
	for i, p := range feedIn.Result.PriceHistory {
		newHist[i] = toPrice(p)
		if i < len(fh.PriceHistory) &&
			(fh.PriceHistory[i].Base.Amount != newHist[i].Base.Amount || fh.PriceHistory[i].Quote.Amount != newHist[i].Quote.Amount) {
			diffEntries++
			fmt.Printf("price_history[%d]: go=%d/%d hived=%d/%d\n", i,
				fh.PriceHistory[i].Base.Amount, fh.PriceHistory[i].Quote.Amount,
				newHist[i].Base.Amount, newHist[i].Quote.Amount)
		}
	}
	fmt.Printf("price_history entries differing: %d (len go=%d hived=%d)\n", diffEntries, len(fh.PriceHistory), len(newHist))
	fh.PriceHistory = newHist
	fh.CurrentMedianHistory = toPrice(feedIn.Result.CurrentMedianHistory)
	fh.MarketMedianHistory = toPrice(feedIn.Result.MarketMedianHistory)
	fh.CurrentMinHistory = toPrice(feedIn.Result.CurrentMinHistory)
	fh.CurrentMaxHistory = toPrice(feedIn.Result.CurrentMaxHistory)
	objects.PutFeedHistory(s, &fh)
	if err := s.Commit(false); err != nil {
		panic(err)
	}

	// --- Write the -vst sibling ---
	outDir := snapDir + "-vst"
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

	subs, _ := os.ReadDir(filepath.Join(snapDir, "subsystems"))
	for _, e := range subs {
		if e.Name() == "witness_store.bin" || e.Name() == "witness_schedule_store.bin" {
			continue
		}
		if err := os.Link(filepath.Join(snapDir, "subsystems", e.Name()), filepath.Join(outDir, "subsystems", e.Name())); err != nil {
			panic(err)
		}
	}
	// witness_store.bin
	wf, err := os.Create(filepath.Join(outDir, "subsystems", "witness_store.bin"))
	if err != nil {
		panic(err)
	}
	bw := bufio.NewWriterSize(wf, 1<<20)
	if err := ws.MarshalSnapshot(bw); err != nil {
		panic(err)
	}
	bw.Flush()
	wf.Close()
	// witness_schedule_store.bin
	ssf, err := os.Create(filepath.Join(outDir, "subsystems", "witness_schedule_store.bin"))
	if err != nil {
		panic(err)
	}
	sbw := bufio.NewWriterSize(ssf, 1<<20)
	if err := wss.MarshalSnapshot(sbw); err != nil {
		panic(err)
	}
	sbw.Flush()
	ssf.Close()
	// mem.bin
	mf, err := os.Create(filepath.Join(outDir, "mem.bin"))
	if err != nil {
		panic(err)
	}
	mw := bufio.NewWriterSize(mf, 1<<20)
	mw.Write([]byte("GHIVEMEM"))
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 1)
	mw.Write(hdr[:])
	if err := state.WriteAllRows(db, mw); err != nil {
		panic(err)
	}
	mw.Flush()
	mf.Close()

	man["mem_sha256"] = hashFile(filepath.Join(outDir, "mem.bin"), algo)
	for _, sub := range man["subsystems"].([]any) {
		entry := sub.(map[string]any)
		if entry["name"] == "witness_store" {
			entry["sha256"] = hashFile(filepath.Join(outDir, "subsystems", "witness_store.bin"), algo)
		}
		if entry["name"] == "witness_schedule_store" {
			entry["sha256"] = hashFile(filepath.Join(outDir, "subsystems", "witness_schedule_store.bin"), algo)
		}
	}
	outMan, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), outMan, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s\n", outDir)
}
