package auth

import "sync"

// AccountId is a process-local dense integer handle for an account name.
//
// It is NOT consensus state. hived's account_object carries a persisted
// object id, but this replay never serializes or compares account ids — the
// wire format and every snapshot key on the NAME. AccountId exists purely as
// a performance handle: it lets the hot indices (comment/vote/account maps)
// key by a uint32 instead of hashing+comparing name strings (~42% of dense-era
// CPU is string-keyed map ops), and it lets object name fields be stored as a
// 4-byte value instead of a GC-scanned string header.
//
// Id 0 is reserved as "invalid / unset" so a zero-valued field is detectable.
type AccountId uint32

// InvalidAccountId is the zero handle (no account).
const InvalidAccountId AccountId = 0

var (
	accountIdMu     sync.RWMutex
	accountIdByName = make(map[string]AccountId, 1<<20)
	accountNames    = []string{""} // index 0 reserved; accountNames[id] == name
)

// InternAccountId returns the dense id for name, assigning the next free id on
// first sight. The reverse table retains a single backing string per name.
// Concurrency-safe (verify/touchset run in parallel); the common case (already
// interned) takes only the read lock.
func InternAccountId(name string) AccountId {
	accountIdMu.RLock()
	id, ok := accountIdByName[name]
	accountIdMu.RUnlock()
	if ok {
		return id
	}
	accountIdMu.Lock()
	defer accountIdMu.Unlock()
	// Re-check under the write lock (another goroutine may have assigned it).
	if id, ok = accountIdByName[name]; ok {
		return id
	}
	// Copy the key so the table doesn't pin a caller's larger backing array
	// (mirrors fcraw.InternString's ownership discipline).
	owned := string([]byte(name))
	id = AccountId(len(accountNames))
	accountNames = append(accountNames, owned)
	accountIdByName[owned] = id
	return id
}

// LookupAccountId returns the id for an already-known name without assigning a
// new one — for read-only probes that must not grow the table.
func LookupAccountId(name string) (AccountId, bool) {
	accountIdMu.RLock()
	id, ok := accountIdByName[name]
	accountIdMu.RUnlock()
	return id, ok
}

// AccountNameOf returns the name for an id in O(1). Empty for InvalidAccountId
// or any id past the table (shouldn't happen for ids this process minted).
func AccountNameOf(id AccountId) string {
	accountIdMu.RLock()
	defer accountIdMu.RUnlock()
	if int(id) >= len(accountNames) {
		return ""
	}
	return accountNames[id]
}

// AccountIdCount returns the number of distinct names interned (incl. the
// reserved 0 slot). Test/diagnostic use.
func AccountIdCount() int {
	accountIdMu.RLock()
	defer accountIdMu.RUnlock()
	return len(accountNames)
}
