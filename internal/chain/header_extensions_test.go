package chain

import (
	"strings"
	"testing"

	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
)

// TestStateStampExtensionRefusedBeforeHF29 — hived's process_header_visitor
// asserts has_hardfork(HIVE_HARDFORK_1_29_STAMP_BLOCK_EXTENSION) on a
// state_stamp, so a pre-HF29 block carrying one is invalid even though the
// wire format now parses it.
func TestStateStampExtensionRefusedBeforeHF29(t *testing.T) {
	s := state.NewSession(state.OpenMem())
	hdr := block.SignedHeader{Header: block.Header{
		Witness: "alice",
		Extensions: block.AppendHeaderExtensions(nil, []block.HeaderExtension{
			{Tag: block.HeaderExtStateStamp, StateMerkleRoot: [20]byte{1}},
		}),
	}}
	pre := hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF28] + 1)
	if pre.Has(hardfork.HF29) {
		t.Skip("HF29 is scheduled in this build")
	}
	err := processHeaderExtensions(s, &hdr, pre)
	if err == nil || !strings.Contains(err.Error(), "not allowed before HF29") {
		t.Fatalf("pre-HF29 state stamp: err = %v", err)
	}
}
