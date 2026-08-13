package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// ghiveEnvSwitches are the GHIVE_* environment variables that change node
// behaviour. They exist because several are experiment/recovery knobs that do
// not belong in --help, but that makes them INVISIBLE: an operator cannot see
// which are active, and some materially change how state is loaded.
//
// GHIVE_COLD_REUSE in particular will serve STALE ROWS if paired with a cold
// dir from a different run, and nothing in the log would say so. Logging the
// active set at startup makes the node's actual configuration self-describing.
var ghiveEnvSwitches = []struct{ name, effect string }{
	{"GHIVE_COLD_REUSE", "skip rewriting cold rows on load — REQUIRES a cold dir from the same snapshot/run, else lookups return stale rows"},
	{"GHIVE_SLIM_COLD", "decode and slim frozen comments on load (smaller cold tier, slower load)"},
	{"GHIVE_COLD_COMPRESS", "cold-tier compression (zstd|snappy); empty = none"},
	{"GHIVE_SNAPSHOT_SKIP_COMMENTS", "load without the comment corpus — state is INCOMPLETE, API/consensus will be wrong"},
	{"GHIVE_SNAPSHOT_SERIAL", "load snapshot subsystems serially"},
	{"GHIVE_SNAPSHOT_LOAD_SERIAL", "load snapshot subsystems serially"},
	{"GHIVE_SNAPSHOT_LOAD_PACE_MB", "throttle snapshot load (MB/s)"},
	{"GHIVE_SNAPSHOT_ZSTD", "zstd-compress snapshot subsystem files"},
	{"GHIVE_RESUME_MARKER_INTERVAL", "blocks between replay resume markers"},
	{"GHIVE_INV_FINE_FROM", "p2p inventory fine-trace start block"},
	{"GHIVE_INV_FINE_EVERY", "p2p inventory fine-trace interval"},
	{"GHIVE_TRACE_LO", "state-trace low block bound"},
	{"GHIVE_TRACE_HI", "state-trace high block bound"},
}

// logActiveEnvSwitches prints every GHIVE_* switch that is actually set, with
// what it does. Silent when none are set, so a default node stays quiet.
func logActiveEnvSwitches(printf func(string, ...any)) {
	var active []string
	for _, sw := range ghiveEnvSwitches {
		if v, ok := os.LookupEnv(sw.name); ok && v != "" {
			active = append(active, fmt.Sprintf("  %s=%s — %s", sw.name, v, sw.effect))
		}
	}
	if len(active) == 0 {
		return
	}
	sort.Strings(active)
	printf("GHIVE_* switches active (these change node behaviour):\n%s", strings.Join(active, "\n"))
}
