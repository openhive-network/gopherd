package main

import (
	"fmt"
	"gopherd/internal/config"
	"sort"
	"strings"
	"time"
)

// gopherdEnvSwitches are the GOPHERD_* environment variables that change node
// behaviour. They exist because several are experiment/recovery knobs that do
// not belong in --help, but that makes them INVISIBLE: an operator cannot see
// which are active, and some materially change how state is loaded.
//
// GOPHERD_COLD_REUSE in particular will serve STALE ROWS if paired with a cold
// dir from a different run, and nothing in the log would say so. Logging the
// active set at startup makes the node's actual configuration self-describing.
var gopherdEnvSwitches = []struct{ name, effect string }{
	{"GOPHERD_COLD_REUSE", "skip rewriting cold rows on load — REQUIRES a cold dir from the same snapshot/run, else lookups return stale rows"},
	{"GOPHERD_SLIM_COLD", "decode and slim frozen comments on load (smaller cold tier, slower load)"},
	{"GOPHERD_COLD_COMPRESS", "cold-tier compression (zstd|snappy); empty = none"},
	{"GOPHERD_SNAPSHOT_SKIP_COMMENTS", "load without the comment corpus — state is INCOMPLETE, API/consensus will be wrong"},
	{"GOPHERD_SNAPSHOT_SERIAL", "load snapshot subsystems serially"},
	{"GOPHERD_SNAPSHOT_LOAD_SERIAL", "load snapshot subsystems serially"},
	{"GOPHERD_SNAPSHOT_LOAD_PACE_MB", "throttle snapshot load (MB/s)"},
	{"GOPHERD_SNAPSHOT_ZSTD", "zstd-compress snapshot subsystem files"},
	{"GOPHERD_RESUME_MARKER_INTERVAL", "blocks between replay resume markers"},
	{"GOPHERD_INV_FINE_FROM", "p2p inventory fine-trace start block"},
	{"GOPHERD_INV_FINE_EVERY", "p2p inventory fine-trace interval"},
	{"GOPHERD_TRACE_LO", "state-trace low block bound"},
	{"GOPHERD_TRACE_HI", "state-trace high block bound"},
}

// logActiveEnvSwitches prints every GHIVE_* switch that is actually set, with
// what it does. Silent when none are set, so a default node stays quiet.
func logActiveEnvSwitches(printf func(string, ...any)) {
	var active []string
	for _, sw := range gopherdEnvSwitches {
		// config.Env also accepts the deprecated GHIVE_* alias, so a node
		// started with the old names still reports its real configuration.
		if v := config.Env(sw.name); v != "" {
			active = append(active, fmt.Sprintf("  %s=%s — %s", sw.name, v, sw.effect))
		}
	}
	if len(active) == 0 {
		return
	}
	sort.Strings(active)
	printf("GOPHERD_* switches active (these change node behaviour):\n%s", strings.Join(active, "\n"))
}

// procStart is stamped as early as possible so every boot phase can be reported
// against a single origin. Startup previously logged per-file timings but never
// "how long until this node was actually up", which is the number an operator
// waiting on a restart actually cares about.
var procStart = time.Now()
