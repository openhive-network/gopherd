package op

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
)

// TestDelegateRCWireFormat pins tag 37 to hived's delegate_rc_operation
// (7cc13d0af): FC_REFLECT( delegate_rc_operation, (from)(delegatees)(max_rc)
// (extensions) ) packed by fc::raw — the static_variant index as a varint,
// account_name_type as a length-prefixed string, flat_set as a varint count and
// its elements, int64 little-endian, and an empty extensions set as one zero.
func TestDelegateRCWireFormat(t *testing.T) {
	d := DelegateRC{From: "alice", Delegatees: []auth.AccountName{"bob", "carol"}, MaxRC: 1000}
	want := "25" + // tag 37
		"05" + hex.EncodeToString([]byte("alice")) +
		"02" + "03" + hex.EncodeToString([]byte("bob")) + "05" + hex.EncodeToString([]byte("carol")) +
		"e803000000000000" +
		"00"
	wire := AppendOperation(nil, d)
	if got := hex.EncodeToString(wire); got != want {
		t.Fatalf("wire:\n got %s\nwant %s", got, want)
	}
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if got.OpTag() != TagDelegateRC || TagDelegateRC.Name() != "delegate_rc" {
		t.Fatalf("tag %d %q", got.OpTag(), got.OpTag().Name())
	}
	d2 := got.(DelegateRC)
	if d2.From != d.From || !reflect.DeepEqual(d2.Delegatees, d.Delegatees) || d2.MaxRC != d.MaxRC {
		t.Fatalf("round-trip: got %+v want %+v", d2, d)
	}
	if !bytes.Equal(AppendOperation(nil, d2), wire) {
		t.Fatal("re-marshal differs")
	}

	// A negative max_rc is two's complement on the wire (validate refuses it).
	neg := AppendOperation(nil, DelegateRC{From: "a", Delegatees: []auth.AccountName{"b"}, MaxRC: -1})
	if !strings.HasSuffix(hex.EncodeToString(neg), "ffffffffffffffff00") {
		t.Fatalf("negative max_rc: %x", neg)
	}
}

// TestDelegateRCValidate is delegate_rc_operation::validate.
func TestDelegateRCValidate(t *testing.T) {
	names := func(n ...string) []auth.AccountName {
		out := make([]auth.AccountName, len(n))
		for i, s := range n {
			out[i] = auth.AccountName(s)
		}
		return out
	}
	many := make([]auth.AccountName, 0, MaxAccountsPerDelegationOp+1)
	for i := 0; i <= MaxAccountsPerDelegationOp; i++ {
		many = append(many, auth.AccountName("acct"+string(rune('a'+i/26))+string(rune('a'+i%26))))
	}
	for _, tc := range []struct {
		name string
		op   DelegateRC
		ok   bool
	}{
		{"valid", DelegateRC{From: "alice", Delegatees: names("bob"), MaxRC: 10}, true},
		{"remove", DelegateRC{From: "alice", Delegatees: names("bob"), MaxRC: 0}, true},
		{"bad from", DelegateRC{From: "A", Delegatees: names("bob"), MaxRC: 10}, false},
		{"no delegatees", DelegateRC{From: "alice", MaxRC: 10}, false},
		{"bad delegatee", DelegateRC{From: "alice", Delegatees: names("bob\t"), MaxRC: 10}, false},
		{"self", DelegateRC{From: "alice", Delegatees: names("bob", "alice"), MaxRC: 10}, false},
		{"negative", DelegateRC{From: "alice", Delegatees: names("bob"), MaxRC: -1}, false},
		{"101 delegatees", DelegateRC{From: "alice", Delegatees: many, MaxRC: 10}, false},
		// flat_set: a duplicate is one delegatee, so 100 distinct + 1 repeat fits
		{"100 + duplicate", DelegateRC{From: "alice", Delegatees: append(append([]auth.AccountName{}, many[:100]...), many[0]), MaxRC: 10}, true},
	} {
		err := tc.op.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if got := (DelegateRC{Delegatees: names("carol", "bob", "carol")}).SortedDelegatees(); !reflect.DeepEqual(got, names("bob", "carol")) {
		t.Errorf("SortedDelegatees = %v", got)
	}
}
