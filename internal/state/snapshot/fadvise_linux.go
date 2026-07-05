//go:build linux

package snapshot

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropCacheUpTo drops the OS page cache for [0, upTo) of f
// (posix_fadvise DONTNEED). Snapshot files are consumed strictly forward and
// never re-read within a run; releasing consumed pages keeps a multi-GB
// save/load from evicting the replayer's heap and the cold tier's hot pages
// under a tight cgroup. Best-effort: errors ignored. (Same pattern as
// blocklog's forward-replay cache drop.)
func dropCacheUpTo(f *os.File, upTo int64) {
	if f == nil || upTo <= 0 {
		return
	}
	_ = unix.Fadvise(int(f.Fd()), 0, upTo, unix.FADV_DONTNEED)
}

// diskFree returns the free bytes available to this process on the
// filesystem holding path, and whether the probe worked.
func diskFree(path string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}
