package witness

import (
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state/objects"
)

// TestHardforkReleaseConsistency pins the HF29 release checklist in
// hardfork.go: the binary may not claim a hardfork version whose activation
// time is still the year-2100 placeholder, and may not carry a real time for
// a fork newer than its version. Either half alone would make this witness
// vote, or refuse to vote, for a fork the network schedules differently.
func TestHardforkReleaseConsistency(t *testing.T) {
	binaryHF := binaryHardforkVersion()
	for id := hardfork.ID(1); id <= hardfork.NumHardforks; id++ {
		known := binaryHF >= hardfork.PackedVersion(id)
		placeholder := hardfork.HasPlaceholderTime(id)
		switch {
		case known && placeholder:
			t.Fatalf("HF%d: BlockchainVersion %d.%d claims the fork but MainnetActivationTime[HF%d] is still the placeholder — set the real HIVE_HARDFORK_1_%d_TIME with the version bump",
				id, network.BlockchainVersionMajor, network.BlockchainVersionHardfork, id, id)
		case !known && !placeholder && id > hardfork.HF28:
			t.Fatalf("HF%d: MainnetActivationTime is set but BlockchainVersion %d.%d does not claim the fork — bump the version with the time",
				id, network.BlockchainVersionMajor, network.BlockchainVersionHardfork)
		}
	}
}

// TestNoVoteForAPlaceholderTime: a hardfork_version_vote carrying the
// year-2100 sentinel is not a vote for HF29, it is noise hived never counts.
// The hpo here is deliberately behind the binary so the voting branch runs
// and asks for HF29, whose time is the placeholder.
func TestNoVoteForAPlaceholderTime(t *testing.T) {
	if !hardfork.HasPlaceholderTime(hardfork.HF29) {
		t.Skip("HF29 has a real activation time in this build")
	}
	p := versionVoteFixture(t,
		objects.Witness{Owner: auth.AccountName("alice"), RunningVersion: network.BlockchainVersionNum},
		objects.HardforkPropertyObject{
			LastHardfork:           uint32(hardfork.HF28),
			CurrentHardforkVersion: hardforkVersionOf(hardfork.HF27), // behind the binary: the vote branch runs
		})
	if ext, ok := findExt(extensionsFor(t, p, "alice"), block.HeaderExtHardforkVersionVote); ok {
		t.Fatalf("voted %#x at %d; a placeholder time must not be voted", ext.HardforkVersion, ext.HardforkVersionTime)
	}
}
