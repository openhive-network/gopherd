package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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
	id++
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[i] = byte(id >> (i * 8))
	}
	ctx.DB.Put(objects.DynamicGlobalPropertyTable, dhfProposalCounterIndex, []byte{0}, out)
	return id
}

type createProposalEvaluator struct{}

func (createProposalEvaluator) Apply(ctx *Context, o op.Operation) error {
	cp, ok := o.(op.CreateProposal)
	if !ok {
		return fmt.Errorf("createProposalEvaluator: unexpected op type %T", o)
	}
	if cp.DailyPay.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: daily_pay must be HBD", ErrUnsupportedSymbol)
	}
	if cp.DailyPay.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	if cp.StartDate >= cp.EndDate {
		return fmt.Errorf("create_proposal: start_date must precede end_date")
	}
	if has, err := objects.HasAccount(ctx.DB, cp.Creator); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: creator=%q", ErrAccountNotFound, cp.Creator)
	}
	if has, err := objects.HasAccount(ctx.DB, cp.Receiver); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: receiver=%q", ErrAccountNotFound, cp.Receiver)
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
	return nil
}

type updateProposalVotesEvaluator struct{}

func (updateProposalVotesEvaluator) Apply(ctx *Context, o op.Operation) error {
	uv, ok := o.(op.UpdateProposalVotes)
	if !ok {
		return fmt.Errorf("updateProposalVotesEvaluator: unexpected op type %T", o)
	}
	voter, err := objects.GetAccount(ctx.DB, uv.Voter)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: voter=%q", ErrAccountNotFound, uv.Voter)
		}
		return err
	}
	for _, id := range uv.ProposalIDs {
		p, err := objects.GetProposal(ctx.DB, id)
		if err != nil {
			if err == state.ErrNotFound {
				return fmt.Errorf("%w: id=%d", ErrProposalNotFound, id)
			}
			return err
		}
		had, _ := objects.HasProposalVote(ctx.DB, id, uv.Voter)
		if uv.Approve && !had {
			objects.PutProposalVote(ctx.DB, id, uv.Voter)
			p.TotalVotes += voter.VestingShares
			objects.PutProposal(ctx.DB, &p)
		} else if !uv.Approve && had {
			objects.DeleteProposalVote(ctx.DB, id, uv.Voter)
			p.TotalVotes -= voter.VestingShares
			if p.TotalVotes < 0 {
				p.TotalVotes = 0
			}
			objects.PutProposal(ctx.DB, &p)
		}
	}
	return nil
}

type removeProposalEvaluator struct{}

func (removeProposalEvaluator) Apply(ctx *Context, o op.Operation) error {
	rp, ok := o.(op.RemoveProposal)
	if !ok {
		return fmt.Errorf("removeProposalEvaluator: unexpected op type %T", o)
	}
	for _, id := range rp.ProposalIDs {
		p, err := objects.GetProposal(ctx.DB, id)
		if err != nil {
			if err == state.ErrNotFound {
				continue
			}
			return err
		}
		if p.Creator != rp.ProposalOwner {
			return fmt.Errorf("%w: id=%d", ErrProposalNotOwnedByYou, id)
		}
		objects.DeleteProposal(ctx.DB, id)
	}
	return nil
}

type updateProposalEvaluator struct{}

func (updateProposalEvaluator) Apply(ctx *Context, o op.Operation) error {
	up, ok := o.(op.UpdateProposal)
	if !ok {
		return fmt.Errorf("updateProposalEvaluator: unexpected op type %T", o)
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
