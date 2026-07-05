//go:build !linux

package snapshot

import "os"

func dropCacheUpTo(f *os.File, upTo int64) {}
