package witness

import (
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// hardforkVersionOf is the packed hardfork_version for an id — a version with
// the revision forced to zero, which is what the hardfork_property object and
// the votes hold.
func hardforkVersionOf(id hardfork.ID) uint32 { return hardfork.PackedVersion(id) }

// binaryHardforkVersion is this build's blockchain version with the revision
// masked off, i.e. the newest fork this binary knows how to apply.
func binaryHardforkVersion() uint32 { return network.BlockchainVersionNum &^ 0xFFFF }

func versionVoteFixture(t *testing.T, w objects.Witness, hpo objects.HardforkPropertyObject) *Plugin {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := state.NewSession(db)
	objects.PutWitness(s, &w)
	objects.PutHardforkPropertyObject(s, &hpo)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	return &Plugin{State: db}
}

func extensionsFor(t *testing.T, p *Plugin, name string) []block.HeaderExtension {
	t.Helper()
	return p.headerVersionExtensions(state.NewSession(p.State), name)
}

func findExt(exts []block.HeaderExtension, tag block.HeaderExtensionTag) (block.HeaderExtension, bool) {
	for _, e := range exts {
		if e.Tag == tag {
			return e, true
		}
	}
	return block.HeaderExtension{}, false
}

// TestVersionExtensionEmittedWhenWitnessIsStale covers the half of
// block_producer::adjust_hardfork_version_vote that matters for liveness.
//
// validate_block rejects a block whose producer's running_version is below
// current_hardfork_version, and a witness that has NEVER produced has 0. The
// version extension is what writes running_version, so without it gopherd
// could not bootstrap a new witness past HF5 — its own blocks would be
// rejected, by us and by the network.
func TestVersionExtensionEmittedWhenWitnessIsStale(t *testing.T) {
	appliedHF := hardfork.HF28

	t.Run("a witness that has never produced", func(t *testing.T) {
		p := versionVoteFixture(t,
			objects.Witness{Owner: auth.AccountName("alice"), RunningVersion: 0},
			objects.HardforkPropertyObject{
				LastHardfork:           uint32(appliedHF),
				CurrentHardforkVersion: hardforkVersionOf(appliedHF),
			})
		ext, ok := findExt(extensionsFor(t, p, "alice"), block.HeaderExtVersion)
		if !ok {
			t.Fatal("no version extension: this witness's blocks would be rejected for running_version 0")
		}
		if ext.Version != network.BlockchainVersionNum {
			t.Errorf("version: got %#x, want %#x", ext.Version, network.BlockchainVersionNum)
		}
	})

	t.Run("a witness already reporting this version", func(t *testing.T) {
		p := versionVoteFixture(t,
			objects.Witness{
				Owner:               auth.AccountName("alice"),
				RunningVersion:      network.BlockchainVersionNum,
				HardforkVersionVote: binaryHardforkVersion(),
				HardforkTimeVote:    hardfork.ActivationTime(appliedHF),
			},
			objects.HardforkPropertyObject{
				LastHardfork:           uint32(appliedHF),
				CurrentHardforkVersion: hardforkVersionOf(appliedHF),
			})
		if _, ok := findExt(extensionsFor(t, p, "alice"), block.HeaderExtVersion); ok {
			t.Error("version extension emitted when the chain already agrees; hived emits none")
		}
	})
}

// TestHardforkVoteOnlyWhenBinaryIsAhead covers the second entry. The majority
// of these votes is what schedules a hardfork, so a vote for a fork this
// binary cannot apply would help activate one we then fail to follow.
func TestHardforkVoteOnlyWhenBinaryIsAhead(t *testing.T) {
	// The chain sits at exactly the newest fork this binary knows: nothing to
	// vote for. This is mainnet's steady state, where HIVE_BLOCKCHAIN_VERSION
	// is 1.28.7 and HF28 is applied.
	t.Run("chain level with the binary", func(t *testing.T) {
		applied := hardfork.ID(binaryHardforkVersion() >> 16 & 0xFF)
		p := versionVoteFixture(t,
			objects.Witness{
				Owner:               auth.AccountName("alice"),
				RunningVersion:      network.BlockchainVersionNum,
				HardforkVersionVote: hardforkVersionOf(applied),
				HardforkTimeVote:    hardfork.ActivationTime(applied),
			},
			objects.HardforkPropertyObject{
				LastHardfork:           uint32(applied),
				CurrentHardforkVersion: hardforkVersionOf(applied),
			})
		if exts := extensionsFor(t, p, "alice"); len(exts) != 0 {
			t.Errorf("steady state must emit nothing, got %+v", exts)
		}
	})

	// The chain is behind this binary: vote for the NEXT fork after the last
	// applied one — not for the binary's own version, which differs whenever
	// more than one fork is pending.
	t.Run("chain two forks behind votes for the next one, not the newest", func(t *testing.T) {
		applied := hardfork.HF26
		p := versionVoteFixture(t,
			objects.Witness{Owner: auth.AccountName("alice"), RunningVersion: network.BlockchainVersionNum},
			objects.HardforkPropertyObject{
				LastHardfork:           uint32(applied),
				CurrentHardforkVersion: hardforkVersionOf(applied),
			})
		ext, ok := findExt(extensionsFor(t, p, "alice"), block.HeaderExtHardforkVersionVote)
		if !ok {
			t.Fatal("expected a hardfork vote: the chain is behind this binary")
		}
		next := applied + 1
		if ext.HardforkVersion != hardforkVersionOf(next) {
			t.Errorf("voted for %#x, want HF%d (%#x) — voting for the binary's own version skips pending forks",
				ext.HardforkVersion, next, hardforkVersionOf(next))
		}
		if ext.HardforkVersionTime != hardfork.ActivationTime(next) {
			t.Errorf("vote time: got %d, want %d", ext.HardforkVersionTime, hardfork.ActivationTime(next))
		}
	})

	// A vote left behind by a newer binary that used to produce for this
	// witness gets walked back: a vote we cannot honour is worse than none.
	t.Run("a vote past what this binary knows is walked back", func(t *testing.T) {
		applied := hardfork.ID(binaryHardforkVersion() >> 16 & 0xFF)
		p := versionVoteFixture(t,
			objects.Witness{
				Owner:               auth.AccountName("alice"),
				RunningVersion:      network.BlockchainVersionNum,
				HardforkVersionVote: hardforkVersionOf(applied) + (1 << 16), // one fork past us
			},
			objects.HardforkPropertyObject{
				LastHardfork:           uint32(applied),
				CurrentHardforkVersion: hardforkVersionOf(applied),
			})
		ext, ok := findExt(extensionsFor(t, p, "alice"), block.HeaderExtHardforkVersionVote)
		if !ok {
			t.Fatal("expected the stale future vote to be walked back")
		}
		if ext.HardforkVersion != hardforkVersionOf(applied) {
			t.Errorf("walked back to %#x, want the applied fork %#x", ext.HardforkVersion, hardforkVersionOf(applied))
		}
	})
}

// TestHardforkVoteMasksRevision is the trap this comparison sets. A
// hardfork_version has its revision forced to zero, but HIVE_BLOCKCHAIN_VERSION
// carries one (1.28.7). Comparing raw v_nums would make 1.28.7 look strictly
// newer than hardfork 1.28 and emit a vote for a fork that does not exist.
func TestHardforkVoteMasksRevision(t *testing.T) {
	if network.BlockchainVersionRevision == 0 {
		t.Skip("this build's version has no revision; the masking bug is unreachable here")
	}
	applied := hardfork.ID(binaryHardforkVersion() >> 16 & 0xFF)
	p := versionVoteFixture(t,
		objects.Witness{
			Owner:               auth.AccountName("alice"),
			RunningVersion:      network.BlockchainVersionNum,
			HardforkVersionVote: hardforkVersionOf(applied),
			HardforkTimeVote:    hardfork.ActivationTime(applied),
		},
		objects.HardforkPropertyObject{
			LastHardfork:           uint32(applied),
			CurrentHardforkVersion: hardforkVersionOf(applied),
		})
	if ext, ok := findExt(extensionsFor(t, p, "alice"), block.HeaderExtHardforkVersionVote); ok {
		t.Errorf("voted for %#x with the chain already at this binary's newest fork — "+
			"the revision byte leaked into the comparison", ext.HardforkVersion)
	}
}

// TestVersionExtensionsAbsentWitnessIsSafe — production must not panic or
// invent extensions for a witness the state does not know.
func TestVersionExtensionsAbsentWitnessIsSafe(t *testing.T) {
	p := versionVoteFixture(t,
		objects.Witness{Owner: auth.AccountName("alice")},
		objects.HardforkPropertyObject{LastHardfork: 28, CurrentHardforkVersion: hardforkVersionOf(hardfork.HF28)})
	if exts := extensionsFor(t, p, "nobody"); len(exts) != 0 {
		t.Errorf("got %+v for an unknown witness, want none", exts)
	}
}
