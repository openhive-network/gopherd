package api

import (
	"encoding/json"
	"testing"

	"gopherd/internal/protocol/block"
)

// The two JSON forms hived serves for block_header_extensions. The version case
// is block 110746189 as api.hive.blog returned it (block_api and condenser_api);
// the vote case follows FC_REFLECT(hardfork_version_vote, (hf_version)(hf_time)).
func TestHeaderExtensionsJSONMatchesHived(t *testing.T) {
	raw := block.AppendHeaderExtensions(nil, []block.HeaderExtension{
		{Tag: block.HeaderExtVersion, Version: 0x011C0008},
		{Tag: block.HeaderExtHardforkVersionVote, HardforkVersion: 0x011C0000, HardforkVersionTime: 1763557200},
	})
	for _, c := range []struct {
		legacy bool
		want   string
	}{
		{false, `[{"type":"version","value":"1.28.8"},{"type":"hardfork_version_vote","value":{"hf_time":"2025-11-19T13:00:00","hf_version":"1.28.0"}}]`},
		{true, `[[1,"1.28.8"],[2,{"hf_time":"2025-11-19T13:00:00","hf_version":"1.28.0"}]]`},
	} {
		got, err := headerExtensionsJSON(raw, c.legacy)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(got)
		if string(b) != c.want {
			t.Errorf("legacy=%v:\n got %s\nwant %s", c.legacy, b, c.want)
		}
	}
	// HF29 state_stamp (tag 3): FC_REFLECT(state_stamp, (merkle_root)) with
	// the ripemd160 in hex; condenser's legacy variant lists it at tag 3 too.
	var root [20]byte
	for i := range root {
		root[i] = byte(i + 1)
	}
	raw = block.AppendHeaderExtensions(nil, []block.HeaderExtension{
		{Tag: block.HeaderExtStateStamp, StateMerkleRoot: root},
	})
	for _, c := range []struct {
		legacy bool
		want   string
	}{
		{false, `[{"type":"state_stamp","value":{"merkle_root":"0102030405060708090a0b0c0d0e0f1011121314"}}]`},
		{true, `[[3,{"merkle_root":"0102030405060708090a0b0c0d0e0f1011121314"}]]`},
	} {
		got, err := headerExtensionsJSON(raw, c.legacy)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(got)
		if string(b) != c.want {
			t.Errorf("state_stamp legacy=%v:\n got %s\nwant %s", c.legacy, b, c.want)
		}
	}
	// The common case: an empty set renders [], not null.
	got, err := headerExtensionsJSON([]byte{0}, true)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got); string(b) != "[]" {
		t.Errorf("empty: got %s, want []", b)
	}
}
