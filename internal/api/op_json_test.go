package api

import (
	"encoding/json"
	"testing"

	"hived-go/internal/protocol/op"
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
