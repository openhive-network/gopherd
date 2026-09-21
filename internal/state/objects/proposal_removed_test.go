package objects

import "testing"

// TestProposalRemovedRoundTrip covers both directions of the Removed flag's
// encoding, including the one that matters operationally: a row written before
// the field existed must still decode. Every proposal in every snapshot on
// disk is in that older form, and they were all unflagged — nothing could set
// the flag — so "short row" must mean Removed=false, not a decode error.
func TestProposalRemovedRoundTrip(t *testing.T) {
	base := Proposal{
		ID: 7, Creator: "alice", Receiver: "bob",
		StartDate: 100, EndDate: 200, DailyPay: 3000,
		Subject: "s", Permlink: "p", TotalVotes: 42,
	}

	for _, removed := range []bool{false, true} {
		p := base
		p.Removed = removed
		got, err := ReadProposal(AppendProposal(nil, &p))
		if err != nil {
			t.Fatalf("removed=%v: %v", removed, err)
		}
		if got != p {
			t.Fatalf("removed=%v: round trip changed the proposal:\n got %+v\nwant %+v", removed, got, p)
		}
	}

	// A pre-field row: everything through total_votes and not one byte more.
	legacy := AppendProposal(nil, &base)
	legacy = legacy[:len(legacy)-1]
	got, err := ReadProposal(legacy)
	if err != nil {
		t.Fatalf("legacy row must still decode: %v", err)
	}
	if got != base {
		t.Fatalf("legacy row decoded wrong:\n got %+v\nwant %+v", got, base)
	}

	// Trailing bytes beyond the flag are still an error — the field is one
	// byte, not an open-ended tail.
	if _, err := ReadProposal(append(AppendProposal(nil, &base), 0)); err == nil {
		t.Error("trailing bytes past the flag should be rejected")
	}
}
