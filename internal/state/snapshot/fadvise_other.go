//go:build !linux

package snapshot

import "os"

func dropCacheUpTo(f *os.File, upTo int64) {}

func diskFree(path string) (uint64, bool) { return 0, false }
