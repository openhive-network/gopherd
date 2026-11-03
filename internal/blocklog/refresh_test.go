package blocklog

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

// A Reader opened on a log that a Writer keeps appending to sees the new
// blocks after Refresh, and reads every block below its Head correctly.
func TestReaderRefreshFollowsWriter(t *testing.T) {
	for _, compress := range []bool{false, true} {
		t.Run(fmt.Sprintf("compress=%v", compress), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "block_log")
			w, err := OpenForWrite(path, compress)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			block := func(n int) []byte {
				return bytes.Repeat([]byte{byte(n), byte(n >> 8)}, 40+n)
			}
			for n := 1; n <= 5; n++ {
				if err := w.Append(block(n)); err != nil {
					t.Fatal(err)
				}
			}
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if r.Head() != 5 {
				t.Fatalf("head %d after open, want 5", r.Head())
			}
			for n := 6; n <= 12; n++ {
				if err := w.Append(block(n)); err != nil {
					t.Fatal(err)
				}
			}
			if r.Head() != 5 {
				t.Fatalf("head moved to %d without Refresh", r.Head())
			}
			if err := r.Refresh(); err != nil {
				t.Fatal(err)
			}
			if r.Head() != 12 {
				t.Fatalf("head %d after Refresh, want 12", r.Head())
			}
			for n := 1; n < 12; n++ {
				got, err := r.Block(uint32(n))
				if err != nil {
					t.Fatalf("block %d: %v", n, err)
				}
				if !bytes.Equal(got, block(n)) {
					t.Fatalf("block %d: wrong bytes after Refresh", n)
				}
			}
			// Nothing new: Refresh is a no-op.
			if err := r.Refresh(); err != nil || r.Head() != 12 {
				t.Fatalf("idle Refresh: head %d err %v", r.Head(), err)
			}
		})
	}
}
