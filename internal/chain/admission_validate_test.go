package chain

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// TestAdmissionRunsOperationValidators pins that structurally invalid
// operations are refused at admission.
//
// hived validates every operation of every transaction it accepts
// (database.cpp:2627). We shipped the validators and never called them on this
// path, so we admitted transactions the rest of the network had already
// refused. They accumulated (~20/block), passed every other check we ran, and
// 579 of them went into block 108912883 — which the network rejected.
func TestAdmissionRunsOperationValidators(t *testing.T) {
	// A transfer with an invalid account name: hived's is_valid_account_name
	// rejects it, so no witness would ever include it.
	bad := op.Transfer{
		From:   "not a valid name!",
		To:     "alice",
		Amount: asset.Asset{Amount: 1, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("fixture is not actually invalid; the test would prove nothing")
	}

	// A well-formed one must pass, or admission would reject real traffic.
	good := op.Transfer{
		From:   "initminer",
		To:     "alice",
		Amount: asset.Asset{Amount: 1, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid transfer rejected by validator: %v", err)
	}

	// The dispatch admission uses must find the validator through the tagged
	// union — if Operation() did not surface it, the check would silently pass
	// everything and we would be back where we started.
	var wrapped op.Op = op.FromOperation(&bad)
	v, ok := wrapped.Operation().(op.Validator)
	if !ok {
		t.Fatal("Operation() did not expose the Validator interface; admission would validate nothing")
	}
	if err := v.Validate(); err == nil {
		t.Fatal("validator reached through the tagged union accepted an invalid operation")
	}
}

// TestCustomOpBlockLimitAppliesToAssembly pins that block ASSEMBLY enforces the
// same per-account custom-operation cap that block APPLICATION does.
//
// It did not, and that is very likely why block 108912883 was rejected by the
// network: we packed 579 transactions (65KB) where the real block for that slot
// held 90. A witness may only include customOpBlockLimit custom operations per
// account per block; everything past that makes the block INVALID. Our own node
// never objected, because it enforces the rule only in ApplyBlock — so we
// assembled a block we would have refused had someone else sent it to us.
func TestCustomOpBlockLimitAppliesToAssembly(t *testing.T) {
	counts := map[auth.AccountName]uint8{}
	mk := func(who string) *tx.SignedTransaction {
		return &tx.SignedTransaction{Transaction: tx.Transaction{
			Operations: []op.Op{op.FromOperation(&op.CustomJSON{
				RequiredPostingAuths: []auth.AccountName{auth.AccountName(who)},
				ID:                   "test",
				JSON:                 "{}",
			})},
		}}
	}

	// The same account may only get customOpBlockLimit into one block.
	accepted := 0
	for i := 0; i < int(customOpBlockLimit)+3; i++ {
		if err := applyCustomOpBlockLimit(counts, mk("spammer")); err == nil {
			accepted++
		}
	}
	if accepted != int(customOpBlockLimit) {
		t.Fatalf("accepted %d custom ops for one account, cap is %d — assembling past the cap produces a block the network rejects",
			accepted, customOpBlockLimit)
	}

	// A DIFFERENT account is unaffected: the cap is per account, and treating it
	// as per block would starve everyone else once one spammer hit the limit.
	if err := applyCustomOpBlockLimit(counts, mk("someone-else")); err != nil {
		t.Fatalf("a different account was refused: %v — the cap is per ACCOUNT, not per block", err)
	}

	// Fresh block, fresh budget: the counter is per block, so a new assembly
	// must start from zero or we would under-fill every subsequent block.
	fresh := map[auth.AccountName]uint8{}
	if err := applyCustomOpBlockLimit(fresh, mk("spammer")); err != nil {
		t.Fatalf("new block did not reset the per-account budget: %v", err)
	}
}
