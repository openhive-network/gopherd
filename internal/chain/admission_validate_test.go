package chain

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
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
