package fcraw

import "sync"

// HiveAccountNameMaxLen is the protocol-bounded maximum length for a
// Hive account_name (HIVE_MAX_ACCOUNT_NAME_LENGTH in libraries/protocol/
// include/hive/protocol/config.hpp = 16). Used by ReadString to decide
// whether to intern a short string.
const HiveAccountNameMaxLen = 16

// internedStrings is a process-wide cache of short strings, primarily
// used to dedup account-name allocations during deserialization. Hive
// account names are bounded to 16 characters and a single account name
// (e.g. "initminer", "steemit") may be deserialized millions of times
// during replay — each appearing as Owner, From, To, Author, Voter,
// Witness, etc. in different objects. Interning collapses these into
// a single backing string.
//
// The map[string]string keyed lookup uses Go's special case for
// `m[string(byteSlice)]` which avoids allocating during the lookup
// itself; only the miss path allocates the canonical string.
//
// Phase 2 onward this map is hit concurrently by p2p sync, gossip, and
// witness goroutines all deserializing blocks in parallel. RWMutex is
// required to avoid the runtime's "concurrent map read and map write"
// fatal. Reads use RLock (no contention between readers); the rare
// miss escalates to Lock for the insert with a re-check inside.
var (
	// Plain Mutex over RWMutex: single Lock+Unlock per call beats
	// RLock+RUnlock when the read path is short (a single map probe).
	// Even in live-sync with multiple deserialize goroutines, the lock
	// is held only for the map probe — contention stays low.
	internMu        sync.Mutex
	internedStrings = make(map[string]string, 1<<14)
)

// InternString returns a canonical instance of the string contents of
// b. For repeated short strings (account names) this avoids the per-
// deserialization allocation. Long strings are passed through without
// caching to keep the map bounded.
func InternString(b []byte) string {
	if len(b) > HiveAccountNameMaxLen {
		return string(b)
	}
	internMu.Lock()
	if s, ok := internedStrings[string(b)]; ok {
		internMu.Unlock()
		return s
	}
	s := string(b)
	internedStrings[s] = s
	internMu.Unlock()
	return s
}
