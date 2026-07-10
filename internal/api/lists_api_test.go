package api

import (
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// TestListsAPI checks the table-scan list/find methods: empty scans return empty
// (not error), and a seeded proposal appears in list_proposals / find_proposals.
func TestListsAPI(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	{
		sess := state.NewSession(db)
		objects.PutProposal(sess, &objects.Proposal{
			ID: 7, Creator: "alice", Receiver: "bob", DailyPay: 1000,
			Subject: "fund", Permlink: "p", TotalVotes: 42,
		})
		if err := sess.Commit(false); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(chain.New(db))

	// Empty account scans must return [] without error.
	for _, m := range []string{
		"condenser_api.get_conversion_requests", "condenser_api.get_open_orders",
		"condenser_api.get_vesting_delegations", "condenser_api.find_recurrent_transfers",
		"condenser_api.get_savings_withdraw_from",
	} {
		resp := call(t, srv, m, `["nobody"]`)
		if arr, ok := resp.Result.([]map[string]interface{}); !ok || len(arr) != 0 {
			t.Fatalf("%s on empty should be [], got %#v", m, resp.Result)
		}
	}

	// list_proposals returns the seeded proposal.
	lp := call(t, srv, "condenser_api.list_proposals", `{"limit":100}`)
	arr, _ := lp.Result.([]map[string]interface{})
	if len(arr) != 1 || arr[0]["creator"] != "alice" || arr[0]["subject"] != "fund" {
		t.Fatalf("list_proposals = %#v want 1 proposal by alice", lp.Result)
	}

	// find_proposals by id.
	fp := call(t, srv, "condenser_api.find_proposals", `[[7]]`)
	farr, _ := fp.Result.([]map[string]interface{})
	if len(farr) != 1 || farr[0]["receiver"] != "bob" {
		t.Fatalf("find_proposals([7]) = %#v want proposal to bob", fp.Result)
	}

	// witness enumeration: genesis seeds initminer, so count is ≥ 1.
	if wc := call(t, srv, "condenser_api.get_witness_count", `[]`); wc.Result.(int) < 1 {
		t.Fatalf("get_witness_count = %v want ≥1 (initminer)", wc.Result)
	}
}
