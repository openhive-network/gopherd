package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Errors specific to escrow.
var (
	ErrEscrowExists       = errors.New("escrow with this id already exists")
	ErrEscrowNotFound     = errors.New("escrow not found")
	ErrEscrowAlreadyDisputed = errors.New("escrow already disputed")
	ErrEscrowNotApproved  = errors.New("escrow not yet approved by all parties")
)

type escrowTransferEvaluator struct{}

func (escrowTransferEvaluator) Apply(ctx *Context, o op.Operation) error {
	e, ok := o.(op.EscrowTransfer)
	if !ok {
		return fmt.Errorf("escrowTransferEvaluator: unexpected op type %T", o)
	}
	if e.Fee.Symbol.Num != asset.HiveSymbol {
		return fmt.Errorf("%w: escrow fee must be HIVE", ErrUnsupportedSymbol)
	}
	if e.HiveAmount.Symbol.Num != asset.HiveSymbol || e.HbdAmount.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: escrow_transfer hive_amount must be HIVE and hbd_amount HBD", ErrUnsupportedSymbol)
	}
	if e.HiveAmount.Amount < 0 || e.HbdAmount.Amount < 0 || e.Fee.Amount < 0 {
		return ErrTransferAmountNotPos
	}

	from, err := objects.GetAccount(ctx.DB, e.From)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: from=%q", ErrAccountNotFound, e.From)
		}
		return err
	}
	if from.Balance < e.HiveAmount.Amount+e.Fee.Amount {
		return fmt.Errorf("%w: %q insufficient HIVE", ErrInsufficientBalance, e.From)
	}
	if from.HBDBalance < e.HbdAmount.Amount {
		return fmt.Errorf("%w: %q insufficient HBD", ErrInsufficientBalance, e.From)
	}
	if has, err := objects.HasEscrow(ctx.DB, e.From, e.EscrowID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrEscrowExists, e.From, e.EscrowID)
	}

	from.Balance -= e.HiveAmount.Amount + e.Fee.Amount
	from.HBDBalance -= e.HbdAmount.Amount
	objects.PutAccount(ctx.DB, &from)

	objects.PutEscrow(ctx.DB, &objects.Escrow{
		From:                 e.From,
		To:                   e.To,
		Agent:                e.Agent,
		EscrowID:             e.EscrowID,
		HBDAmount:            e.HbdAmount.Amount,
		HiveAmount:           e.HiveAmount.Amount,
		PendingFee:           e.Fee.Amount,
		RatificationDeadline: e.RatificationDeadline,
		EscrowExpiration:     e.EscrowExpiration,
	})
	return nil
}

type escrowApproveEvaluator struct{}

func (escrowApproveEvaluator) Apply(ctx *Context, o op.Operation) error {
	a, ok := o.(op.EscrowApprove)
	if !ok {
		return fmt.Errorf("escrowApproveEvaluator: unexpected op type %T", o)
	}
	esc, err := objects.GetEscrow(ctx.DB, a.From, a.EscrowID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%d", ErrEscrowNotFound, a.From, a.EscrowID)
		}
		return err
	}
	if a.Approve {
		switch a.Who {
		case esc.To:
			esc.ToApproved = true
		case esc.Agent:
			esc.AgentApproved = true
		default:
			return fmt.Errorf("escrow_approve: %q is not to or agent", a.Who)
		}
		// On full approval the agent receives the fee.
		if esc.ToApproved && esc.AgentApproved && esc.PendingFee > 0 {
			agent, err := objects.GetAccount(ctx.DB, esc.Agent)
			if err == nil {
				agent.Balance += esc.PendingFee
				objects.PutAccount(ctx.DB, &agent)
			}
			esc.PendingFee = 0
		}
		objects.PutEscrow(ctx.DB, &esc)
		return nil
	}
	// Rejection: refund the from account and delete the escrow.
	from, err := objects.GetAccount(ctx.DB, esc.From)
	if err != nil {
		return err
	}
	from.Balance += esc.HiveAmount + esc.PendingFee
	from.HBDBalance += esc.HBDAmount
	objects.PutAccount(ctx.DB, &from)
	objects.DeleteEscrow(ctx.DB, a.From, a.EscrowID)
	return nil
}

type escrowDisputeEvaluator struct{}

func (escrowDisputeEvaluator) Apply(ctx *Context, o op.Operation) error {
	d, ok := o.(op.EscrowDispute)
	if !ok {
		return fmt.Errorf("escrowDisputeEvaluator: unexpected op type %T", o)
	}
	esc, err := objects.GetEscrow(ctx.DB, d.From, d.EscrowID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%d", ErrEscrowNotFound, d.From, d.EscrowID)
		}
		return err
	}
	if esc.Disputed {
		return ErrEscrowAlreadyDisputed
	}
	esc.Disputed = true
	objects.PutEscrow(ctx.DB, &esc)
	return nil
}

type escrowReleaseEvaluator struct{}

func (escrowReleaseEvaluator) Apply(ctx *Context, o op.Operation) error {
	r, ok := o.(op.EscrowRelease)
	if !ok {
		return fmt.Errorf("escrowReleaseEvaluator: unexpected op type %T", o)
	}
	esc, err := objects.GetEscrow(ctx.DB, r.From, r.EscrowID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%d", ErrEscrowNotFound, r.From, r.EscrowID)
		}
		return err
	}
	if r.HiveAmount.Symbol.Num != asset.HiveSymbol || r.HbdAmount.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: escrow_release amounts", ErrUnsupportedSymbol)
	}
	if r.HiveAmount.Amount > esc.HiveAmount || r.HbdAmount.Amount > esc.HBDAmount {
		return fmt.Errorf("%w: release exceeds escrow", ErrInsufficientBalance)
	}

	receiver, err := objects.GetAccount(ctx.DB, r.Receiver)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: receiver=%q", ErrAccountNotFound, r.Receiver)
		}
		return err
	}
	receiver.Balance += r.HiveAmount.Amount
	receiver.HBDBalance += r.HbdAmount.Amount
	objects.PutAccount(ctx.DB, &receiver)

	esc.HiveAmount -= r.HiveAmount.Amount
	esc.HBDAmount -= r.HbdAmount.Amount
	if esc.HiveAmount == 0 && esc.HBDAmount == 0 {
		objects.DeleteEscrow(ctx.DB, r.From, r.EscrowID)
	} else {
		objects.PutEscrow(ctx.DB, &esc)
	}
	return nil
}

func init() {
	register(op.TagEscrowTransfer, escrowTransferEvaluator{})
	register(op.TagEscrowApprove, escrowApproveEvaluator{})
	register(op.TagEscrowDispute, escrowDisputeEvaluator{})
	register(op.TagEscrowRelease, escrowReleaseEvaluator{})
}
