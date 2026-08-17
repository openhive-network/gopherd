package main

// snapshot-diff compares two state-jsonl files line-by-line, reporting
// objects present in only one side and field-by-field differences for
// objects on both sides. Designed to verify our chain state matches
// hived's state_snapshot output at HF1/HF20/HF28 boundaries.
//
// Usage: ghive-tools snapshot-diff <file1.jsonl> <file2.jsonl>
//
// Both files MUST be sorted by (type, name) — state-jsonl already
// emits in that order. The differ reads both streams in lockstep,
// comparing identifying keys, and prints differences to stdout. A
// summary trailer reports total diff count.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"hived-go/internal/statejsonl"
)

func cmdSnapshotDiff(args []string) int {
	fs := flag.NewFlagSet("snapshot-diff", flag.ContinueOnError)
	maxDiffs := fs.Int("max-diffs", 50, "stop after this many diff lines (0 = unlimited)")
	skipFields := fs.String("skip-fields",
		"rc_mana_current,rc_mana_last_update,last_max_rc,delegated_rc_in,delegated_rc_out,"+
			"voting_mana_current,voting_mana_last_update,hbd_last_interest_payment,"+
			"savings_hbd_last_interest_payment,signing_key,hardfork_version_vote,running_version",
		"comma-separated field names to ignore. Default drops fields the hived dump-jsonl can't populate from account_api (RC manabar, interest stamps) and encoding-divergent ones (signing_key b58-vs-hex, packed versions). Pass an empty value for a strict Go-vs-Go regression diff.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gopherd snapshot-diff <a.jsonl> <b.jsonl>")
		return 2
	}

	skip := make(map[string]bool)
	for _, f := range commaSplit(*skipFields) {
		skip[f] = true
	}

	a, err := os.Open(rest[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-diff: %v\n", err)
		return 1
	}
	defer a.Close()
	b, err := os.Open(rest[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-diff: %v\n", err)
		return 1
	}
	defer b.Close()

	scanA := bufio.NewScanner(a)
	scanA.Buffer(make([]byte, 64*1024), 4*1024*1024)
	scanB := bufio.NewScanner(b)
	scanB.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var aLine, bLine []byte
	var aHas, bHas bool
	advanceA := func() bool {
		if scanA.Scan() {
			aLine = append(aLine[:0], scanA.Bytes()...)
			return true
		}
		return false
	}
	advanceB := func() bool {
		if scanB.Scan() {
			bLine = append(bLine[:0], scanB.Bytes()...)
			return true
		}
		return false
	}

	aHas = advanceA()
	bHas = advanceB()

	diffs := 0
	for aHas || bHas {
		if *maxDiffs > 0 && diffs >= *maxDiffs {
			fmt.Printf("(diff limit %d reached; stopping)\n", *maxDiffs)
			break
		}
		switch {
		case aHas && !bHas:
			fmt.Printf("- %s\n", string(aLine))
			aHas = advanceA()
			diffs++
		case !aHas && bHas:
			fmt.Printf("+ %s\n", string(bLine))
			bHas = advanceB()
			diffs++
		default:
			ka := statejsonl.IdentKey(aLine)
			kb := statejsonl.IdentKey(bLine)
			switch {
			case ka < kb:
				fmt.Printf("- %s\n", string(aLine))
				aHas = advanceA()
				diffs++
			case ka > kb:
				fmt.Printf("+ %s\n", string(bLine))
				bHas = advanceB()
				diffs++
			default:
				if d := diffJSON(aLine, bLine, skip); d != "" {
					fmt.Printf("~ %s\n  %s\n", ka, d)
					diffs++
				}
				aHas = advanceA()
				bHas = advanceB()
			}
		}
	}
	if err := scanA.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "scan a: %v\n", err)
	}
	if err := scanB.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "scan b: %v\n", err)
	}
	if diffs == 0 {
		fmt.Println("PASS — snapshots match")
		return 0
	}
	fmt.Printf("FAIL — %d differences\n", diffs)
	return 1
}

// diffJSON returns a one-line description of differing fields between
// two JSON objects, or "" when they match (modulo skipped fields).
func diffJSON(aLine, bLine []byte, skip map[string]bool) string {
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(aLine, &a)
	_ = json.Unmarshal(bLine, &b)
	var out []string
	for k, av := range a {
		if skip[k] {
			continue
		}
		bv, ok := b[k]
		if !ok {
			out = append(out, fmt.Sprintf("missing.%s", k))
			continue
		}
		if !rawEq(av, bv) {
			out = append(out, fmt.Sprintf("%s: %s ≠ %s", k, av, bv))
		}
	}
	for k := range b {
		if skip[k] {
			continue
		}
		if _, ok := a[k]; !ok {
			out = append(out, fmt.Sprintf("extra.%s", k))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return joinShort(out, 80)
}

func rawEq(a, b json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func unquoteJSON(b json.RawMessage) string {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		return string(b[1 : len(b)-1])
	}
	return string(b)
}

func commaSplit(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func joinShort(parts []string, max int) string {
	out := ""
	for _, p := range parts {
		if len(out) > 0 {
			out += " | "
		}
		out += p
		if len(out) > max {
			out += " ..."
			break
		}
	}
	return out
}

// reading sentinel — never used.
var _ io.Reader = (*os.File)(nil)
