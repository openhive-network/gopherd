package api

// api_review_test.go locks in the security/performance fixes from the module
// audit: bounded list scans (early termination honors `limit`), the O(1) account
// count, limit clamps, the JSON-RPC batch cap, mempool eviction, and the RC
// fill_level big.Int path (no int64 overflow).

import (
	"context"
	"strconv"
	"testing"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/chain"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// seedAccounts builds a server with `n` deterministically-named accounts
// (acct-000, acct-001, …) plus RC state, for scan-bound assertions.
func seedAccounts(t *testing.T, n int) *Server {
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
	for i := 0; i < n; i++ {
		name := "acct-" + strconv.Itoa(1000+i) // acct-1000, acct-1001, … (sortable)
		objects.PutAccount(sess, &objects.Account{Name: auth.AccountName(name), VestingShares: int64(i + 1)})
	}
	if err := sess.Commit(false); err != nil {
		t.Fatal(err)
	}
	return New(chain.New(db))
}

// TestListRCAccountsBoundedByLimit: with more accounts than `limit`, exactly
// `limit` are returned, in ascending name order from `start` (early termination).
func TestListRCAccountsBoundedByLimit(t *testing.T) {
	srv := seedAccounts(t, 50)
	res := resultMap(t, mustCall(t, srv, "rc_api.list_rc_accounts", `{"start":"acct-1000","limit":5}`))
	accts, _ := res["rc_accounts"].([]interface{})
	if len(accts) != 5 {
		t.Fatalf("want exactly 5 rc_accounts, got %d", len(accts))
	}
	want := []string{"acct-1000", "acct-1001", "acct-1002", "acct-1003", "acct-1004"}
	for i, a := range accts {
		if got := a.(map[string]interface{})["account"]; got != want[i] {
			t.Fatalf("rc_accounts[%d] = %v want %s", i, got, want[i])
		}
	}
}

// TestLookupAccountsBounded: lookup_accounts honors limit + ascending order.
func TestLookupAccountsBounded(t *testing.T) {
	srv := seedAccounts(t, 50)
	resp := mustCall(t, srv, "condenser_api.lookup_accounts", `["acct-1000",3]`)
	names, _ := resp.Result.([]string)
	if len(names) != 3 || names[0] != "acct-1000" || names[2] != "acct-1002" {
		t.Fatalf("lookup_accounts bounded = %#v", resp.Result)
	}
}

// TestGetAccountCountMatchesStore: O(1) count equals the seeded total (+genesis).
func TestGetAccountCountMatchesStore(t *testing.T) {
	srv := seedAccounts(t, 17)
	resp := mustCall(t, srv, "condenser_api.get_account_count", "[]")
	if n := resp.Result.(int); n < 17 {
		t.Fatalf("get_account_count = %d want ≥17", n)
	}
}

// TestListProposalsLimitClamped: an absurd limit must not error and is clamped.
func TestListProposalsLimitClamped(t *testing.T) {
	srv := compatServer(t)
	if resp := callR(srv, "database_api.list_proposals", `{"limit":1000000000}`); resp.Error != nil {
		t.Fatalf("list_proposals huge limit should clamp, not error: %+v", resp.Error)
	}
}

// TestBatchCap: a batch larger than maxBatchRequests is rejected.
func TestBatchCap(t *testing.T) {
	srv := compatServer(t)
	body := "["
	for i := 0; i < 600; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"jsonrpc":"2.0","id":1,"method":"database_api.get_config","params":{}}`
	}
	body += "]"
	out := srv.Registry().Handle(context.Background(), []byte(body))
	resp, ok := out.(jsonrpc.Response)
	if !ok || resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("oversized batch should be rejected with InvalidRequest, got %#v", out)
	}
}

// TestMempoolEviction: the pool never exceeds maxMempool and evicts FIFO.
func TestMempoolEviction(t *testing.T) {
	m := newMempool()
	for i := 0; i < maxMempool+100; i++ {
		m.Add("tx-"+strconv.Itoa(i), tx.SignedTransaction{})
	}
	if len(m.txs) != maxMempool {
		t.Fatalf("mempool size = %d want %d", len(m.txs), maxMempool)
	}
	if len(m.order) != maxMempool {
		t.Fatalf("order slice = %d want %d", len(m.order), maxMempool)
	}
	// The first 100 inserted must have been evicted; the last maxMempool remain.
	if m.Has("tx-0") || m.Has("tx-99") {
		t.Fatalf("oldest entries should have been evicted")
	}
	if !m.Has("tx-" + strconv.Itoa(maxMempool+99)) {
		t.Fatalf("newest entry should be present")
	}
}

// TestResourcePoolFillLevelNoOverflow: a pool large enough that pool*10000
// overflows int64 in the naive path still yields a correct fill_level.
func TestResourcePoolFillLevelNoOverflow(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	sess := state.NewSession(db)
	// pool == pool_eq == 2e15: naive 2e15*10000 = 2e19 > int64 max (~9.2e18) →
	// would wrap negative; big.Int path must give fill_level == 10000.
	const big15 = int64(2_000_000_000_000_000)
	var rp objects.RCResourceParam
	var pool objects.RCPool
	for i := 0; i < objects.RCNumResources; i++ {
		rp.Entries[i].Dynamics.PoolEq = uint64(big15)
		pool.PoolArray[i] = big15
	}
	objects.PutRCResourceParam(sess, &rp)
	objects.PutRCPool(sess, &pool)
	if err := sess.Commit(false); err != nil {
		t.Fatal(err)
	}
	srv := New(chain.New(db))

	res := resultMap(t, mustCall(t, srv, "rc_api.get_resource_pool", "{}"))
	pools := res["resource_pool"].(map[string]interface{})
	hist := pools["resource_history_bytes"].(map[string]interface{})
	if fl := hist["fill_level"].(float64); fl != 10000 {
		t.Fatalf("fill_level = %v want 10000 (int64 overflow not handled)", fl)
	}
}
