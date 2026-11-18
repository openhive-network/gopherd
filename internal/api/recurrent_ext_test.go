package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"gopherd/internal/protocol/op"
)

// hived 6e9ce8d7e (#813): condenser_api emits recurrent_transfer extensions as
// [[1,{"pair_id":3}]]; the HF26 form is {"type":"recurrent_transfer_pair_id",...}.
// Both must parse, and render back the same way.
func TestRecurrentTransferExtensionsJSON(t *testing.T) {
	want := []byte{1, 1, 3} // one extension, tag 1, pair_id 3
	for _, ext := range []string{
		`[[1,{"pair_id":3}]]`,
		`[["recurrent_transfer_pair_id",{"pair_id":3}]]`,
		`[{"type":"recurrent_transfer_pair_id","value":{"pair_id":3}}]`,
	} {
		body := `["recurrent_transfer",{"from":"alice","to":"bob","amount":"1.000 HIVE","memo":"","recurrence":24,"executions":2,"extensions":` + ext + `}]`
		o, err := parseOp(json.RawMessage(body))
		if err != nil {
			t.Fatalf("%s: %v", ext, err)
		}
		if got := o.AsRecurrentTransfer().Extensions; !bytes.Equal(got, want) {
			t.Fatalf("%s: encoded %x, want %x", ext, got, want)
		}
		legacy, _ := json.Marshal(opToJSON(o, true))
		if !bytes.Contains(legacy, []byte(`"extensions":[[1,{"pair_id":3}]]`)) {
			t.Fatalf("legacy render: %s", legacy)
		}
		modern, _ := json.Marshal(opToVariant(o, false))
		if !bytes.Contains(modern, []byte(`"extensions":[{"type":"recurrent_transfer_pair_id","value":{"pair_id":3}}]`)) {
			t.Fatalf("HF26 render: %s", modern)
		}
	}
	// No extensions still renders as [].
	o, err := parseOp(json.RawMessage(`["recurrent_transfer",{"from":"alice","to":"bob","amount":"1.000 HIVE","memo":"","recurrence":24,"executions":2,"extensions":[]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(opToJSON(o, true)); !bytes.Contains(b, []byte(`"extensions":[]`)) {
		t.Fatalf("empty extensions render: %s", b)
	}
	if o.Tag != op.TagRecurrentTransfer {
		t.Fatal("wrong op")
	}
}
