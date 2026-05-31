//go:build !linux

package blocklog

import "os"

// dropCacheUpTo is a no-op on non-Linux platforms (no posix_fadvise binding).
func dropCacheUpTo(f *os.File, upTo int64) {}
