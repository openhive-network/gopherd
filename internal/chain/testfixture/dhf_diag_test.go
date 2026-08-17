package testfixture

import (
	"fmt"
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

func TestDHFDiag(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob", "carol")
	f.FundLiquidHBD("alice", 1_000_000)
	f.FundVesting("carol", 100_000_000)
	f.GenerateBlock()
	seedTreasury(t, f, "steem.dao", 100_000_000)
	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.CreateProposal{Creator: "alice", Receiver: "bob",
		StartDate: now - 60, EndDate: now + 60*60*24*30,
		DailyPay: HBD(10_000), Subject: "x", Permlink: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := f.ApplyOp(op.UpdateProposalVotes{Voter: "carol", ProposalIDs: []int64{0}, Approve: true}); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("carol vests=%d dgpoNDM=%d head=%d time=%d\n", f.Vests("carol"), f.DGPO().NextMaintenanceTime, f.BlockNum, f.HeadBlockTime())
	s := state.NewSession(f.State)
	_ = s.Range(objects.ProposalTable, state.PrimaryIndex, nil, func(_, v []byte) (bool, error) {
		p, _ := objects.ReadProposal(v)
		fmt.Printf("proposal id=%d votes=%d start=%d end=%d pay=%d\n", p.ID, p.TotalVotes, p.StartDate, p.EndDate, p.DailyPay)
		return true, nil
	})
	skip := proposalMaintenanceUntilNext(f)
	fmt.Printf("skip=%d\n", skip)
	f.SkipSeconds(skip)
	fmt.Printf("after skip: bob=%d treasury=%d NDM=%d time=%d\n", f.HBDBalance("bob"), f.HBDBalance("steem.dao"), f.DGPO().NextMaintenanceTime, f.HeadBlockTime())
	s2 := state.NewSession(f.State)
	_ = s2.Range(objects.ProposalTable, state.PrimaryIndex, nil, func(_, v []byte) (bool, error) {
		p, _ := objects.ReadProposal(v)
		fmt.Printf("post proposal id=%d votes=%d\n", p.ID, p.TotalVotes)
		return true, nil
	})
}
