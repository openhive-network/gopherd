// Package config — environment switch compatibility.
package config

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Env reads a gopherd environment switch, accepting the legacy GHIVE_* name as
// a deprecated alias.
//
// The switches were named GHIVE_* under an older name for this project; the
// binary, logs and P2P identity are all "gopherd" now. Renaming outright would
// silently break every existing invocation and script — a switch that stops
// being read does not error, it just stops taking effect, and several of these
// change how state is loaded. So GOPHERD_* is preferred, GHIVE_* still works,
// and using the old name warns exactly once so it is visible without spamming.
//
// Precedence: GOPHERD_* wins if both are set, since that is the name someone
// setting it today would expect to be authoritative.
func Env(name string) string {
	if !strings.HasPrefix(name, "GOPHERD_") {
		panic("config.Env: switch names must start with GOPHERD_, got " + name)
	}
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	legacy := "GHIVE_" + strings.TrimPrefix(name, "GOPHERD_")
	if v, ok := os.LookupEnv(legacy); ok {
		warnLegacyOnce(legacy, name)
		return v
	}
	return ""
}

var legacyWarned sync.Map

func warnLegacyOnce(legacy, preferred string) {
	if _, seen := legacyWarned.LoadOrStore(legacy, true); seen {
		return
	}
	fmt.Fprintf(os.Stderr,
		"gopherd: %s is deprecated, use %s (still honoured for now)\n", legacy, preferred)
}
