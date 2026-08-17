package api

// hived_compat_test.go ports hived's api_tests/message_format_tests corpus
// (tests/python/api_tests/message_format_tests/*) into Go contract tests for the
// APIs gopherd implements: database_api, condenser_api (consensus subset),
// rc_api, account_by_key_api, network_broadcast_api, block_api.
//
// hived's message-format tests are mostly "call the method with valid args and
// assert it does not error" (against live_mainnet, should_prepare=False), plus a
// set of negative tests that assert bad args raise an error. We mirror that:
//   1. every registered method is callable with valid args → no JSON-RPC error;
//   2. object-scan methods over an unseeded account return an empty result, not
//      an error (hived's "nobody" contract);
//   3. seeded positive assertions (find_accounts, rc, proposals, witnesses);
//   4. negative arg-validation (rc limit bounds, malformed start, unknown op).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gopherd/internal/api/jsonrpc"
	"gopherd/internal/chain"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// callR is a non-fatal variant of the api_test.go `call` helper: it returns the
// full response (including any Error) so tests can assert on error presence.
func callR(srv *Server, method, params string) jsonrpc.Response {
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
	out := srv.Registry().Handle(context.Background(), []byte(body))
	resp, _ := out.(jsonrpc.Response)
	return resp
}

// compatServer builds a Server over a genesis DB seeded with a representative
// instance of each object the positive tests read.
func compatServer(t *testing.T) *Server {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	sess := state.NewSession(db)

	// Two RC accounts with authorities + balances. alice delegates 500 RC to bob.
	objects.PutAccount(sess, &objects.Account{
		Name: "alice", Balance: 1234, HBDBalance: 5, VestingShares: 1_000_000,
		RCManaCurrent: 12345, RCManaLastUpdate: 1000, RCAdjustment: 2020748973,
		DelegatedRCOut: 500,
	})
	objects.PutAccount(sess, &objects.Account{
		Name: "bob", VestingShares: 2_000_000, DelegatedRCIn: 500,
	})
	for _, n := range []string{"alice", "bob"} {
		objects.PutAccountAuthority(sess, &objects.AccountAuthority{
			Account: auth.AccountName(n),
			Owner:   auth.Authority{WeightThreshold: 1},
			Active:  auth.Authority{WeightThreshold: 1},
			Posting: auth.Authority{WeightThreshold: 1},
		})
	}

	// A proposal (id 7).
	objects.PutProposal(sess, &objects.Proposal{
		ID: 7, Creator: "alice", Receiver: "bob", DailyPay: 1000,
		Subject: "fund", Permlink: "p", TotalVotes: 42,
	})

	// RC singletons + a direct delegation, so the rc_api positive paths work.
	objects.PutRCDirectDelegation(sess, &objects.RCDirectDelegation{From: "alice", To: "bob", DelegatedRC: 500})
	var rp objects.RCResourceParam
	for i := 0; i < objects.RCNumResources; i++ {
		rp.Entries[i].Dynamics.PoolEq = 1_000_000
		rp.Entries[i].Dynamics.MaxPool = 2_000_000
		rp.Entries[i].Dynamics.ResourceUnit = 1
	}
	objects.PutRCResourceParam(sess, &rp)
	var pool objects.RCPool
	for i := 0; i < objects.RCNumResources; i++ {
		pool.PoolArray[i] = 500_000
	}
	objects.PutRCPool(sess, &pool)
	var st objects.RCStats
	st.BlockNum = 42
	st.Regen = 777
	st.OpStats[0].Count = 2
	st.OpStats[0].Cost[objects.RCHistoryBytes] = 200
	st.OpStats[0].Usage[objects.RCHistoryBytes] = 20
	objects.PutRCStats(sess, objects.RCStatsIDArchive, &st)

	if err := sess.Commit(false); err != nil {
		t.Fatal(err)
	}
	return New(chain.New(db))
}

// TestHivedCompatCallable is the ported "call with valid args → no error"
// coverage: one row per hived message_format test, targeting the exact method
// gopherd registers. A row is (method, params); the response must not carry a
// JSON-RPC error. Object-scan methods over the (unseeded-for-that-type) account
// legitimately return empty — that is still a non-error response.
func TestHivedCompatCallable(t *testing.T) {
	srv := compatServer(t)

	// dummy pubkey (STM + 50 base58) for get_key_references; resolves to empty.
	const dummyKey = `"STM8GC13uCZbP44HzMLV6zPZGwVQ8Nt4Kji8PapsPiNq1BK153XTX"`
	const dummyTrxID = `"0000000000000000000000000000000000000000"`

	rows := []struct {
		method string
		params string
	}{
		// ---- database_api globals (message_format_tests/database_api_tests) ----
		{"database_api.get_dynamic_global_properties", "{}"},
		{"database_api.get_config", "{}"},
		{"database_api.get_current_price_feed", "{}"},
		{"database_api.get_feed_history", "{}"},
		{"database_api.get_version", "{}"},
		{"database_api.get_active_witnesses", "{}"},
		{"database_api.get_witness_schedule", "{}"},
		{"database_api.get_hardfork_properties", "{}"},
		{"database_api.get_reward_funds", "{}"},
		{"database_api.get_order_book", `{"limit":10}`},
		{"database_api.find_accounts", `{"accounts":["alice","bob"]}`},
		{"database_api.find_witnesses", `{"owners":["initminer"]}`},
		{"database_api.find_proposals", `{"proposal_ids":[7]}`},
		{"database_api.list_proposals", `{"limit":100}`},

		// ---- condenser_api globals ----
		{"condenser_api.get_dynamic_global_properties", "[]"},
		{"condenser_api.get_config", "[]"},
		{"condenser_api.get_version", "[]"},
		{"condenser_api.get_chain_properties", "[]"},
		{"condenser_api.get_hardfork_version", "[]"},
		{"condenser_api.get_next_scheduled_hardfork", "[]"},
		{"condenser_api.get_feed_history", "[]"},
		{"condenser_api.get_current_median_history_price", "[]"},
		{"condenser_api.get_active_witnesses", "[]"},
		{"condenser_api.get_witness_schedule", "[]"},
		{"condenser_api.get_reward_fund", `["post"]`},

		// ---- condenser_api accounts / witnesses ----
		{"condenser_api.get_account_count", "[]"},
		{"condenser_api.get_accounts", `[["alice","bob"]]`},
		{"condenser_api.lookup_accounts", `["a",100]`},
		{"condenser_api.lookup_witness_accounts", `["",100]`},
		{"condenser_api.get_witness_count", "[]"},
		{"condenser_api.get_witness_by_account", `["initminer"]`},
		{"condenser_api.get_witnesses_by_vote", `["",21]`},

		// ---- condenser_api object scans (empty for these accounts) ----
		{"condenser_api.get_order_book", `[10]`},
		{"condenser_api.get_conversion_requests", `["alice"]`},
		{"condenser_api.get_collateralized_conversion_requests", `["alice"]`},
		{"condenser_api.get_vesting_delegations", `["alice","",100]`},
		{"condenser_api.get_savings_withdraw_from", `["alice"]`},
		{"condenser_api.get_savings_withdraw_to", `["alice"]`},
		{"condenser_api.find_recurrent_transfers", `["alice"]`},
		{"condenser_api.get_open_orders", `["alice"]`},
		{"condenser_api.get_escrow", `["alice",0]`},
		{"condenser_api.get_owner_history", `["alice"]`},
		{"condenser_api.get_recovery_request", `["alice"]`},
		{"condenser_api.list_proposals", `{"limit":100}`},
		{"condenser_api.find_proposals", `[[7]]`},
		{"condenser_api.get_key_references", `[[` + dummyKey + `]]`},
		{"condenser_api.is_known_transaction", `[` + dummyTrxID + `]`},

		// ---- rc_api (message_format_tests/rc_api_tests) ----
		{"rc_api.get_resource_params", "{}"},
		{"rc_api.get_resource_pool", "{}"},
		{"rc_api.find_rc_accounts", `{"accounts":["alice"]}`},
		{"rc_api.list_rc_accounts", `{"start":"a","limit":100}`},
		{"rc_api.list_rc_direct_delegations", `{"start":["alice",""],"limit":100}`},
		{"rc_api.get_rc_stats", "{}"},
		{"rc_api.get_rc_operation_stats", `{"operation":"vote"}`},
		{"condenser_api.find_rc_accounts", `[["alice"]]`},
		{"condenser_api.list_rc_accounts", `["a",100]`},
		{"condenser_api.list_rc_direct_delegations", `[["alice",""],100]`},

		// ---- account_by_key_api ----
		{"account_by_key_api.get_key_references", `{"keys":[` + dummyKey + `]}`},
	}

	for _, r := range rows {
		resp := callR(srv, r.method, r.params)
		if resp.Error != nil {
			t.Errorf("%s(%s) unexpected error: %+v", r.method, r.params, resp.Error)
		}
	}
}

// TestHivedCompatPositive ports the assertion-bearing message_format tests
// (find_accounts non-empty, list_witnesses non-empty, rc.find_rc_accounts, etc.).
func TestHivedCompatPositive(t *testing.T) {
	srv := compatServer(t)

	// database_api.find_accounts → alice present (test_find_accounts.py).
	fa := resultMap(t, mustCall(t, srv, "database_api.find_accounts", `{"accounts":["alice"]}`))
	if accts, _ := fa["accounts"].([]interface{}); len(accts) != 1 {
		t.Fatalf("find_accounts([alice]) want 1, got %#v", fa["accounts"])
	}

	// condenser_api.get_witness_count ≥ 1 (initminer seeded by genesis).
	if wc := mustCall(t, srv, "condenser_api.get_witness_count", "[]"); wc.Result.(int) < 1 {
		t.Fatalf("get_witness_count = %v want ≥1", wc.Result)
	}

	// rc_api.find_rc_accounts → alice with delegated_rc 500 (test_find_rc_accounts.py).
	rc := resultMap(t, mustCall(t, srv, "rc_api.find_rc_accounts", `{"accounts":["alice"]}`))
	accts, _ := rc["rc_accounts"].([]interface{})
	if len(accts) != 1 {
		t.Fatalf("find_rc_accounts want 1, got %#v", rc["rc_accounts"])
	}
	if a0 := accts[0].(map[string]interface{}); a0["delegated_rc"].(float64) != 500 {
		t.Fatalf("alice delegated_rc = %v want 500", a0["delegated_rc"])
	}

	// rc_api.get_resource_pool → fill_level present (test_get_resource_pool.py).
	rp := resultMap(t, mustCall(t, srv, "rc_api.get_resource_pool", "{}"))
	pools := rp["resource_pool"].(map[string]interface{})
	if _, ok := pools["resource_history_bytes"]; !ok {
		t.Fatalf("get_resource_pool missing resource_history_bytes: %#v", pools)
	}

	// rc_api.get_resource_params → 5 resource_names (test_get_resource_params.py).
	gp := resultMap(t, mustCall(t, srv, "rc_api.get_resource_params", "{}"))
	if names, _ := gp["resource_names"].([]interface{}); len(names) != objects.RCNumResources {
		t.Fatalf("resource_names len = %d want %d", len(names), objects.RCNumResources)
	}

	// list_rc_direct_delegations → alice→bob (test_list_rc_direct_delegations.py).
	ld := resultMap(t, mustCall(t, srv, "rc_api.list_rc_direct_delegations", `{"start":["alice",""],"limit":100}`))
	if dels, _ := ld["rc_direct_delegations"].([]interface{}); len(dels) != 1 {
		t.Fatalf("list_rc_direct_delegations want 1, got %#v", ld["rc_direct_delegations"])
	}

	// database_api.find_proposals([7]) → proposal to bob (test_find_proposals.py).
	fp := resultMap(t, mustCall(t, srv, "database_api.find_proposals", `{"proposal_ids":[7]}`))
	if props, _ := fp["proposals"].([]interface{}); len(props) != 1 {
		t.Fatalf("find_proposals([7]) want 1, got %#v", fp["proposals"])
	}
}

// TestHivedCompatNegative ports the negative message_format tests: bad args must
// raise a JSON-RPC error (mirrors pytest.raises(ErrorInResponseError)).
func TestHivedCompatNegative(t *testing.T) {
	srv := compatServer(t)

	neg := []struct {
		name   string
		method string
		params string
	}{
		// rc list limit bounds (test_list_rc_accounts.py / list_rc_direct_delegations).
		{"rc list_accounts limit 0", "rc_api.list_rc_accounts", `{"start":"a","limit":0}`},
		{"rc list_accounts limit 1001", "rc_api.list_rc_accounts", `{"start":"a","limit":1001}`},
		{"rc list_delegations limit 0", "rc_api.list_rc_direct_delegations", `{"start":["alice",""],"limit":0}`},
		{"rc list_delegations limit 1001", "rc_api.list_rc_direct_delegations", `{"start":["alice",""],"limit":1001}`},
		// list_rc_direct_delegations start must be a 2-tuple.
		{"rc list_delegations bad start", "rc_api.list_rc_direct_delegations", `{"start":["alice"],"limit":100}`},
		// unknown operation name.
		{"rc op_stats bad op", "rc_api.get_rc_operation_stats", `{"operation":"definitely_not_an_op"}`},
	}
	for _, n := range neg {
		if resp := callR(srv, n.method, n.params); resp.Error == nil {
			t.Errorf("%s: expected error, got result %#v", n.name, resp.Result)
		}
	}

	// Unknown method → method-not-found.
	if resp := callR(srv, "rc_api.nope", "{}"); resp.Error == nil || resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Errorf("unknown method: expected method-not-found, got %+v", resp)
	}
}

// TestHivedCompatEmptyScans ports the "nobody" contract: object-scan methods
// over a non-existent account return an empty array (not an error).
func TestHivedCompatEmptyScans(t *testing.T) {
	srv := compatServer(t)
	for _, m := range []string{
		"condenser_api.get_conversion_requests",
		"condenser_api.get_collateralized_conversion_requests",
		"condenser_api.get_open_orders",
		"condenser_api.get_savings_withdraw_from",
		"condenser_api.get_savings_withdraw_to",
		"condenser_api.find_recurrent_transfers",
	} {
		resp := callR(srv, m, `["nobody-here"]`)
		if resp.Error != nil {
			t.Errorf("%s on empty: unexpected error %+v", m, resp.Error)
			continue
		}
		arr, ok := resp.Result.([]map[string]interface{})
		if !ok || len(arr) != 0 {
			// some methods marshal as []interface{}; accept any empty slice.
			b, _ := json.Marshal(resp.Result)
			if s := strings.TrimSpace(string(b)); s != "[]" {
				t.Errorf("%s on empty: want [], got %s", m, s)
			}
		}
	}
}

// mustCall is the fatal-on-error call used by positive tests.
func mustCall(t *testing.T, srv *Server, method, params string) jsonrpc.Response {
	t.Helper()
	resp := callR(srv, method, params)
	if resp.Error != nil {
		t.Fatalf("%s error: %+v", method, resp.Error)
	}
	return resp
}
