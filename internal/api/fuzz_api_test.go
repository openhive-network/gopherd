package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gopherd/internal/chain/testfixture"
	"gopherd/internal/protocol/hardfork"
)

// paramSeeds are parameter shapes the API's methods take, in both the
// positional (condenser) and named (database_api) styles.
var paramSeeds = []string{
	`[]`, `{}`, `null`, `[null]`, `["alice"]`, `[["alice","bob"]]`, `{"accounts":["alice"]}`,
	`{"account":"alice"}`, `[0, 10]`, `[1]`, `{"block_num":1}`, `{"block_num":4294967295}`,
	`["alice", 10]`, `["alice", -1, 1000]`, `{"start":"","limit":10,"order":"by_name"}`,
	`{"start":["alice",""],"limit":1000,"order":"by_author_last_update"}`,
	`["", 1001]`, `{"keys":["STM6LLegbAgLAy28EHrffBVuANFWcFgmqRMW13wBmTExqFE9SCkg4"]}`,
	`[{"ref_block_num":0,"ref_block_prefix":0,"expiration":"2030-01-01T00:00:00","operations":[["vote",{"voter":"alice","author":"bob","permlink":"p","weight":10000}]],"extensions":[],"signatures":[]}]`,
	`{"trx":{"operations":[["transfer",{"from":"alice","to":"bob","amount":"1.000 HIVE","memo":""}]]}}`,
	`["bob","p"]`, `{"author":"bob","permlink":"p"}`, `[["0.001 HIVE"]]`, `{"id":1}`, `[1e300]`, `["\u0000"]`,
}

// FuzzAPIDispatch sends arbitrary parameters to every registered JSON-RPC
// method against a funded chain state. Anyone can reach a public node's API,
// so no method may panic, whatever the parameters: net/http recovers a
// handler panic per request, but one that leaves a lock held wedges the node.
// The first input byte picks the method; the rest is its params.
func FuzzAPIDispatch(f *testing.F) {
	fx := testfixture.NewShared(f)
	fx.HardforksUpTo(hardfork.HF28)
	fx.Actors("alice", "bob")
	fx.FundLiquid("alice", 1_000_000)
	fx.FundVesting("bob", 1_000_000)
	fx.GenerateBlock()

	srv := New(fx.Chain)
	reg := srv.Registry()
	reg.SetPanicHook(func(method string, p any, stack []byte) {
		panic(fmt.Sprintf("%s panicked: %v\n%s", method, p, stack))
	})
	names := reg.Names()
	for i := range names {
		for _, p := range paramSeeds {
			f.Add(append([]byte{byte(i)}, p...))
		}
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) == 0 {
			return
		}
		method := names[int(in[0])%len(names)]
		params := json.RawMessage(in[1:])
		if !json.Valid(params) {
			params = nil
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			return
		}
		out := reg.Handle(context.Background(), body)
		if _, err := json.Marshal(out); err != nil {
			t.Fatalf("%s: response does not marshal: %v", method, err)
		}
	})
}

// FuzzAPIBody feeds arbitrary bytes to the JSON-RPC envelope parser,
// including batches and the legacy "call" form.
func FuzzAPIBody(f *testing.F) {
	fx := testfixture.NewShared(f)
	fx.GenerateBlock()
	reg := New(fx.Chain).Registry()
	reg.SetPanicHook(func(method string, p any, stack []byte) {
		panic(fmt.Sprintf("%s panicked: %v\n%s", method, p, stack))
	})
	for _, b := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"condenser_api.get_dynamic_global_properties","params":[]}`,
		`[{"jsonrpc":"2.0","id":1,"method":"database_api.get_config"},{"id":2,"method":"call","params":["condenser_api","get_accounts",[["alice"]]]}]`,
		`{"method":"call","params":["database_api"]}`, `[]`, `[1,2]`, `{"method":null}`, ``,
	} {
		f.Add([]byte(b))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		out := reg.Handle(context.Background(), body)
		if _, err := json.Marshal(out); err != nil {
			t.Fatalf("response does not marshal: %v", err)
		}
	})
}
