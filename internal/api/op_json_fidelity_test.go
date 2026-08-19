package api

import (
	"encoding/json"
	"sort"
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
)

// TestOpJSONTimeFieldsAreISO pins that time_point_sec operation fields render
// as ISO-8601, not as raw numbers.
//
// They are plain uint32 in Go, so the reflection walk cannot distinguish them
// from a request id and emitted numbers. hived rejects that with "Month number
// is out of range 1..12" — an error about the value that is really about the
// type — and any consumer diffing our get_block output against hived's would
// see every escrow deadline and order expiry differ.
func TestOpJSONTimeFieldsAreISO(t *testing.T) {
	o := op.Ops(op.LimitOrderCreate{
		Owner: "alice", OrderID: 7,
		AmountToSell: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		MinToReceive: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Expiration:   1700000000,
	})[0]
	raw, err := json.Marshal(OpToJSON(o, true))
	if err != nil {
		t.Fatal(err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) != 2 {
		t.Fatalf("shape: %s", raw)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(arr[1], &fields); err != nil {
		t.Fatal(err)
	}
	got, _ := fields["expiration"].(string)
	if got != "2023-11-14T22:13:20" {
		t.Errorf("expiration = %v, want ISO-8601 string 2023-11-14T22:13:20", fields["expiration"])
	}
	// orderid is also uint32 and must stay a NUMBER. Asserting only "not a
	// string" was vacuous: the key was actually emitted as "order_id", so this
	// lookup returned nil, the type assertion failed, and the test passed while
	// the field name was wrong. Require the key to EXIST.
	v, ok := fields["orderid"]
	if !ok {
		t.Fatalf("no \"orderid\" field; got %v", keysOf(fields))
	}
	if _, isNum := v.(float64); !isNum {
		t.Errorf("orderid = %#v, want a number (only time_point_sec fields convert)", v)
	}
}

// TestOpJSONFieldNamesMatchHived pins the operation fields whose JSON names are
// NOT the snake_case of the Go field. hived uses "orderid"/"requestid" without
// an underscore for market and convert operations but "request_id" WITH one for
// savings operations (hive_operations.hpp:1119-1120 vs 1196).
//
// Getting these wrong does not produce a field error: hived fails to parse the
// operation and reports "missing required active authority", which points at
// the signature rather than the payload.
func TestOpJSONFieldNamesMatchHived(t *testing.T) {
	hive := asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}}
	hbd := asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}}
	for _, c := range []struct {
		op   op.Op
		want string
		bad  string
	}{
		{op.Ops(op.LimitOrderCreate{Owner: "a", OrderID: 1, AmountToSell: hive, MinToReceive: hbd})[0], "orderid", "order_id"},
		{op.Ops(op.LimitOrderCancel{Owner: "a", OrderID: 1})[0], "orderid", "order_id"},
		{op.Ops(op.Convert{Owner: "a", RequestID: 1, Amount: hbd})[0], "requestid", "request_id"},
		{op.Ops(op.CollateralizedConvert{Owner: "a", RequestID: 1, Amount: hive})[0], "requestid", "request_id"},
		// Savings really does use the underscored form — not an oversight.
		{op.Ops(op.TransferFromSavings{From: "a", RequestID: 1, To: "b", Amount: hive})[0], "request_id", "requestid"},
		{op.Ops(op.CancelTransferFromSavings{From: "a", RequestID: 1})[0], "request_id", "requestid"},
	} {
		raw, _ := json.Marshal(OpToJSON(c.op, true))
		var arr []json.RawMessage
		json.Unmarshal(raw, &arr)
		var fields map[string]interface{}
		json.Unmarshal(arr[1], &fields)
		if _, ok := fields[c.want]; !ok {
			t.Errorf("%s: missing %q (got %v)", c.op.Tag.Name(), c.want, keysOf(fields))
		}
		if _, ok := fields[c.bad]; ok {
			t.Errorf("%s: emitted %q, hived expects %q", c.op.Tag.Name(), c.bad, c.want)
		}
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOpJSONEmptyExtensionsIsArray — a single 0x00 is the fc encoding of an
// EMPTY extensions list, not a one-byte blob. Rendering it as "00" made hived
// reject operations with "Invalid cast from string_type to Array".
func TestOpJSONEmptyExtensionsIsArray(t *testing.T) {
	o := op.Ops(op.ClaimAccount{
		Creator: "alice",
		Fee:     asset.Asset{Amount: 0, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		// canonical empty extensions
		Extensions: []byte{0x00},
	})[0]
	raw, _ := json.Marshal(OpToJSON(o, true))
	var arr []json.RawMessage
	json.Unmarshal(raw, &arr)
	var fields map[string]interface{}
	json.Unmarshal(arr[1], &fields)
	ext, ok := fields["extensions"].([]interface{})
	if !ok {
		t.Fatalf("extensions = %#v, want a JSON array", fields["extensions"])
	}
	if len(ext) != 0 {
		t.Errorf("extensions = %v, want empty", ext)
	}
}
