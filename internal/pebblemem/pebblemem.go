// Package pebblemem reports the memory pebble holds outside the Go heap.
//
// pebble allocates its block cache and memtables with C malloc when built
// with cgo, so none of it counts toward --mem-limit-gib or shows up in a Go
// heap profile. A process can sit at its Go limit and still be OOM-killed by
// its cgroup because of what pebble holds; this is the one place to see it.
package pebblemem

import (
	"bufio"
	"fmt"
	"os"
	"runtime/metrics"
	"sort"
	"strconv"
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

// LeakedIters counts pooled iterators closed by the GC rather than by Commit
// or Close: each pinned pebble memtables until its Session was collected.
// Anything above zero names a missing Close.
var LeakedIters atomic.Int64

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
	fmt.Fprintf(&b, "pebble state pooled iterators open: %d, closed by GC: %d\n", OpenIters.Load(), LeakedIters.Load())
	if anon, ok := rssAnon(); ok {
		goRes := goResident()
		fmt.Fprintf(&b, "process memory: anon %s, Go runtime %s, outside Go %s\n",
			mib(anon), mib(goRes), mib(anon-min(anon, goRes)))
	}
	return b.String()
}

// rssAnon reads the process's resident anonymous memory from
// /proc/self/status. ok is false where that file does not exist.
func rssAnon() (uint64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if rest, found := strings.CutPrefix(line, "RssAnon:"); found {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			if err != nil {
				return 0, false
			}
			return kb << 10, true
		}
	}
	return 0, false
}

// goResident is the memory the Go runtime has mapped and not returned to the
// OS: heap, stacks and runtime metadata. What anon holds beyond it is cgo
// allocations, pebble's above all. It is an upper bound (free heap the runtime
// has not yet returned may not be resident), so "outside Go" errs low.
func goResident() uint64 {
	s := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 || s[1].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64() - s[1].Value.Uint64()
}

func mib(n uint64) string { return fmt.Sprintf("%dM", n>>20) }
