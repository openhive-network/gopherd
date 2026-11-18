package evaluator

import (
	"fmt"
	"math"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

func drcTxUsage(s *state.Session) [objects.RCNumResources]int64 {
	return objects.GetRCPendingData(s).DifferentialUsage
}

func drcResetTxUsage(s *state.Session) {
	p := objects.GetRCPendingData(s)
	p.ResetDifferentialUsage()
	objects.PutRCPendingData(s, &p)
}

func drcUsage(state, exec int64) [objects.RCNumResources]int64 {
	var u [objects.RCNumResources]int64
	u[objects.RCStateBytes] = state
	u[objects.RCExecutionTime] = exec
	return u
}

func drcBatch(from string, ops ...string) op.Op {
	payload := "["
	for i, o := range ops {
		if i > 0 {
			payload += ","
		}
		payload += `["delegate_rc",` + o + `]`
	}
	payload += "]"
	return op.FromOperation(&op.CustomJSON{
		RequiredPostingAuths: []auth.AccountName{auth.AccountName(from)},
		ID:                   "rc",
		JSON:                 payload,
	})
}

func drcValue(from string, maxRC int64, to string) string {
	return fmt.Sprintf(`{"from":%q,"delegatees":[%q],"max_rc":%d}`, from, to, maxRC)
}

// TestEmbeddedDelegateRCUsage is hived's rc_usage_of_failed_embedded_rc_delegation_in_block
// (hf29_tests.cpp, 7cc13d0af). Each embedded delegate_rc takes its state
// discount right before it runs and adds its own usage only once it succeeded.
// When one fails inside a block the custom_json stays and its writes are
// undone; before HF29 the usage already collected stays charged (the discount
// of the failed operation included), from HF29 (REVERT_RC_USAGE_OF_FAILED_CUSTOM_OPS)
// it is reverted with them.
//
// The failure used is a delegatee maximum-RC overflow, which gopherd enforces
// in blocks before HF29 as hived does; hived's test uses over-delegation, a
// check gopherd skips in blocks before HF29 (A15).
func TestEmbeddedDelegateRCUsage(t *testing.T) {
	const existing int64 = 5 * drcMinDelegation
	for _, hf29 := range []bool{false, true} {
		s := drcSession(t)
		if err := ApplyOp(drcCtx(s, false, true), drcEmbedded("alice", existing, "bob")); err != nil {
			t.Fatal(err)
		}
		// bob's maximum RC leaves room for exactly `existing` more.
		bob := mustAccount(t, s, "bob")
		bob.VestingShares = math.MaxInt64 - bob.DelegatedRCIn - existing
		objects.PutAccount(s, &bob)
		tooMuch := 2*existing + 1

		ctx := drcCtx(s, hf29, false)
		drcResetTxUsage(s)
		if err := ApplyOp(ctx, drcBatch("alice", drcValue("alice", tooMuch, "bob"))); err != nil {
			t.Fatalf("hf29=%v: failed embedded delegate_rc failed the block: %v", hf29, err)
		}
		want := drcUsage(-DelegateRCBaseStateSize, 0) // discount for the delegation it did not change
		if hf29 {
			want = drcUsage(0, 0)
		}
		if got := drcTxUsage(s); got != want {
			t.Errorf("hf29=%v failed update: usage %v, want %v", hf29, got, want)
		}

		drcResetTxUsage(s)
		if err := ApplyOp(ctx, drcBatch("alice", drcValue("alice", existing, "carol"), drcValue("alice", tooMuch, "bob"))); err != nil {
			t.Fatal(err)
		}
		// before HF29: carol's new delegation and the reverted run are paid
		// for, bob's discount offsets carol's state
		want = drcUsage(0, DelegateRCExecTimeNS)
		if hf29 {
			want = drcUsage(0, 0)
		}
		if got := drcTxUsage(s); got != want {
			t.Errorf("hf29=%v create + failed update: usage %v, want %v", hf29, got, want)
		}
		if _, ok := drcDelegated(s, "alice", "carol"); ok {
			t.Fatalf("hf29=%v: carol's delegation survived the failed batch", hf29)
		}

		drcResetTxUsage(s)
		if err := ApplyOp(ctx, drcBatch("alice", drcValue("alice", existing, "carol"), drcValue("alice", existing/2, "bob"))); err != nil {
			t.Fatal(err)
		}
		if got, want := drcTxUsage(s), drcUsage(DelegateRCBaseStateSize, 2*DelegateRCExecTimeNS); got != want {
			t.Errorf("hf29=%v create + update: usage %v, want %v", hf29, got, want)
		}
		if _, ok := drcDelegated(s, "alice", "carol"); !ok {
			t.Fatalf("hf29=%v: carol's delegation missing", hf29)
		}
	}
}

// TestEmbeddedDelegateRCUsageNotCharged: nothing is collected where no
// delegate_rc evaluator runs — before HF26 (hived returns before the discount),
// or when the payload fails to parse, validate or match its authorities.
func TestEmbeddedDelegateRCUsageNotCharged(t *testing.T) {
	s := drcSession(t)
	pre26 := &Context{DB: s, Hardforks: hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF26] - 1), HeadBlockTime: drcNow}
	for _, tc := range []struct {
		name string
		ctx  *Context
		o    op.Op
	}{
		{"pre-HF26", pre26, drcEmbedded("alice", 2*drcMinDelegation, "bob")},
		{"unparsable", drcCtx(s, false, false), op.FromOperation(&op.CustomJSON{
			RequiredPostingAuths: []auth.AccountName{"alice"}, ID: "rc", JSON: `["delegate_rc",`})},
		{"invalid", drcCtx(s, false, false), drcEmbedded("alice", -1, "bob")},
		{"wrong authority", drcCtx(s, false, false), op.FromOperation(&op.CustomJSON{
			RequiredPostingAuths: []auth.AccountName{"bob"}, ID: "rc",
			JSON: `["delegate_rc",` + drcValue("alice", 2*drcMinDelegation, "carol") + `]`})},
	} {
		if tc.ctx == pre26 && hardfork.MainnetActivationBlock[hardfork.HF26] == 0 {
			continue // testnet build: every hardfork is active from block 0
		}
		drcResetTxUsage(s)
		if err := ApplyOp(tc.ctx, tc.o); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := drcTxUsage(s); got != ([objects.RCNumResources]int64{}) {
			t.Errorf("%s: usage %v, want none", tc.name, got)
		}
	}
}
