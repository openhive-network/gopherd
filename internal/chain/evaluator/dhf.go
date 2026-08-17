package evaluator

import (
	"errors"
	"fmt"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Errors specific to DHF.
var (
	ErrProposalNotFound      = errors.New("proposal not found")
	ErrProposalNotOwnedByYou = errors.New("only the proposal creator may modify or remove it")
)

// proposalIDCounter is a simple monotonic counter used as the next
// proposal_id when an evaluator creates a Proposal. Hived uses an
// auto-incremented field on dynamic_global_property_object; we keep a
// dedicated counter row in the DGPO table at sub-key 1 to avoid
// expanding DGPO's main schema. NOT consensus-correct relative to
// hived's exact id assignment but sufficient to make the apply
// pipeline drive forward.
const dhfProposalCounterIndex state.IndexID = 2

func nextProposalID(ctx *Context) int64 {
	v, err := ctx.DB.Get(objects.DynamicGlobalPropertyTable, dhfProposalCounterIndex, []byte{0})
	var id int64
	if err == nil && len(v) >= 8 {
		for i := 0; i < 8; i++ {
			id |= int64(v[i]) << (i * 8)
		}
	}
	// chainbase assigns each new object the CURRENT counter value, then
	// increments — hived's first proposal_object has id=0. Go was
	// pre-incrementing (id++ before assigning), making its first proposal
	// id=1; every update_proposal_votes referencing hived's 0-based ids
	// then missed. Post-increment: return `id`, persist id+1. First
	// mainnet hit: update_proposal_votes id=0 at block 35,921,808 (22
	// blocks after HF21), voting on the chain's first proposal.
	next := id + 1
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[i] = byte(next >> (i * 8))
	}
	ctx.DB.Put(objects.DynamicGlobalPropertyTable, dhfProposalCounterIndex, []byte{0}, out)
	return id
}

type createProposalEvaluator struct{}

// HIVE_TREASURY_FEE = 10 HBD (10000 sat). HIVE_PROPOSAL_FEE_INCREASE_*
// constants kick in for HF24+ when proposal duration > 60 days.
const (
	treasuryFeeHBD             int64 = 10000
	proposalFeeIncreaseDays          = 60
	proposalFeeIncreaseDaysSec       = proposalFeeIncreaseDays * 86400
	proposalFeeIncreaseAmount  int64 = 1000 // +1 HBD per extra day
	oneDaySeconds                    = 86400
)

func (createProposalEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagCreateProposal {
		return fmt.Errorf("createProposalEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	cp := opArg.AsCreateProposal()
	// HF21__3215 — DHF activates at HF21.
	if !ctx.Hardforks.Has(hardfork.HF21) {
		return fmt.Errorf("create_proposal: requires HF21")
	}
	if err := cp.Validate(); err != nil {
		return err
	}
	if cp.DailyPay.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: daily_pay must be HBD", ErrUnsupportedSymbol)
	}
	if cp.DailyPay.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	// end_date must be in the future of head_block_time
	// (dhf_evaluator.cpp:43). start_date can predate head_time slightly.
	if cp.EndDate <= ctx.HeadBlockTime {
		return fmt.Errorf("create_proposal: end_date must be > head_block_time")
	}
	if cp.StartDate >= cp.EndDate {
		return fmt.Errorf("create_proposal: start_date must precede end_date")
	}

	// HF24+ fee scaling: proposals with duration > 60 days pay +1 HBD per
	// extra day (dhf_evaluator.cpp:47-56).
	feeHBD := treasuryFeeHBD
	if ctx.Hardforks.Has(hardfork.HF24) {
		runTime := uint32(0)
		if cp.EndDate > cp.StartDate {
			runTime = cp.EndDate - cp.StartDate
		}
		if runTime > proposalFeeIncreaseDaysSec {
			extraDays := int64(runTime/oneDaySeconds) - proposalFeeIncreaseDays
			if extraDays > 0 {
				feeHBD += proposalFeeIncreaseAmount * extraDays
			}
		}
	}

	// Treasury account must exist (dhf_evaluator.cpp:59). Resolve name
	// at the current hardfork — pre-HF24 = "steem.dao", HF24+ = "hive.fund".
	treasuryName := "steem.dao"
	if ctx.Hardforks.Has(hardfork.HF24) {
		treasuryName = "hive.fund"
	}
	treasury, terr := objects.GetAccount(ctx.DB, treasuryName)
	if terr != nil {
		return fmt.Errorf("create_proposal: treasury account %q missing", treasuryName)
	}

	creator, cerr := objects.GetAccount(ctx.DB, cp.Creator)
	if cerr != nil {
		return fmt.Errorf("%w: creator=%q", ErrAccountNotFound, cp.Creator)
	}
	if has, err := objects.HasAccount(ctx.DB, cp.Receiver); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: receiver=%q", ErrAccountNotFound, cp.Receiver)
	}
	if creator.HBDBalance < feeHBD {
		return fmt.Errorf("create_proposal: creator %q has insufficient HBD for treasury fee (%d < %d)",
			cp.Creator, creator.HBDBalance, feeHBD)
	}

	id := nextProposalID(ctx)
	objects.PutProposal(ctx.DB, &objects.Proposal{
		ID:        id,
		Creator:   cp.Creator,
		Receiver:  cp.Receiver,
		StartDate: cp.StartDate,
		EndDate:   cp.EndDate,
		DailyPay:  cp.DailyPay.Amount,
		Subject:   cp.Subject,
		Permlink:  cp.Permlink,
	})

	// Pay the fee: debit creator, credit treasury (dhf_evaluator.cpp:94-96).
	objects.AdjustHBDBalance(ctx.DB, &creator, ctx.HeadBlockTime, -feeHBD)
	objects.AdjustHBDBalance(ctx.DB, &treasury, ctx.HeadBlockTime, feeHBD)
	objects.PutAccount(ctx.DB, &creator)
	objects.PutAccount(ctx.DB, &treasury)

	// hived dhf_evaluator.cpp:97 — push proposal_fee_operation virtual op.
	ctx.EmitProposalFee(cp.Creator, treasuryName, id, feeHBD)

	return nil
}

type updateProposalVotesEvaluator struct{}

func (updateProposalVotesEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagUpdateProposalVotes {
		return fmt.Errorf("updateProposalVotesEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	uv := opArg.AsUpdateProposalVotes()
	if !ctx.Hardforks.Has(hardfork.HF21) {
		return fmt.Errorf("update_proposal_votes: requires HF21")
	}
	if err := uv.Validate(); err != nil {
		return err
	}
	voter, err := objects.GetAccount(ctx.DB, uv.Voter)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: voter=%q", ErrAccountNotFound, uv.Voter)
		}
		return err
	}
	// dhf_evaluator.cpp:169 — UNCONDITIONAL refresh of governance vote
	// expiration on every proposal vote, with the HF25 redistribution
	// formula applied to votes that would otherwise expire before
	// HARDFORK_1_25_FIRST_GOVERNANCE_VOTE_EXPIRE_TIMESTAMP.
	updateGovernanceVoteExpirationTs(&voter, ctx.HeadBlockTime)
	objects.PutAccount(ctx.DB, &voter)
	for _, id := range uv.ProposalIDs {
		p, err := objects.GetProposal(ctx.DB, id)
		if err != nil {
			if err == state.ErrNotFound {
				// dhf_evaluator.cpp:178 — only HF28+
				// (DONT_TRY_VOTE_FOR_NONEXISTENT_PROPOSAL) asserts on a
				// missing proposal; pre-HF28 hived silently `continue`s
				// (skips this id). Go was erroring unconditionally.
				if ctx.Hardforks.Has(hardfork.HF28) {
					return fmt.Errorf("%w: id=%d", ErrProposalNotFound, id)
				}
				continue
			}
			return err
		}
		// dhf_evaluator.cpp:183-190 — the expiry assert
		// (HIVE_CHAIN_TIME_ASSERT(head_block_time <= end_date)) is gated on
		// HIVE_HARDFORK_1_25 and is NOT gated on approve. Pre-HF25 a vote on
		// an expired proposal is ALLOWED (proposal 16's approve at block
		// 36,196,594 — pre-HF25 — is accepted by hived; Go was rejecting it).
		// At HF25+ reject when head_time > end_date (equality is allowed).
		if ctx.Hardforks.Has(hardfork.HF25) && ctx.HeadBlockTime > p.EndDate {
			return fmt.Errorf("update_proposal_votes: proposal %d has expired (end_date=%d, head=%d)",
				id, p.EndDate, ctx.HeadBlockTime)
		}
		had, _ := objects.HasProposalVote(ctx.DB, id, uv.Voter)
		if uv.Approve && !had {
			objects.PutProposalVote(ctx.DB, id, uv.Voter)
		} else if !uv.Approve && had {
			objects.DeleteProposalVote(ctx.DB, id, uv.Voter)
		}
		// Mirror C++ dhf_evaluator.cpp: update_proposal_votes_evaluator
		// only mutates the vote-relation table — it does NOT touch
		// p.total_votes. The maintenance tick recalculates from scratch
		// via calculate_votes(). Gopherd previously updated TotalVotes
		// inline using `voter.VestingShares` (a stale/wrong formula:
		// missing sum_delayed_votes subtraction and proxied_vsf_votes
		// addition), which then got overwritten by the next maintenance
		// recalc — producing transient ordering divergence in any
		// between-tick read.
		_ = voter // signature retained; voter check above is still required
		_ = p
	}
	return nil
}

type removeProposalEvaluator struct{}

func (removeProposalEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagRemoveProposal {
		return fmt.Errorf("removeProposalEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	rp := opArg.AsRemoveProposal()
	if !ctx.Hardforks.Has(hardfork.HF21) {
		return fmt.Errorf("remove_proposal: requires HF21")
	}
	if err := rp.Validate(); err != nil {
		return err
	}
	for _, id := range rp.ProposalIDs {
		p, err := objects.GetProposal(ctx.DB, id)
		if err != nil {
			if err == state.ErrNotFound {
				// HIVE_HARDFORK_1_28_DONT_TRY_REMOVE_NONEXISTENT_PROPOSAL
				// (dhf_helper.cpp): HF28+ asserts on a missing proposal id;
				// pre-HF28 silently skips it (continue).
				if ctx.Hardforks.Has(hardfork.HF28) {
					return fmt.Errorf("%w: id=%d", ErrProposalNotFound, id)
				}
				continue
			}
			return err
		}
		if p.Creator != rp.ProposalOwner {
			return fmt.Errorf("%w: id=%d", ErrProposalNotOwnedByYou, id)
		}
		// C++ on_proposal_remove (database_dhf.cpp) cascades the delete
		// to every ProposalVote row for the removed proposal_id. The
		// proposal_vote primary key is (int64BE(id), account_name), so a
		// prefix Range over the id half enumerates exactly the voter
		// rows attached to this proposal.
		objects.DeleteAllProposalVotesForProposal(ctx.DB, id)
		objects.DeleteProposalFull(ctx.DB, &p)
	}
	return nil
}

type updateProposalEvaluator struct{}

func (updateProposalEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagUpdateProposal {
		return fmt.Errorf("updateProposalEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	up := opArg.AsUpdateProposal()
	// HF24__71 — update_proposal is HF24+ only.
	if !ctx.Hardforks.Has(hardfork.HF24) {
		return fmt.Errorf("update_proposal: requires HF24")
	}
	p, err := objects.GetProposal(ctx.DB, up.ProposalID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: id=%d", ErrProposalNotFound, up.ProposalID)
		}
		return err
	}
	if p.Creator != up.Creator {
		return fmt.Errorf("%w: id=%d", ErrProposalNotOwnedByYou, up.ProposalID)
	}
	if up.DailyPay.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: daily_pay must be HBD", ErrUnsupportedSymbol)
	}
	if up.DailyPay.Amount > p.DailyPay {
		return fmt.Errorf("update_proposal: daily_pay can only decrease")
	}
	p.DailyPay = up.DailyPay.Amount
	p.Subject = up.Subject
	p.Permlink = up.Permlink
	objects.PutProposal(ctx.DB, &p)
	return nil
}

func init() {
	register(op.TagCreateProposal, createProposalEvaluator{})
	register(op.TagUpdateProposalVotes, updateProposalVotesEvaluator{})
	register(op.TagRemoveProposal, removeProposalEvaluator{})
	register(op.TagUpdateProposal, updateProposalEvaluator{})
}
