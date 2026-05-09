package main

// Comparator framework. A `comparator` is a closure that pulls the
// same field from both Go and hived state and returns zero or more
// drift records. The runner aggregates and sorts them by table-priority
// then field name, so the output stays greppable.
//
// Adding a new comparator:
//
//   1. Pick the table it lives in (DGPO, witness_schedule, feed_history,
//      reward_fund, witness:NAME).
//   2. Use `cmpUint64`, `cmpInt64`, `cmpString`, `cmpBytes` for primitive
//      fields, or build a small custom function for structured fields.
//   3. Append the comparator to the slice returned by buildComparators.
//
// Output format per drift:
//
//   BLOCK 5,148,656: dgpo.current_aslot Go=5193905 hived=5194004 (Δ=-99)
//
// where Δ is computed for numeric fields only.

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
)

// drift records one diverging field at one block.
type drift struct {
	block uint32
	table string // "dgpo", "witness_schedule", "feed_history", "reward_fund:post", "witness:owner"
	field string
	go_   string // stringified Go value
	hived string // stringified hived value
	delta string // "Δ=..." or "" if not numeric
}

// String renders the drift in the canonical greppable form.
func (d drift) String() string {
	out := fmt.Sprintf("BLOCK %s: %s.%s Go=%s hived=%s",
		humanBlock(d.block), d.table, d.field, d.go_, d.hived)
	if d.delta != "" {
		out += " (" + d.delta + ")"
	}
	return out
}

// humanBlock formats large block numbers with thousands separators —
// "5,148,656" reads better than 5148656 in mixed grep output.
func humanBlock(n uint32) string {
	s := strconv.FormatUint(uint64(n), 10)
	if len(s) <= 3 {
		return s
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	first := len(s) % 3
	if first > 0 {
		out = append(out, s[:first]...)
		if first < len(s) {
			out = append(out, ',')
		}
	}
	for i := first; i < len(s); i += 3 {
		out = append(out, s[i:i+3]...)
		if i+3 < len(s) {
			out = append(out, ',')
		}
	}
	return string(out)
}

// tablePriority orders tables in the output. Lower value = printed
// earlier. The intuition: cascade-likely tables (DGPO, schedule, feed)
// surface first, per-witness drift second.
var tablePriority = map[string]int{
	"dgpo":             0,
	"witness_schedule": 1,
	"feed_history":     2,
	"reward_fund:post": 3,
}

func tableRank(t string) int {
	if v, ok := tablePriority[t]; ok {
		return v
	}
	if len(t) >= 8 && t[:8] == "witness:" {
		return 10
	}
	return 99
}

// sortDrifts orders drifts by (block asc, table priority asc, field asc).
func sortDrifts(ds []drift) {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].block != ds[j].block {
			return ds[i].block < ds[j].block
		}
		if rt := tableRank(ds[i].table) - tableRank(ds[j].table); rt != 0 {
			return rt < 0
		}
		if ds[i].table != ds[j].table {
			return ds[i].table < ds[j].table
		}
		return ds[i].field < ds[j].field
	})
}

// --- primitive comparators ------------------------------------------------
//
// These return drift{} (with empty .field) when there's no divergence;
// callers decide whether to record. Keeping them tiny makes the
// per-table comparators very compact.

// cmpInt64 emits a drift if go and hived differ.
func cmpInt64(block uint32, table, field string, goV, hivedV int64) (drift, bool) {
	if goV == hivedV {
		return drift{}, false
	}
	return drift{
		block: block,
		table: table,
		field: field,
		go_:   strconv.FormatInt(goV, 10),
		hived: strconv.FormatInt(hivedV, 10),
		delta: fmt.Sprintf("Δ=%d", goV-hivedV),
	}, true
}

func cmpUint64(block uint32, table, field string, goV, hivedV uint64) (drift, bool) {
	if goV == hivedV {
		return drift{}, false
	}
	delta := ""
	if goV >= hivedV {
		delta = fmt.Sprintf("Δ=+%d", goV-hivedV)
	} else {
		delta = fmt.Sprintf("Δ=-%d", hivedV-goV)
	}
	return drift{
		block: block,
		table: table,
		field: field,
		go_:   strconv.FormatUint(goV, 10),
		hived: strconv.FormatUint(hivedV, 10),
		delta: delta,
	}, true
}

func cmpUint32(block uint32, table, field string, goV, hivedV uint32) (drift, bool) {
	return cmpUint64(block, table, field, uint64(goV), uint64(hivedV))
}

func cmpUint16(block uint32, table, field string, goV, hivedV uint16) (drift, bool) {
	return cmpUint64(block, table, field, uint64(goV), uint64(hivedV))
}

func cmpUint8(block uint32, table, field string, goV, hivedV uint8) (drift, bool) {
	return cmpUint64(block, table, field, uint64(goV), uint64(hivedV))
}

func cmpString(block uint32, table, field, goV, hivedV string) (drift, bool) {
	if goV == hivedV {
		return drift{}, false
	}
	return drift{
		block: block,
		table: table,
		field: field,
		go_:   strconv.Quote(goV),
		hived: strconv.Quote(hivedV),
	}, true
}

// cmpBytes compares two byte arrays as hex strings. Used for uint128
// fields (16 bytes BE).
func cmpBytes(block uint32, table, field string, goV, hivedV []byte) (drift, bool) {
	if len(goV) == len(hivedV) {
		eq := true
		for i := range goV {
			if goV[i] != hivedV[i] {
				eq = false
				break
			}
		}
		if eq {
			return drift{}, false
		}
	}
	return drift{
		block: block,
		table: table,
		field: field,
		go_:   "0x" + hex.EncodeToString(goV),
		hived: "0x" + hex.EncodeToString(hivedV),
	}, true
}
