package op

import (
	"reflect"
	"testing"

	"gopherd/internal/fcraw"
)

func decode(t *testing.T, o Operation) Op {
	t.Helper()
	r := fcraw.NewReader(AppendOperation(nil, o))
	got, err := ReadOp(r)
	if err != nil {
		t.Fatal(err)
	}
	if r.Remaining() != 0 {
		t.Fatalf("%d trailing bytes", r.Remaining())
	}
	return got
}

// TestFlatContainersDecodeLikeFC pins fc's flat_set / flat_map unpack, which
// inserts each element: the decoded operation holds its sets and maps sorted
// by key with repeats dropped, a repeated map key keeping its FIRST value.
// hived only ever sees that form, so gopherd must too: a name listed twice in
// required_auths counts once against the membership limit, remove_proposal
// [5, 5] removes proposal 5 once instead of failing on the second, and a
// witness_set_properties with two "key" props is checked against the first.
func TestFlatContainersDecodeLikeFC(t *testing.T) {
	cj := decode(t, &CustomJSON{
		RequiredAuths:        []string{"carol", "alice", "carol"},
		RequiredPostingAuths: []string{"bob", "bob"},
		ID:                   "x",
		JSON:                 "{}",
	}).AsCustomJSON()
	if want := []string{"alice", "carol"}; !reflect.DeepEqual(cj.RequiredAuths, want) {
		t.Errorf("custom_json required_auths %v, want %v", cj.RequiredAuths, want)
	}
	if want := []string{"bob"}; !reflect.DeepEqual(cj.RequiredPostingAuths, want) {
		t.Errorf("custom_json required_posting_auths %v, want %v", cj.RequiredPostingAuths, want)
	}

	rp := decode(t, &RemoveProposal{ProposalOwner: "alice", ProposalIDs: []int64{5, 3, 5}}).AsRemoveProposal()
	if want := []int64{3, 5}; !reflect.DeepEqual(rp.ProposalIDs, want) {
		t.Errorf("remove_proposal ids %v, want %v", rp.ProposalIDs, want)
	}
	uv := decode(t, &UpdateProposalVotes{Voter: "alice", ProposalIDs: []int64{9, -1, 9}, Approve: true}).AsUpdateProposalVotes()
	if want := []int64{-1, 9}; !reflect.DeepEqual(uv.ProposalIDs, want) {
		t.Errorf("update_proposal_votes ids %v, want %v", uv.ProposalIDs, want)
	}

	ws := decode(t, &WitnessSetProperties{
		Owner: "alice",
		Props: []WitnessProperty{
			{Key: "url", Value: []byte("u")},
			{Key: "key", Value: []byte("first")},
			{Key: "key", Value: []byte("second")},
		},
	}).AsWitnessSetProperties()
	want := []WitnessProperty{{Key: "key", Value: []byte("first")}, {Key: "url", Value: []byte("u")}}
	if !reflect.DeepEqual(ws.Props, want) {
		t.Errorf("witness_set_properties props %+v, want %+v", ws.Props, want)
	}
}

// TestCommentOptionsKeepsFirstBeneficiaries pins that a second
// comment_payout_beneficiaries extension is dropped as fc's flat_set insert
// drops it (static_variant compares by tag), instead of being appended.
func TestCommentOptionsKeepsFirstBeneficiaries(t *testing.T) {
	ext := []byte{2} // two extensions, both tag 0
	for _, b := range []struct {
		name string
		w    uint16
	}{{"bob", 1000}, {"carol", 2000}} {
		ext = append(ext, 0, 1) // tag 0, one beneficiary
		ext = appendString(ext, b.name)
		ext = append(ext, byte(b.w), byte(b.w>>8))
	}
	o := decode(t, &CommentOptions{Author: "alice", Permlink: "p", AllowVotes: true, RawExtensions: ext}).AsCommentOptions()
	if want := []CommentBeneficiary{{Account: "bob", Weight: 1000}}; !reflect.DeepEqual(o.Beneficiaries, want) {
		t.Errorf("beneficiaries %+v, want %+v", o.Beneficiaries, want)
	}
}
