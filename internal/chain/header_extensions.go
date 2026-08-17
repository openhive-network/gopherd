package chain

import (
	"fmt"

	"gopherd/internal/protocol/block"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// processHeaderExtensions ports hive::chain::database::process_header_extensions
// (libraries/chain/database.cpp:2402-2422). Each block header may carry a
// version vote and/or a hardfork_version_vote in its extensions; both are
// applied to the producing witness's record so the witness_schedule's
// median tally and the hardfork_property tally see the witness's current
// position.
//
// Returns nil if there are no extensions, or if the producing witness is
// unknown (which would itself be a hard validation error caught upstream).
func processHeaderExtensions(s *state.Session, hdr *block.SignedHeader) error {
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
		case block.HeaderExtVoid:
			// no-op
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
