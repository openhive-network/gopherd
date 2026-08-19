package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gopherd/internal/api/jsonrpc"
	"gopherd/internal/chain"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// call runs one JSON-RPC request through the server and returns the decoded
// response.
func call(t *testing.T, srv *Server, method string, params string) jsonrpc.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
	out := srv.Registry().Handle(context.Background(), []byte(body))
	resp, ok := out.(jsonrpc.Response)
	if !ok {
		t.Fatalf("expected single Response, got %T", out)
	}
	if resp.Error != nil {
		t.Fatalf("%s error: %+v", method, resp.Error)
	}
	return resp
}

// resultMap marshals a response result back to a generic map for field checks.
func resultMap(t *testing.T, resp jsonrpc.Response) map[string]interface{} {
	t.Helper()
	b, _ := json.Marshal(resp.Result)
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result not an object: %v (%s)", err, b)
	}
	return m
}

func TestDatabaseAPIGlobals(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	srv := New(chain.New(db))

	// get_dynamic_global_properties (database_api → NAI object assets).
	m := resultMap(t, call(t, srv, "database_api.get_dynamic_global_properties", "{}"))
	if _, ok := m["head_block_number"]; !ok {
		t.Fatalf("DGPO missing head_block_number: %v", m)
	}
	vs, ok := m["virtual_supply"].(map[string]interface{})
	if !ok || vs["nai"] == nil {
		t.Fatalf("database_api asset should be a NAI object, got %#v", m["virtual_supply"])
	}

	// condenser get_dynamic_global_properties → legacy "X.XXX HIVE" string.
	cm := resultMap(t, call(t, srv, "condenser_api.get_dynamic_global_properties", "[]"))
	cs, ok := cm["virtual_supply"].(string)
	// Suffix is the active network's HIVE display symbol (HIVE / TESTS).
	hiveSuffix := " " + (asset.Symbol{Num: asset.HiveSymbol}).String()
	if !ok || !strings.HasSuffix(cs, hiveSuffix) {
		t.Fatalf("condenser asset should be a legacy string, got %#v", cm["virtual_supply"])
	}

	// get_version — chain_id present + 64 hex chars.
	vm := resultMap(t, call(t, srv, "database_api.get_version", "{}"))
	if id, _ := vm["chain_id"].(string); len(id) != 64 {
		t.Fatalf("chain_id should be 64 hex chars, got %q", vm["chain_id"])
	}

	// Legacy "call" convention: ["database_api","get_version",[]].
	out := srv.Registry().Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"call","params":["database_api","get_version",[]]}`))
	if resp, _ := out.(jsonrpc.Response); resp.Error != nil {
		t.Fatalf("call-convention dispatch failed: %+v", resp.Error)
	}

	// get_feed_history + get_active_witnesses just need to not error.
	call(t, srv, "database_api.get_feed_history", "{}")
	call(t, srv, "condenser_api.get_active_witnesses", "[]")

	// Accounts: seed alice with an authority, then read via both conventions.
	{
		sess := state.NewSession(db)
		objects.PutAccount(sess, &objects.Account{Name: "alice", Balance: 1234, HBDBalance: 5, VestingShares: 999})
		objects.PutAccountAuthority(sess, &objects.AccountAuthority{
			Account: "alice",
			Owner:   auth.Authority{WeightThreshold: 1},
			Active:  auth.Authority{WeightThreshold: 1},
			Posting: auth.Authority{WeightThreshold: 1},
		})
		if err := sess.Commit(false); err != nil {
			t.Fatal(err)
		}
	}
	// condenser get_accounts (positional, legacy assets, bare array).
	ga := call(t, srv, "condenser_api.get_accounts", `[["alice","ghost"]]`)
	arr, _ := ga.Result.([]map[string]interface{})
	if len(arr) != 1 || arr[0]["name"] != "alice" {
		t.Fatalf("get_accounts should return exactly alice, got %#v", ga.Result)
	}
	wantBal := "1.234" + hiveSuffix
	if bal, _ := arr[0]["balance"].(legacyAsset); asset.Asset(bal).String() != wantBal {
		t.Fatalf("alice legacy balance = %#v want %s", arr[0]["balance"], wantBal)
	}
	// database_api find_accounts (named, NAI assets, {accounts:[...]}).
	fa := resultMap(t, call(t, srv, "database_api.find_accounts", `{"accounts":["alice"]}`))
	accts, _ := fa["accounts"].([]interface{})
	if len(accts) != 1 {
		t.Fatalf("find_accounts should return 1 account, got %#v", fa["accounts"])
	}
	if a0, _ := accts[0].(map[string]interface{}); a0["name"] != "alice" {
		t.Fatalf("find_accounts[0] name = %v want alice", accts[0])
	}

	// Unknown method → method-not-found error.
	bad := srv.Registry().Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"database_api.nope","params":{}}`))
	if resp, _ := bad.(jsonrpc.Response); resp.Error == nil || resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("expected method-not-found, got %+v", bad)
	}
}
