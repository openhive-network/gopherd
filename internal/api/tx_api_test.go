package api

import (
	"encoding/json"
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

func TestTxAPIVerifyAndSignatures(t *testing.T) {
	const keyStr = "STM8GC13uCZbP44HzMLV6zPZGwVQ8Nt4Kji8PapsPiNq1BK153XTX"
	k, err := crypto.DecodePublicKey(keyStr)
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
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
		objects.PutAccount(sess, &objects.Account{Name: "alice", Balance: 1000})
		objects.PutAccountAuthority(sess, &objects.AccountAuthority{
			Account: "alice",
			Owner:   auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Key: k, Weight: 1}}},
			Active:  auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Key: k, Weight: 1}}},
			Posting: auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Key: k, Weight: 1}}},
		})
		if err := sess.Commit(false); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(chain.New(db))

	// Build a transfer tx (active authority) from alice, expressed in JSON.
	opJSON, _ := json.Marshal(opToJSON(op.FromOperation(op.Transfer{
		From: "alice", To: "bob", Amount: hiveAsset(1), Memo: "",
	}), true))
	txJSON := `{"ref_block_num":1,"ref_block_prefix":2,"expiration":"2020-01-01T00:00:00","operations":[` + string(opJSON) + `],"signatures":[]}`

	// get_transaction_hex round-trips through the parser + serializer.
	if h := call(t, srv, "condenser_api.get_transaction_hex", `[`+txJSON+`]`); h.Result.(string) == "" {
		t.Fatal("get_transaction_hex returned empty")
	}

	// get_potential_signatures: alice's active key must be present.
	ps := call(t, srv, "condenser_api.get_potential_signatures", `[`+txJSON+`]`)
	keys, _ := ps.Result.([]string)
	found := false
	for _, kk := range keys {
		if kk == keyStr {
			found = true
		}
	}
	if !found {
		t.Fatalf("get_potential_signatures = %v, want to include %s", keys, keyStr)
	}

	// verify_authority on the UNSIGNED tx → not valid (no signatures).
	va := call(t, srv, "condenser_api.verify_authority", `[`+txJSON+`]`)
	if va.Result.(bool) != false {
		t.Fatalf("verify_authority on unsigned tx = %v, want false", va.Result)
	}
}
