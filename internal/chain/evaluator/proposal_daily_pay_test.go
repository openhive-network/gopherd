package evaluator

import (
	"strings"
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state/objects"
)

// hived 86d893b0b: create_proposal refuses daily_pay above
// HIVE_PROPOSAL_MAX_DAILY_PAY in control and from HF29; in pre-HF29 blocks it
// is still valid.
func TestCreateProposalDailyPayCap(t *testing.T) {
	s := seedProposals(t, nil, nil)
	mk := func(pay int64) op.CreateProposal {
		return op.CreateProposal{
			Creator: "alice", Receiver: "alice",
			StartDate: proposalTestHeadTime, EndDate: proposalTestHeadTime + 86400,
			DailyPay: asset.Asset{Amount: pay, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}},
			Subject:  "s", Permlink: "p",
		}
	}
	tooHigh := func(err error) bool { return err != nil && strings.Contains(err.Error(), "too high") }
	for _, tc := range []struct {
		name      string
		inControl bool
		hf29      bool
		pay       int64
		want      bool
	}{
		{"block pre-HF29, over cap", false, false, objects.ProposalMaxDailyPay + 1, false},
		{"in control, over cap", true, false, objects.ProposalMaxDailyPay + 1, true},
		{"HF29 block, over cap", false, true, objects.ProposalMaxDailyPay + 1, true},
		{"HF29 block, at cap", false, true, objects.ProposalMaxDailyPay, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &Context{DB: s, Hardforks: preHF29Set(), HeadBlockTime: proposalTestHeadTime, InControl: tc.inControl}
			if tc.hf29 {
				ctx.Hardforks = hf29Set()
			}
			if got := tooHigh(Apply(ctx, mk(tc.pay))); got != tc.want {
				t.Fatalf("daily_pay %d refused as too high=%v, want %v", tc.pay, got, tc.want)
			}
		})
	}
}

// The HF29 hook clamps every proposal, #0 included.
func TestClampProposalDailyPays(t *testing.T) {
	s := seedProposals(t, []string{"hive.fund", "alice", "bob"}, []int{0, 0, 0})
	for id, pay := range []int64{objects.ProposalMaxDailyPay * 1000, 5, objects.ProposalMaxDailyPay + 1} {
		p, _ := objects.GetProposal(s, int64(id))
		p.DailyPay = pay
		objects.PutProposal(s, &p)
	}
	if n := objects.ClampProposalDailyPays(s); n != 2 {
		t.Fatalf("clamped %d proposals, want 2", n)
	}
	for id, want := range []int64{objects.ProposalMaxDailyPay, 5, objects.ProposalMaxDailyPay} {
		if p, _ := objects.GetProposal(s, int64(id)); p.DailyPay != want {
			t.Fatalf("proposal %d daily_pay=%d, want %d", id, p.DailyPay, want)
		}
	}
}
