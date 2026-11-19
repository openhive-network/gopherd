package api

import "testing"

// TestSnakeCase pins the Go field name to hived JSON field name mapping both
// rendering and parsing of operations rely on. ProposalIDs used to come out
// as proposal_i_ds, so update_proposal_votes and remove_proposal rendered a
// field hived does not have and a broadcast's proposal_ids was ignored.
func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{
		"ProposalIDs":   "proposal_ids",
		"JSONMetadata":  "json_metadata",
		"RequiredAuths": "required_auths",
		"MaxRC":         "max_rc",
		"HBDAmount":     "hbd_amount",
		"ID":            "id",
		"TrxID":         "trx_id",
		"URL":           "url",
		"Owner":         "owner",
	} {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}
