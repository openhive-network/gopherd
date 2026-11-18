package evaluator

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

const (
	drcNow = 1_700_000_000
	// account_creation_fee 3.000 HIVE at 1000 VESTS-satoshi per HIVE-satoshi:
	// min delegation = (3000/3) * 1000 = 1_000_000.
	drcFee           = 3000
	drcMinDelegation = 1_000_000
	drcAliceVests    = 10_000_000_000
)

// drcSession seeds what delegate_rc reads: the vesting price, the median
// account creation fee and three accounts whose RC is fresh at drcNow.
func drcSession(t *testing.T) *state.Session {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := state.NewSession(db)
	objects.PutDGPO(s, &objects.DynamicGlobalProperty{
		TotalVestingShares:   1_000_000_000_000,
		TotalVestingFundHive: 1_000_000_000,
		Time:                 drcNow,
	})
	objects.PutWitnessSchedule(s, &objects.WitnessSchedule{MedianAccountCreationFee: drcFee})
	for name, vests := range map[string]int64{"alice": drcAliceVests, "bob": 1_000_000, "carol": 1_000_000} {
		objects.PutAccount(s, &objects.Account{
			Name:             auth.AccountName(name),
			VestingShares:    vests,
			RCManaCurrent:    vests,
			LastMaxRC:        vests,
			RCManaLastUpdate: drcNow,
		})
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	return state.NewSession(db)
}

func drcCtx(s *state.Session, hf29, inControl bool) *Context {
	hs := preHF29Set()
	if hf29 {
		hs = hf29Set()
	}
	return &Context{DB: s, Hardforks: hs, InControl: inControl, HeadBlockTime: drcNow}
}

func drcRegular(from string, maxRC int64, to ...string) op.Op {
	d := op.DelegateRC{From: auth.AccountName(from), MaxRC: maxRC}
	for _, n := range to {
		d.Delegatees = append(d.Delegatees, auth.AccountName(n))
	}
	return op.FromOperation(&d)
}

func drcEmbedded(from string, maxRC int64, to ...string) op.Op {
	q := make([]string, len(to))
	for i, n := range to {
		q[i] = fmt.Sprintf("%q", n)
	}
	return op.FromOperation(&op.CustomJSON{
		RequiredPostingAuths: []auth.AccountName{auth.AccountName(from)},
		ID:                   "rc",
		JSON: fmt.Sprintf(`["delegate_rc",{"from":%q,"delegatees":[%s],"max_rc":%d}]`,
			from, strings.Join(q, ","), maxRC),
	})
}

func drcDelegated(s *state.Session, from, to string) (int64, bool) {
	d, err := objects.GetRCDirectDelegation(s, auth.AccountName(from), auth.AccountName(to))
	if err != nil {
		return 0, false
	}
	return int64(d.DelegatedRC), true
}

// TestDelegateRCRegularOperationGate: tag 37 is delegate_rc_operation, refused
// before HF29 whether or not in control (HIVE_CHAIN_HARDFORK_ASSERT has no
// is_in_control half), applied from HF29 like the embedded form.
func TestDelegateRCRegularOperationGate(t *testing.T) {
	for _, inControl := range []bool{true, false} {
		s := drcSession(t)
		if err := ApplyOp(drcCtx(s, false, inControl), drcRegular("alice", 2*drcMinDelegation, "bob")); err == nil {
			t.Fatalf("inControl=%v: regular delegate_rc accepted before HF29", inControl)
		}
		if _, ok := drcDelegated(s, "alice", "bob"); ok {
			t.Fatalf("inControl=%v: delegation written by a refused operation", inControl)
		}
		if err := ApplyOp(drcCtx(s, true, inControl), drcRegular("alice", 2*drcMinDelegation, "bob")); err != nil {
			t.Fatalf("inControl=%v: regular delegate_rc at HF29: %v", inControl, err)
		}
		if got, ok := drcDelegated(s, "alice", "bob"); !ok || got != 2*drcMinDelegation {
			t.Fatalf("inControl=%v: alice→bob = %d,%v", inControl, got, ok)
		}
		if got := mustAccount(t, s, "bob").DelegatedRCIn; got != 2*drcMinDelegation {
			t.Fatalf("inControl=%v: bob received %d", inControl, got)
		}
		if got := mustAccount(t, s, "alice").DelegatedRCOut; got != 2*drcMinDelegation {
			t.Fatalf("inControl=%v: alice delegated %d", inControl, got)
		}
	}
	// validate() runs on the regular form too.
	s := drcSession(t)
	if err := ApplyOp(drcCtx(s, true, false), drcRegular("alice", 2*drcMinDelegation, "alice")); err == nil {
		t.Fatal("regular delegate_rc to self accepted")
	}
}

func mustAccount(t *testing.T, s *state.Session, name string) objects.Account {
	t.Helper()
	a, err := objects.GetAccount(s, auth.AccountName(name))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// drcOutcome applies o and reports whether it failed (error) and whether the
// alice→bob delegation is there afterwards.
type drcOutcome struct {
	failed    bool
	delegated bool
}

// TestDelegateRCStrictRulesAtHF29 is hived's rc_delegation_below_minimum_in_block
// and rc_delegation_during_pending_removal_in_block (hf29_tests.cpp): before
// HF29 the minimum delegation and the pending-removal ban are checked only in
// control, so a block can carry an effective delegation that breaks them; from
// HF29 (STRICT_RC_DELEGATION_RULES) the embedded form is a no-op in a block and
// the regular form fails the block.
func TestDelegateRCStrictRulesAtHF29(t *testing.T) {
	cases := []struct {
		name  string
		maxRC int64
		setup func(s *state.Session)
	}{
		{"below minimum", drcMinDelegation / 2, func(*state.Session) {}},
		{"pending removal", 2 * drcMinDelegation, func(s *state.Session) {
			objects.PutRCExpiredDelegation(s, &objects.RCExpiredDelegation{From: "alice", RemainingExpired: 10, ExpiryTime: 1})
		}},
	}
	for _, tc := range cases {
		run := func(hf29, inControl, regular bool) drcOutcome {
			s := drcSession(t)
			tc.setup(s)
			o := drcEmbedded("alice", tc.maxRC, "bob")
			if regular {
				o = drcRegular("alice", tc.maxRC, "bob")
			}
			err := ApplyOp(drcCtx(s, hf29, inControl), o)
			_, ok := drcDelegated(s, "alice", "bob")
			return drcOutcome{failed: err != nil, delegated: ok}
		}
		for _, c := range []struct {
			hf29, inControl, regular bool
			want                     drcOutcome
		}{
			{false, true, false, drcOutcome{failed: true}},
			{false, false, false, drcOutcome{delegated: true}}, // effective before HF29
			{true, true, false, drcOutcome{failed: true}},
			{true, false, false, drcOutcome{}}, // no-op in a block
			{true, true, true, drcOutcome{failed: true}},
			{true, false, true, drcOutcome{failed: true}}, // invalidates the block
		} {
			if got := run(c.hf29, c.inControl, c.regular); got != c.want {
				t.Errorf("%s hf29=%v inControl=%v regular=%v: got %+v want %+v",
					tc.name, c.hf29, c.inControl, c.regular, got, c.want)
			}
		}
	}
}

// TestDelegateRCBlockChecksAtHF29 covers the asserts hived runs everywhere but
// gopherd skips in a block before HF29 (A15 in docs/hived-parity-review-2026-10.md):
// over-delegation, a new delegation of 0, an update to the same amount. From
// HF29 RC is consensus and they hold in blocks too.
func TestDelegateRCBlockChecksAtHF29(t *testing.T) {
	existing := int64(5 * drcMinDelegation)
	cases := []struct {
		name  string
		maxRC int64
		to    string
	}{
		{"insufficient RC", 2 * drcAliceVests, "carol"},
		{"create with 0", 0, "carol"},
		{"same amount", existing, "bob"},
	}
	for _, tc := range cases {
		for _, c := range []struct {
			hf29, regular bool
			wantFail      bool
		}{
			{false, false, false}, // skipped in a block before HF29
			{true, false, false},  // embedded: swallowed, rolled back
			{true, true, true},    // regular: the block is invalid
		} {
			s := drcSession(t)
			if err := ApplyOp(drcCtx(s, false, true), drcEmbedded("alice", existing, "bob")); err != nil {
				t.Fatal(err)
			}
			before := mustAccount(t, s, "alice")
			o := drcEmbedded("alice", tc.maxRC, tc.to)
			if c.regular {
				o = drcRegular("alice", tc.maxRC, tc.to)
			}
			err := ApplyOp(drcCtx(s, c.hf29, false), o)
			if (err != nil) != c.wantFail {
				t.Errorf("%s hf29=%v regular=%v: err=%v, want failure %v", tc.name, c.hf29, c.regular, err, c.wantFail)
			}
			after := mustAccount(t, s, "alice")
			changed := after.DelegatedRCOut != before.DelegatedRCOut || after.RCManaCurrent != before.RCManaCurrent
			if tc.name == "insufficient RC" && !c.hf29 && !changed {
				t.Errorf("%s pre-HF29 in a block: delegation not applied", tc.name)
			}
			if c.hf29 && !c.regular && changed {
				t.Errorf("%s hf29 embedded: state changed by a failed delegation", tc.name)
			}
		}
	}
}

// TestDelegateRCDelegateeMaxRCOverflow is hived 7baaad338/1311a2389: an
// increase that would overflow the delegatee's maximum RC is refused even
// when its received RC alone would not overflow — everywhere, as the
// share_type addition it stands for throws everywhere. In a block before HF29
// that makes the embedded form a no-op rather than a wrapped maximum.
func TestDelegateRCDelegateeMaxRCOverflow(t *testing.T) {
	s := drcSession(t)
	bob := mustAccount(t, s, "bob")
	bob.VestingShares = math.MaxInt64 / 2
	bob.DelegatedRCIn = math.MaxInt64 / 2
	bob.LastMaxRC = math.MaxInt64 - 1
	objects.PutAccount(s, &bob)
	if err := ApplyOp(drcCtx(s, false, false), drcEmbedded("alice", 10, "bob")); err != nil {
		t.Fatalf("embedded overflow in a block must be swallowed: %v", err)
	}
	if _, ok := drcDelegated(s, "alice", "bob"); ok {
		t.Fatal("overflowing delegation applied")
	}
	p := DelegateRCParams{From: "alice", Delegatees: []string{"bob"}, MaxRC: 10}
	if err := applyDelegateRC(drcCtx(s, false, false), &p); !errors.Is(err, ErrRCOverflow) {
		t.Fatalf("delegatee maximum RC overflow: err = %v, want ErrRCOverflow", err)
	}
}
