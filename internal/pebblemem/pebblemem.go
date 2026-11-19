// Package pebblemem reports the memory pebble holds outside the Go heap.
//
// pebble allocates its block cache and memtables with C malloc when built
// with cgo, so none of it counts toward --mem-limit-gib or shows up in a Go
// heap profile. A process can sit at its Go limit and still be OOM-killed by
// its cgroup because of what pebble holds; this is the one place to see it.
package pebblemem

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble"
)

var (
	mu  sync.Mutex
	dbs = map[string]*pebble.DB{}
)

// OpenIters counts the state backend's pooled iterators that are open. Each
// pins the memtables live when it was made, so a count that only rises means
// a Session is being dropped without Commit or Close.
var OpenIters atomic.Int64

// Register adds db under name until Unregister; a later Register with the
// same name replaces it.
func Register(name string, db *pebble.DB) {
	mu.Lock()
	dbs[name] = db
	mu.Unlock()
}

// Unregister removes db if it is still the one registered under name.
func Unregister(name string, db *pebble.DB) {
	mu.Lock()
	if dbs[name] == db {
		delete(dbs, name)
	}
	mu.Unlock()
}

// Report returns one line per registered DB with its memtable, block-cache,
// compaction and iterator figures, sorted by name. Empty when none is open.
func Report() string {
	mu.Lock()
	names := make([]string, 0, len(dbs))
	for n := range dbs {
		names = append(names, n)
	}
	sort.Strings(names)
	list := make([]*pebble.DB, len(names))
	for i, n := range names {
		list[i] = dbs[n]
	}
	mu.Unlock()

	var b strings.Builder
	for i, db := range list {
		m := db.Metrics()
		fmt.Fprintf(&b, "pebble %s: memtables %d (%s) zombie %d (%s), block cache %s, flushes in progress %d, compactions %d (%s), table iters %d, zombie tables %d, snapshots %d\n",
			names[i],
			m.MemTable.Count, mib(m.MemTable.Size), m.MemTable.ZombieCount, mib(m.MemTable.ZombieSize),
			mib(uint64(m.BlockCache.Size)),
			m.Flush.NumInProgress,
			m.Compact.NumInProgress, mib(uint64(m.Compact.InProgressBytes)),
			m.TableIters, m.Table.ZombieCount, m.Snapshots.Count)
	}
	fmt.Fprintf(&b, "pebble state pooled iterators open: %d\n", OpenIters.Load())
	return b.String()
}

func mib(n uint64) string { return fmt.Sprintf("%dM", n>>20) }
