package chain

import (
	"fmt"

	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// processHeaderExtensions ports hive::chain::database::process_header_extensions
// (libraries/chain/database_witness.cpp). Each block header may carry a
// version vote and/or a hardfork_version_vote in its extensions; both are
// applied to the producing witness's record so the witness_schedule's
// median tally and the hardfork_property tally see the witness's current
// position.
//
// The state_stamp extension (tag 3, hived 18045a0ee) is refused before HF29
// ("State stamp block extension is not allowed before HF29"); from HF29 the
// block must carry it exactly when state stamps are pending, with their
// merkle root, after which the pending list is reset (checkStateStampExtension).
// hived runs this before the block's transactions and before
// rc finalize_block publishes, so a stamp published in block N is carried
// by block N+1 at the earliest.
//
// hf is the hardfork set the block is applied under (has_hardfork at this
// block: a fork activating at the end of this block is not in it).
func processHeaderExtensions(s *state.Session, hdr *block.SignedHeader, hf hardfork.Set) error {
	exts, err := block.ParseHeaderExtensions(hdr.Extensions)
	if err != nil {
		return fmt.Errorf("process_header_extensions: %w", err)
	}

	// ParseHeaderExtensions already dropped every repeated tag, as hived's
	// flat_set does, so at most one state stamp survives here.
	var stamp *[20]byte
	for i := range exts {
		if exts[i].Tag == block.HeaderExtStateStamp {
			if !hf.Has(hardfork.HF29) {
				return fmt.Errorf("process_header_extensions: State stamp block extension is not allowed before HF29")
			}
			stamp = &exts[i].StateMerkleRoot
		}
	}

	if err := applyWitnessVersionExtensions(s, hdr.Witness, exts); err != nil {
		return err
	}

	if hf.Has(hardfork.HF29) {
		if err := checkStateStampExtension(s, stamp); err != nil {
			return fmt.Errorf("process_header_extensions: %w", err)
		}
	}
	return nil
}

// applyWitnessVersionExtensions records the producer's version and
// hardfork vote from the block's extensions.
func applyWitnessVersionExtensions(s *state.Session, witness string, exts []block.HeaderExtension) error {
	if len(exts) == 0 {
		return nil
	}
	w, err := objects.GetWitness(s, witness)
	if err != nil {
		// Witness not found is non-fatal here — the schedule check
		// upstream will surface it. Skip the update silently rather
		// than abort.
		return nil
	}
	mutated := false
	for _, ext := range exts {
		switch ext.Tag {
		case block.HeaderExtVoid, block.HeaderExtStateStamp:
			// no witness update
		case block.HeaderExtVersion:
			if w.RunningVersion != ext.Version {
				w.RunningVersion = ext.Version
				mutated = true
			}
		case block.HeaderExtHardforkVersionVote:
			if w.HardforkVersionVote != ext.HardforkVersion ||
				w.HardforkTimeVote != ext.HardforkVersionTime {
				w.HardforkVersionVote = ext.HardforkVersion
				w.HardforkTimeVote = ext.HardforkVersionTime
				mutated = true
			}
		}
	}
	if mutated {
		objects.PutWitness(s, &w)
	}
	return nil
}
