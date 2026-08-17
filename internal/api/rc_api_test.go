package api

import (
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestRCAPI exercises the rc_api surface over seeded RC state: per-account RC
// fields, a direct delegation, the resource pool/param singletons, and the
// archived rc_stats object.
func TestRCAPI(t *testing.T) {
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
		// Two RC accounts. alice delegates 500 RC out to bob.
		objects.PutAccount(sess, &objects.Account{
			Name: "alice", RCManaCurrent: 12345, RCManaLastUpdate: 1000,
			VestingShares: 1_000_000, RCAdjustment: 2020748973,
			DelegatedRCOut: 500,
		})
		objects.PutAccount(sess, &objects.Account{
			Name: "bob", RCManaCurrent: 6789, RCManaLastUpdate: 1001,
			VestingShares: 2_000_000, DelegatedRCIn: 500,
		})
		objects.PutRCDirectDelegation(sess, &objects.RCDirectDelegation{
			From: "alice", To: "bob", DelegatedRC: 500,
		})

		// Resource param + pool singletons.
		var rp objects.RCResourceParam
		for i := 0; i < objects.RCNumResources; i++ {
			rp.Entries[i].Dynamics.PoolEq = 1_000_000
			rp.Entries[i].Dynamics.MaxPool = 2_000_000
			rp.Entries[i].Dynamics.ResourceUnit = 1
		}
		objects.PutRCResourceParam(sess, &rp)
		var pool objects.RCPool
		for i := 0; i < objects.RCNumResources; i++ {
			pool.PoolArray[i] = 500_000 // half of pool_eq → fill_level 5000
		}
		objects.PutRCPool(sess, &pool)

		// Archived rc_stats with a vote op observation.
		var st objects.RCStats
		st.BlockNum = 42
		st.Regen = 777
		st.AverageCost[0] = 111
		st.OpStats[0].Count = 2
		st.OpStats[0].Cost[objects.RCHistoryBytes] = 200
		st.OpStats[0].Usage[objects.RCHistoryBytes] = 20
		objects.PutRCStats(sess, objects.RCStatsIDArchive, &st)

		if err := sess.Commit(false); err != nil {
			t.Fatal(err)
		}
	}

	srv := New(chain.New(db))

	// find_rc_accounts: alice's RC fields.
	fa := call(t, srv, "rc_api.find_rc_accounts", `{"accounts":["alice","ghost"]}`)
	fam := resultMap(t, fa)
	accts, _ := fam["rc_accounts"].([]interface{})
	if len(accts) != 1 {
		t.Fatalf("find_rc_accounts: want 1 (ghost skipped), got %#v", fam["rc_accounts"])
	}
	a0 := accts[0].(map[string]interface{})
	if a0["account"] != "alice" {
		t.Fatalf("find_rc_accounts account = %v", a0["account"])
	}
	if a0["delegated_rc"].(float64) != 500 {
		t.Fatalf("alice delegated_rc = %v want 500", a0["delegated_rc"])
	}
	mb := a0["rc_manabar"].(map[string]interface{})
	if mb["current_mana"] != "12345" {
		t.Fatalf("alice rc_manabar.current_mana = %v want 12345", mb["current_mana"])
	}

	// list_rc_accounts: both accounts from "a", limit 10.
	la := call(t, srv, "rc_api.list_rc_accounts", `{"start":"a","limit":10}`)
	lam := resultMap(t, la)
	if lst, _ := lam["rc_accounts"].([]interface{}); len(lst) < 2 {
		t.Fatalf("list_rc_accounts want ≥2, got %d", len(lst))
	}

	// list_rc_direct_delegations: alice → bob.
	ld := call(t, srv, "rc_api.list_rc_direct_delegations", `{"start":["alice",""],"limit":10}`)
	ldm := resultMap(t, ld)
	dels, _ := ldm["rc_direct_delegations"].([]interface{})
	if len(dels) != 1 {
		t.Fatalf("list_rc_direct_delegations want 1, got %#v", ldm["rc_direct_delegations"])
	}
	d0 := dels[0].(map[string]interface{})
	if d0["to"] != "bob" || d0["delegated_rc"].(float64) != 500 {
		t.Fatalf("delegation = %#v want alice→bob 500", d0)
	}

	// get_resource_pool: fill_level 5000 (pool 500k of pool_eq 1M).
	rp := call(t, srv, "rc_api.get_resource_pool", `{}`)
	rpm := resultMap(t, rp)
	pools := rpm["resource_pool"].(map[string]interface{})
	hist := pools["resource_history_bytes"].(map[string]interface{})
	if hist["fill_level"].(float64) != 5000 || hist["pool"].(float64) != 500000 {
		t.Fatalf("resource_history_bytes pool = %#v want pool 500000 fill 5000", hist)
	}

	// get_resource_params: names + dynamics params present.
	gp := call(t, srv, "rc_api.get_resource_params", `{}`)
	gpm := resultMap(t, gp)
	names, _ := gpm["resource_names"].([]interface{})
	if len(names) != objects.RCNumResources || names[0] != "resource_history_bytes" {
		t.Fatalf("resource_names = %#v", gpm["resource_names"])
	}
	sizeInfo := gpm["size_info"].(map[string]interface{})
	execInfo := sizeInfo["resource_execution_time"].(map[string]interface{})
	if execInfo["vote_time"].(float64) != 479+17568+187+78 {
		t.Fatalf("vote_time = %v", execInfo["vote_time"])
	}

	// get_rc_stats: block + regen + vote op present.
	gs := call(t, srv, "rc_api.get_rc_stats", `{}`)
	gsm := resultMap(t, gs)
	stats := gsm["rc_stats"].(map[string]interface{})
	if stats["block"].(float64) != 42 || stats["regen"].(float64) != 777 {
		t.Fatalf("rc_stats block/regen = %v/%v", stats["block"], stats["regen"])
	}
	ops := stats["ops"].(map[string]interface{})
	voteOp := ops["vote"].(map[string]interface{})
	if voteOp["count"].(float64) != 2 || voteOp["avg_cost"].(float64) != 100 {
		t.Fatalf("vote op = %#v want count 2 avg_cost 100", voteOp)
	}

	// get_rc_operation_stats: vote averaged cost/usage.
	os := call(t, srv, "rc_api.get_rc_operation_stats", `{"operation":"vote"}`)
	osm := resultMap(t, os)
	if osm["count"].(float64) != 2 || osm["avg_cost_rc"].(float64) != 100 {
		t.Fatalf("op stats vote count/avg = %v/%v", osm["count"], osm["avg_cost_rc"])
	}
	cost := osm["resource_cost"].(map[string]interface{})
	if cost["history_rc"].(float64) != 100 { // 200/2
		t.Fatalf("history_rc = %v want 100", cost["history_rc"])
	}

	// condenser variant returns a bare array.
	cf := call(t, srv, "condenser_api.find_rc_accounts", `[["alice"]]`)
	if arr, ok := cf.Result.([]map[string]interface{}); !ok || len(arr) != 1 {
		t.Fatalf("condenser find_rc_accounts = %#v want 1-element array", cf.Result)
	}
}
