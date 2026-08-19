package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTxParamAcceptsNamedAndPositional pins both call shapes for
// broadcast_transaction. hived's network_broadcast_api uses the NAMED form
// {"trx": {...}}; condenser_api uses POSITIONAL [tx].
//
// Only the positional form used to work. The named form fell through to the
// "whole params is the transaction" fallback, so the envelope was parsed as the
// transaction and every field read empty — reported as
// `expiration: parsing time ""`, which blames the timestamp rather than the
// envelope and sent me looking in the wrong place.
func TestTxParamAcceptsNamedAndPositional(t *testing.T) {
	const inner = `{"ref_block_num":1,"ref_block_prefix":2,"expiration":"2026-08-19T20:00:00","operations":[],"signatures":[]}`

	for _, c := range []struct{ name, params string }{
		{"named", `{"trx":` + inner + `}`},
		{"positional", `[` + inner + `]`},
		{"bare", inner},
	} {
		got, err := txParam(json.RawMessage(c.params), true)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !strings.Contains(string(got), `"expiration"`) {
			t.Errorf("%s: extracted %s, want the inner transaction", c.name, got)
		}
	}
}
