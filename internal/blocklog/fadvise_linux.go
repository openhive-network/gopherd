//go:build linux

package blocklog

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropCacheUpTo drops the OS page cache for the byte range [0, upTo) of f via
// posix_fadvise(POSIX_FADV_DONTNEED). Used during forward replay of very large
// block_logs to release the page cache for blocks already consumed, so the
// kernel does not evict the replayer's heap to cache file pages it will never
// re-read. Best-effort: any error is ignored.
func dropCacheUpTo(f *os.File, upTo int64) {
	if f == nil || upTo <= 0 {
		return
	}
	_ = unix.Fadvise(int(f.Fd()), 0, upTo, unix.FADV_DONTNEED)
}
