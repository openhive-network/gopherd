// Command cppstate is a small framework around hived's JSON-RPC API.
// It lets us query C++'s authoritative state at any replayed block
// without recompiling hived (which the project rule forbids).
//
// Workflow:
//   1. Replay hived to block N: scripts/hived-replay-to.sh N.
//   2. Start hived API server: scripts/hived-api-serve.sh & (in background).
//   3. Use this tool to query state and diff against Go's snapshot.
//
// Subcommands:
//   list-witnesses       fetch every witness; print pow_worker, votes, vst.
//   pow-queue            fetch every witness, print rows where pow_worker > 0.
//   dgpo                 fetch dynamic_global_property_object.
//   compare-pow-queue    diff hived's by_pow content vs a Go state snapshot.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// rpcEndpoint is the default JSON-RPC URL the hived-api-serve script binds to.
// Override with --endpoint=URL on the cli.
const rpcEndpoint = "http://127.0.0.1:18090"

// rpcCall is a thin JSON-RPC v2 client over HTTP. The webserver_plugin
// implements method dispatch via "api_name.method_name".
//
// All hived API methods we care about are nullary or take a single object
// arg, so accepting `params` as an `interface{}` keeps this generic.
func rpcCall(endpoint, method string, params interface{}) (json.RawMessage, error) {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("decode %s: %w (body=%q)", method, err, string(respBody))
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, fmt.Errorf("%s: %s", method, string(env.Error))
	}
	return env.Result, nil
}

// witness is a partial mirror of database_api::api_witness_object — we
// only decode the fields we need for parity work.
type witness struct {
	Owner                string      `json:"owner"`
	PowWorker            uint64      `json:"pow_worker"`
	VirtualScheduledTime string      `json:"virtual_scheduled_time"`
	Votes                json.Number `json:"votes"`
}

// listWitnesses pages through every witness via database_api.list_witnesses
// (sort=by_name) until the result set runs out. The API has a per-call
// limit (typically 1000), so we resume from the last name + "\x00".
func listWitnesses(endpoint string) ([]witness, error) {
	var all []witness
	start := ""
	for {
		params := map[string]interface{}{
			"start": start,
			"limit": 1000,
			"order": "by_name",
		}
		raw, err := rpcCall(endpoint, "database_api.list_witnesses", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Witnesses []witness `json:"witnesses"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode list_witnesses: %w", err)
		}
		if len(page.Witnesses) == 0 {
			break
		}
		// First call: include all.
		// Subsequent calls: skip the first (it's the resume cursor).
		if len(all) == 0 {
			all = append(all, page.Witnesses...)
		} else {
			all = append(all, page.Witnesses[1:]...)
		}
		if len(page.Witnesses) < 1000 {
			break
		}
		start = page.Witnesses[len(page.Witnesses)-1].Owner
	}
	return all, nil
}

// dgpo decodes hived's dynamic_global_property_object into the few fields
// we care about. The full struct is much wider; json.Unmarshal silently
// drops keys we don't define.
type dgpoResult struct {
	HeadBlockNumber uint32 `json:"head_block_number"`
	NumPowWitnesses uint32 `json:"num_pow_witnesses"`
	TotalPow        uint64 `json:"total_pow"`
}

func cmdDGPO(endpoint string) int {
	raw, err := rpcCall(endpoint, "database_api.get_dynamic_global_properties", map[string]interface{}{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var d dgpoResult
	if err := json.Unmarshal(raw, &d); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("head_block_number=%d num_pow_witnesses=%d total_pow=%d\n",
		d.HeadBlockNumber, d.NumPowWitnesses, d.TotalPow)
	return 0
}

func cmdListWitnesses(endpoint string) int {
	ws, err := listWitnesses(endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, w := range ws {
		fmt.Printf("%s\t%d\t%s\t%s\n", w.Owner, w.PowWorker, w.Votes, w.VirtualScheduledTime)
	}
	fmt.Fprintf(os.Stderr, "total: %d witnesses\n", len(ws))
	return 0
}

func cmdPowQueue(endpoint string) int {
	ws, err := listWitnesses(endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pow := make([]witness, 0, len(ws))
	for _, w := range ws {
		if w.PowWorker > 0 {
			pow = append(pow, w)
		}
	}
	sort.Slice(pow, func(i, j int) bool { return pow[i].PowWorker < pow[j].PowWorker })
	for _, w := range pow {
		fmt.Printf("%d\t%s\n", w.PowWorker, w.Owner)
	}
	fmt.Fprintf(os.Stderr, "queue size: %d (pow_worker > 0)\n", len(pow))
	return 0
}

// cmdComparePowQueue walks Go's by_pow index in the named state snapshot
// and the C++ pow queue from the API, then prints witnesses that are in
// only one side. Helps localize the 14-witness gap that's been blocking
// schedule parity at block 1041516.
func cmdComparePowQueue(endpoint, snapshotPath string) int {
	cppRaw, err := listWitnesses(endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cpp:", err)
		return 1
	}
	cppPow := make(map[string]uint64)
	for _, w := range cppRaw {
		if w.PowWorker > 0 {
			cppPow[w.Owner] = w.PowWorker
		}
	}

	goPow := make(map[string]uint64)
	if err := loadGoPowFromSnapshot(snapshotPath, goPow); err != nil {
		fmt.Fprintln(os.Stderr, "go:", err)
		return 1
	}

	onlyCpp := make([]string, 0)
	for name, pw := range cppPow {
		if _, ok := goPow[name]; !ok {
			onlyCpp = append(onlyCpp, fmt.Sprintf("%d\t%s", pw, name))
		}
	}
	sort.Strings(onlyCpp)
	onlyGo := make([]string, 0)
	for name, pw := range goPow {
		if _, ok := cppPow[name]; !ok {
			onlyGo = append(onlyGo, fmt.Sprintf("%d\t%s", pw, name))
		}
	}
	sort.Strings(onlyGo)
	mismatch := make([]string, 0)
	for name, cpw := range cppPow {
		if gpw, ok := goPow[name]; ok && gpw != cpw {
			mismatch = append(mismatch, fmt.Sprintf("%s\tcpp=%d\tgo=%d", name, cpw, gpw))
		}
	}
	sort.Strings(mismatch)

	fmt.Printf("cpp queue=%d, go queue=%d\n", len(cppPow), len(goPow))
	fmt.Printf("only-in-cpp (%d):\n", len(onlyCpp))
	for _, s := range onlyCpp {
		fmt.Printf("  %s\n", s)
	}
	fmt.Printf("only-in-go (%d):\n", len(onlyGo))
	for _, s := range onlyGo {
		fmt.Printf("  %s\n", s)
	}
	fmt.Printf("mismatched pow_worker (%d):\n", len(mismatch))
	for _, s := range mismatch {
		fmt.Printf("  %s\n", s)
	}
	return 0
}

// loadGoPowFromSnapshot opens a Go state snapshot (produced by
// `ghive-tools replay --save-snapshot-to=...`) and walks the witness
// table, populating `out` with each witness's pow_worker (skipping
// pow_worker == 0).
func loadGoPowFromSnapshot(path string, out map[string]uint64) error {
	if path == "" {
		return fmt.Errorf("--snapshot path required")
	}
	db := state.OpenMem()
	if _, err := db.LoadSnapshot(path); err != nil {
		return err
	}
	s := state.NewSession(db)
	return s.Range(objects.WitnessTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		w, err := objects.ReadWitness(value)
		if err != nil {
			return true, nil
		}
		if w.PowWorker > 0 {
			out[string(w.Owner)] = w.PowWorker
		}
		return true, nil
	})
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cppstate [--endpoint=URL] <subcommand> [args...]")
	fmt.Fprintln(os.Stderr, "subcommands: dgpo, list-witnesses, pow-queue, compare-pow-queue --snapshot=PATH")
}

func main() {
	endpoint := flag.String("endpoint", rpcEndpoint, "JSON-RPC endpoint hived is bound to")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	// Probe once so we fail fast with a clear message if hived isn't running yet.
	if _, err := rpcCall(*endpoint, "database_api.get_config", map[string]interface{}{}); err != nil {
		fmt.Fprintf(os.Stderr, "cppstate: hived API at %s not responding (%v)\n", *endpoint, err)
		fmt.Fprintln(os.Stderr, "Did you run scripts/hived-api-serve.sh?")
		// Wait briefly and retry once — the API can take a few seconds to
		// come up after replay finishes.
		time.Sleep(2 * time.Second)
		if _, err := rpcCall(*endpoint, "database_api.get_config", map[string]interface{}{}); err != nil {
			os.Exit(1)
		}
	}
	args := flag.Args()
	switch strings.ToLower(args[0]) {
	case "dgpo":
		os.Exit(cmdDGPO(*endpoint))
	case "list-witnesses":
		os.Exit(cmdListWitnesses(*endpoint))
	case "pow-queue":
		os.Exit(cmdPowQueue(*endpoint))
	case "compare-pow-queue":
		fs := flag.NewFlagSet("compare-pow-queue", flag.ExitOnError)
		snapshot := fs.String("snapshot", "", "path to a Go state snapshot at the same block hived is replayed to")
		_ = fs.Parse(args[1:])
		os.Exit(cmdComparePowQueue(*endpoint, *snapshot))
	default:
		usage()
		os.Exit(2)
	}
}
