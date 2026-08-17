package api

import (
	"encoding/json"
	"testing"

	"gopherd/internal/protocol/op"
)

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"From": "from", "RequestID": "request_id", "JSONMetadata": "json_metadata",
		"HBDAmount": "hbd_amount", "URL": "url", "Amount": "amount", "MemoKey": "memo_key",
		"WeightThreshold": "weight_threshold",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q)=%q want %q", in, got, want)
		}
	}
}

func TestOpToJSONTransfer(t *testing.T) {
	o := op.FromOperation(op.Transfer{From: "alice", To: "bob", Amount: hiveAsset(1000), Memo: "hi"})

	// Legacy form: ["transfer", {from,to,amount:"1.000 HIVE",memo}].
	legacy := opToJSON(o, true)
	b, _ := json.Marshal(legacy)
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil || len(arr) != 2 {
		t.Fatalf("legacy op should be a 2-array: %s", b)
	}
	var name string
	json.Unmarshal(arr[0], &name)
	if name != "transfer" {
		t.Fatalf("op name = %q want transfer", name)
	}
	var fields map[string]interface{}
	json.Unmarshal(arr[1], &fields)
	if fields["from"] != "alice" || fields["to"] != "bob" || fields["memo"] != "hi" {
		t.Fatalf("transfer fields wrong: %v", fields)
	}
	if fields["amount"] != "1.000 HIVE" {
		t.Fatalf("legacy amount = %v want '1.000 HIVE'", fields["amount"])
	}

	// Modern variant form: {type:"transfer_operation", value:{amount:{nai...}}}.
	variant := opToVariant(o, false)
	vb, _ := json.Marshal(variant)
	var vm map[string]interface{}
	json.Unmarshal(vb, &vm)
	if vm["type"] != "transfer_operation" {
		t.Fatalf("variant type = %v", vm["type"])
	}
	val := vm["value"].(map[string]interface{})
	amt := val["amount"].(map[string]interface{})
	if amt["nai"] != "@@000000021" || amt["amount"] != "1000" {
		t.Fatalf("NAI amount wrong: %v", amt)
	}
}

func TestOpParseRoundTrip(t *testing.T) {
	orig := op.Transfer{From: "alice", To: "bob", Amount: hiveAsset(2500), Memo: "x"}
	// legacy render → parse back.
	j, _ := json.Marshal(opToJSON(op.FromOperation(orig), true))
	got, err := parseOp(j)
	if err != nil {
		t.Fatalf("parseOp: %v", err)
	}
	tr := got.Operation().(op.Transfer)
	if tr.From != "alice" || tr.To != "bob" || tr.Memo != "x" || tr.Amount.Amount != 2500 {
		t.Fatalf("round-trip mismatch: %+v", tr)
	}
	// modern variant render → parse back.
	jv, _ := json.Marshal(opToVariant(op.FromOperation(orig), false))
	got2, err := parseOp(jv)
	if err != nil {
		t.Fatalf("parseOp variant: %v", err)
	}
	if got2.Operation().(op.Transfer).Amount.Amount != 2500 {
		t.Fatal("variant round-trip amount mismatch")
	}
}
