package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestHardforkPropertyObjectGenesis ports the genesis-state portion of
// block_tests.cpp::hardfork_test. At genesis ProcessedHardforks contains
// only the genesis time, LastHardfork == 0, and CurrentHardforkVersion
// is HF0 (packed v_num = 0).
func TestHardforkPropertyObjectGenesis(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	hpo, err := objects.GetHardforkPropertyObject(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetHardforkPropertyObject: %v", err)
	}
	if hpo.LastHardfork != 0 {
		t.Errorf("genesis LastHardfork: got %d, want 0", hpo.LastHardfork)
	}
	if hpo.CurrentHardforkVersion != hardfork.PackedVersion(hardfork.HF0) {
		t.Errorf("genesis CurrentHardforkVersion: got %d, want %d",
			hpo.CurrentHardforkVersion, hardfork.PackedVersion(hardfork.HF0))
	}
	if len(hpo.ProcessedHardforks) != 1 {
		t.Errorf("genesis ProcessedHardforks: got len=%d, want 1",
			len(hpo.ProcessedHardforks))
	}
}

// TestHardforkPropertyObjectAdvances ports the "generate blocks up to HF
// time, hardfork applied" portion of hardfork_test. Activating HF1
// pushes a new entry into ProcessedHardforks, bumps LastHardfork to 1,
// and updates CurrentHardforkVersion to HF1's packed version.
func TestHardforkPropertyObjectAdvances(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF1)

	hpo, err := objects.GetHardforkPropertyObject(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetHardforkPropertyObject: %v", err)
	}
	if hpo.LastHardfork != uint32(hardfork.HF1) {
		t.Errorf("post-HF1 LastHardfork: got %d, want %d",
			hpo.LastHardfork, hardfork.HF1)
	}
	if hpo.CurrentHardforkVersion != hardfork.PackedVersion(hardfork.HF1) {
		t.Errorf("post-HF1 CurrentHardforkVersion: got %d (decoded major.minor=%d.%d), want %d",
			hpo.CurrentHardforkVersion,
			hpo.CurrentHardforkVersion>>24, (hpo.CurrentHardforkVersion>>16)&0xff,
			hardfork.PackedVersion(hardfork.HF1))
	}
	if len(hpo.ProcessedHardforks) != 2 {
		t.Errorf("post-HF1 ProcessedHardforks: got len=%d, want 2 (genesis + HF1)",
			len(hpo.ProcessedHardforks))
	}
}

// TestHardforkAppliedOnlyOnce verifies that re-running activation for an
// already-applied hardfork is a no-op (idempotent). Hived's
// process_hardforks compares last_hardfork against next_hardfork to
// avoid double-firing; we mirror that with the LastHardfork >= id gate
// in bumpHardforkPropertyObject.
func TestHardforkAppliedOnlyOnce(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF1)

	// Snapshot post-HF1 state.
	beforeLen := len(mustHPO(t, f).ProcessedHardforks)

	// Re-running HF1 activation must be a no-op.
	s := state.NewSession(f.State)
	applyHardforkInTest(t, s, hardfork.HF1)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	hpo := mustHPO(t, f)
	if len(hpo.ProcessedHardforks) != beforeLen {
		t.Errorf("HF1 re-activation should be idempotent: ProcessedHardforks went from %d → %d",
			beforeLen, len(hpo.ProcessedHardforks))
	}
}

// TestHardforkSequentialActivation activates HF1 → HF7 in sequence and
// verifies each step bumps the property object as expected. Mirrors the
// "Testing hardfork is only applied once" branch of C++ hardfork_test.
func TestHardforkSequentialActivation(t *testing.T) {
	f := New(t)
	for _, id := range []hardfork.ID{hardfork.HF1, hardfork.HF2, hardfork.HF3, hardfork.HF4, hardfork.HF5, hardfork.HF6, hardfork.HF7} {
		f.HardforksUpTo(id)
		hpo := mustHPO(t, f)
		if hpo.LastHardfork != uint32(id) {
			t.Fatalf("after activating HF%d: LastHardfork=%d", id, hpo.LastHardfork)
		}
		// processed_hardforks length = id + 1 (genesis at index 0).
		if uint32(len(hpo.ProcessedHardforks)) != uint32(id)+1 {
			t.Fatalf("after HF%d: ProcessedHardforks len=%d, want %d",
				id, len(hpo.ProcessedHardforks), uint32(id)+1)
		}
		if hpo.CurrentHardforkVersion != hardfork.PackedVersion(id) {
			t.Fatalf("after HF%d: version=%#x, want %#x",
				id, hpo.CurrentHardforkVersion, hardfork.PackedVersion(id))
		}
	}
}

func mustHPO(t *testing.T, f *Fixture) objects.HardforkPropertyObject {
	t.Helper()
	hpo, err := objects.GetHardforkPropertyObject(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetHardforkPropertyObject: %v", err)
	}
	return hpo
}
