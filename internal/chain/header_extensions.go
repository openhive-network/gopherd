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
// ("State stamp block extension is not allowed before HF29").
//
// hf is the hardfork set the block is applied under (has_hardfork at this
// block: a fork activating at the end of this block is not in it).
func processHeaderExtensions(s *state.Session, hdr *block.SignedHeader, hf hardfork.Set) error {
	if len(hdr.Extensions) == 0 {
		return nil
	}
	exts, err := block.ParseHeaderExtensions(hdr.Extensions)
	if err != nil {
		return fmt.Errorf("process_header_extensions: %w", err)
	}
	if len(exts) == 0 {
		return nil
	}

	for _, ext := range exts {
		if ext.Tag == block.HeaderExtStateStamp && !hf.Has(hardfork.HF29) {
			return fmt.Errorf("process_header_extensions: State stamp block extension is not allowed before HF29")
		}
	}

	w, err := objects.GetWitness(s, hdr.Witness)
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
