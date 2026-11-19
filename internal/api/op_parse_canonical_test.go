package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestParseOpCanonicalizesFlatContainers pins that an operation broadcast as
// JSON gets fc's from_variant semantics, which insert flat_set and flat_map
// elements like its binary unpack: sorted, repeats dropped (first wins).
func TestParseOpCanonicalizesFlatContainers(t *testing.T) {
	o, err := parseOp(json.RawMessage(`["remove_proposal",{"proposal_owner":"alice","proposal_ids":[7,2,7]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := o.AsRemoveProposal().ProposalIDs, []int64{2, 7}; !reflect.DeepEqual(got, want) {
		t.Errorf("proposal_ids %v, want %v", got, want)
	}
	o, err = parseOp(json.RawMessage(`["account_update",{"account":"alice","posting":{"weight_threshold":1,"account_auths":[["zed",1],["amy",2],["zed",3]],"key_auths":[]},"memo_key":"STM6LLegbAgLAy28EHrffBVuANFWcFgmqRMW13wBmTExqFE9SCkg4","json_metadata":""}]`))
	if err != nil {
		t.Fatal(err)
	}
	p := o.AsAccountUpdate().Posting
	if p == nil || len(p.AccountAuths) != 2 || p.AccountAuths[0].Account != "amy" || p.AccountAuths[1].Account != "zed" || p.AccountAuths[1].Weight != 1 {
		t.Errorf("posting account_auths %+v, want amy:2 then zed:1", p)
	}
}
