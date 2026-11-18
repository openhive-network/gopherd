package chain

import (
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestDelegateRCRegularOperationRC pins the regular delegate_rc_operation's
// authority, payer and resource usage to hived's rc_usage_delegate_rc_both_forms
// (rc_tests.cpp, 7cc13d0af): posting of `from`, which also pays; state of
// 1927200 per delegatee (none when removing) plus the transaction's 128 per
// hour to expiration; execution time of the transaction, one signature and
// delegate_rc_time — 94165 + 6622 + 75000 — with no custom_json_time.
func TestDelegateRCRegularOperationRC(t *testing.T) {
	d := op.DelegateRC{From: "dave", Delegatees: []auth.AccountName{"bob", "carol"}, MaxRC: 5000000}
	st := tx.SignedTransaction{
		Transaction: tx.Transaction{Expiration: 3600, Operations: op.Ops(d)},
		Signatures:  make([][tx.SignatureSize]byte, 1),
	}
	req := RequiredAuthorities(&st)
	if _, ok := req[AuthorityPosting]["dave"]; !ok || len(req[AuthorityPosting]) != 1 ||
		len(req[AuthorityActive]) != 0 || len(req[AuthorityOwner]) != 0 {
		t.Fatalf("required authorities %v, want posting of dave only", req)
	}
	if got := ResourceUserOf(&st); got != "dave" {
		t.Fatalf("resource user %q", got)
	}

	usage := CountResourcesForTx(&st, 100, 0)
	if got, want := usage[objects.RCStateBytes], int64(128+2*1927200); got != want {
		t.Errorf("state bytes %d, want %d", got, want)
	}
	if got, want := usage[objects.RCExecutionTime], int64(94165+6622+75000); got != want {
		t.Errorf("execution time %d, want %d", got, want)
	}
	st.Operations = op.Ops(op.DelegateRC{From: "dave", Delegatees: []auth.AccountName{"bob"}})
	if got := CountResourcesForTx(&st, 100, 0)[objects.RCStateBytes]; got != 128 {
		t.Errorf("removal: state bytes %d, want 128", got)
	}

	// Discount: the state of the delegations about to be overwritten, taken
	// before the evaluator like every regular operation's.
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	for _, n := range []auth.AccountName{"dave", "bob", "carol"} {
		objects.PutAccount(s, &objects.Account{Name: n})
	}
	objects.PutRCDirectDelegationFull(s, &objects.RCDirectDelegation{From: "dave", To: "bob", DelegatedRC: 1})
	if got := countDifferentialOp(s, op.FromOperation(d))[objects.RCStateBytes]; got != 1927200 {
		t.Errorf("discount %d, want 1927200 (one existing delegation)", got)
	}
}
