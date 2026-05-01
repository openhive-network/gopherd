package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// identKey extracts type+identifier; verify the few cases used in
// production (account/witness/comment/dgpo) all return stable keys.
func TestSnapshotDiffIdentKey(t *testing.T) {
	cases := []struct {
		line, want string
	}{
		{`{"_type":"account","name":"alice","balance":1}`, "account/alice"},
		{`{"_type":"witness","owner":"bob","url":"x"}`, "witness/bob"},
		{`{"_type":"comment","author":"a","permlink":"p"}`, "comment/a/p"},
		{`{"_type":"dgpo","head_block_number":42}`, "dgpo"},
	}
	for _, c := range cases {
		got := identKey([]byte(c.line))
		if got != c.want {
			t.Errorf("%s → %q, want %q", c.line, got, c.want)
		}
	}
}

// diffJSON: identical objects yield "" (no diff); different scalar
// fields are reported.
func TestSnapshotDiffJSON(t *testing.T) {
	skip := map[string]bool{}
	a := []byte(`{"_type":"account","name":"alice","balance":100}`)
	if d := diffJSON(a, a, skip); d != "" {
		t.Fatalf("identical lines should diff empty, got %q", d)
	}
	b := []byte(`{"_type":"account","name":"alice","balance":200}`)
	d := diffJSON(a, b, skip)
	if !strings.Contains(d, "balance") {
		t.Fatalf("expected diff to mention balance, got %q", d)
	}
}

// diffJSON respects the skip set — fields in `skip` are ignored.
func TestSnapshotDiffSkipFields(t *testing.T) {
	skip := map[string]bool{"rc_mana_current": true}
	a := []byte(`{"_type":"account","name":"alice","rc_mana_current":100,"balance":50}`)
	b := []byte(`{"_type":"account","name":"alice","rc_mana_current":200,"balance":50}`)
	if d := diffJSON(a, b, skip); d != "" {
		t.Fatalf("skipped field shouldn't generate diff, got %q", d)
	}
}

// rawEq sanity.
func TestSnapshotDiffRawEq(t *testing.T) {
	a := json.RawMessage(`123`)
	b := json.RawMessage(`123`)
	c := json.RawMessage(`456`)
	if !rawEq(a, b) {
		t.Fatal("equal raws should match")
	}
	if rawEq(a, c) {
		t.Fatal("unequal raws should not match")
	}
}
