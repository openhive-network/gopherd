package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
)

// delegate_rc (tag 37, HF29) in both API forms: legacy ["delegate_rc",{...}]
// and HF26 {"type":"delegate_rc_operation","value":{...}}, field names from
// hived's FC_REFLECT (from, delegatees, max_rc, extensions).
func TestDelegateRCJSON(t *testing.T) {
	orig := op.DelegateRC{From: "alice", Delegatees: []auth.AccountName{"bob", "carol"}, MaxRC: 5000000}
	j, _ := json.Marshal(opToJSON(op.FromOperation(orig), true))
	const wantLegacy = `["delegate_rc",{"delegatees":["bob","carol"],"extensions":[],"from":"alice","max_rc":5000000}]`
	if string(j) != wantLegacy {
		t.Fatalf("legacy: %s\nwant   %s", j, wantLegacy)
	}
	jv, _ := json.Marshal(opToVariant(op.FromOperation(orig), false))
	const wantVariant = `{"type":"delegate_rc_operation","value":{"delegatees":["bob","carol"],"extensions":[],"from":"alice","max_rc":5000000}}`
	if string(jv) != wantVariant {
		t.Fatalf("variant: %s\nwant    %s", jv, wantVariant)
	}
	for _, raw := range [][]byte{j, jv} {
		got, err := parseOp(raw)
		if err != nil {
			t.Fatalf("parseOp %s: %v", raw, err)
		}
		d := got.Operation().(op.DelegateRC)
		if d.From != "alice" || len(d.Delegatees) != 2 || d.Delegatees[1] != "carol" || d.MaxRC != 5000000 {
			t.Fatalf("parsed %s as %+v", raw, d)
		}
		if !bytes.Equal(op.AppendOperation(nil, d), op.AppendOperation(nil, orig)) {
			t.Fatalf("parsed %s does not serialize like the original", raw)
		}
	}
}
