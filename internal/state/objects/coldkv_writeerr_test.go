package objects

import (
	"testing"
)

// TestColdWriteErrIsStickyAndVisible pins that a cold-tier write failure is
// observable rather than swallowed.
//
// Eviction removes a comment from the resident map BEFORE the batch lands. If
// the write then fails, the row is gone and every later read reports "comment
// does not exist" — silent state loss, with a disk-full as the realistic
// trigger. The error used to be discarded (`_ = c.db.Apply(...)`) with nothing
// logged, so the node carried on serving a hole in its state.
func TestColdWriteErrIsStickyAndVisible(t *testing.T) {
	c, err := openColdKV(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.db.Close()

	// Healthy tier: no error reported.
	c.bset([]byte("k"), []byte("v"))
	c.flushBatch()
	if c.WriteErr() != nil {
		t.Fatalf("healthy tier reported a write error: %v", c.WriteErr())
	}

	// Simulate a failed write having been recorded, and check it is sticky:
	// a tier that has lost a write is permanently missing state, so a later
	// successful batch must NOT clear the flag.
	c.writeErr = errTestWrite
	c.bset([]byte("k2"), []byte("v2"))
	c.flushBatch()
	if c.WriteErr() == nil {
		t.Fatal("write error cleared by a later successful batch — a lost row stays lost")
	}
}

type testWriteErr struct{}

func (testWriteErr) Error() string { return "simulated cold write failure" }

var errTestWrite = testWriteErr{}
