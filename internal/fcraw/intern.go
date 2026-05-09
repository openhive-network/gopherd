package fcraw

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
// Single-threaded replay only — no lock. Production use (concurrent
// readers) would need sync.RWMutex around the map.
var internedStrings = make(map[string]string, 1<<14)

// InternString returns a canonical instance of the string contents of
// b. For repeated short strings (account names) this avoids the per-
// deserialization allocation. Long strings are passed through without
// caching to keep the map bounded.
func InternString(b []byte) string {
	if len(b) > HiveAccountNameMaxLen {
		return string(b)
	}
	if s, ok := internedStrings[string(b)]; ok {
		return s
	}
	s := string(b)
	internedStrings[s] = s
	return s
}
