package evaluator

import (
	"errors"
	"fmt"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Errors specific to escrow.
var (
	ErrEscrowExists          = errors.New("escrow with this id already exists")
	ErrEscrowNotFound        = errors.New("escrow not found")
	ErrEscrowAlreadyDisputed = errors.New("escrow already disputed")
	ErrEscrowNotApproved     = errors.New("escrow not yet approved by all parties")
	ErrEscrowExpired         = errors.New("escrow ratification deadline passed")
	ErrEscrowMismatch        = errors.New("operation field does not match escrow")
	ErrEscrowAlreadyApproved = errors.New("escrow already approved by this party")
)

// MaxPendingTransfers mirrors HIVE_MAX_PENDING_TRANSFERS from
// libraries/protocol/include/hive/protocol/config.hpp:240 — the maximum
// number of simultaneously-open escrow_transfer rows a single from
// account may originate.
const MaxPendingTransfers = 255

type escrowTransferEvaluator struct{}

func (escrowTransferEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagEscrowTransfer {
		return fmt.Errorf("escrowTransferEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	e := opArg.AsEscrowTransfer()
	if e.Fee.Symbol.Num != asset.HiveSymbol && e.Fee.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: escrow fee must be HIVE or HBD", ErrUnsupportedSymbol)
	}
	if e.HiveAmount.Symbol.Num != asset.HiveSymbol || e.HbdAmount.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: escrow_transfer hive_amount must be HIVE and hbd_amount HBD", ErrUnsupportedSymbol)
	}
	if e.HiveAmount.Amount < 0 || e.HbdAmount.Amount < 0 || e.Fee.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	// hive_evaluator_transfer.cpp:84-85 — both deadlines must be in the
	// future of head_block_time.
	if e.RatificationDeadline <= ctx.HeadBlockTime {
		return fmt.Errorf("escrow_transfer: ratification_deadline must be > head_block_time")
	}
	if e.EscrowExpiration <= ctx.HeadBlockTime {
		return fmt.Errorf("escrow_transfer: escrow_expiration must be > head_block_time")
	}

	from, err := objects.GetAccount(ctx.DB, e.From)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: from=%q", ErrAccountNotFound, e.From)
		}
		return err
	}
	// Verify to + agent exist (hive_evaluator_transfer.cpp:81-82).
	if has, err := objects.HasAccount(ctx.DB, e.To); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, e.To)
	}
	if has, err := objects.HasAccount(ctx.DB, e.Agent); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: agent=%q", ErrAccountNotFound, e.Agent)
	}

	// hive_evaluator_transfer.cpp:92-95 — fee can be HIVE or HBD; the
	// from-side debit needs to bucket the fee accordingly.
	hiveSpent := e.HiveAmount.Amount
	hbdSpent := e.HbdAmount.Amount
	if e.Fee.Symbol.Num == asset.HiveSymbol {
		hiveSpent += e.Fee.Amount
	} else {
		hbdSpent += e.Fee.Amount
	}
	if from.Balance < hiveSpent {
		return fmt.Errorf("%w: %q insufficient HIVE", ErrInsufficientBalance, e.From)
	}
	if from.HBDBalance < hbdSpent {
		return fmt.Errorf("%w: %q insufficient HBD", ErrInsufficientBalance, e.From)
	}
	if has, err := objects.HasEscrow(ctx.DB, e.From, e.EscrowID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrEscrowExists, e.From, e.EscrowID)
	}
	// hive_evaluator_transfer.cpp:86 — enforce HIVE_MAX_PENDING_TRANSFERS
	// (255) per from-account; the C++ side persists a uint8
	// pending_escrow_transfers counter on account_object. We instead
	// count rows directly in the escrow_object primary index, which
	// avoids extending the account schema.
	n, err := objects.CountEscrowsByFrom(ctx.DB, e.From)
	if err != nil {
		return err
	}
	if n >= MaxPendingTransfers {
		return fmt.Errorf("escrow_transfer: %q already has the maximum (%d) open escrows", e.From, MaxPendingTransfers)
	}

	from.Balance -= hiveSpent
	objects.AdjustHBDBalance(ctx.DB, &from, ctx.HeadBlockTime, -hbdSpent)
	objects.PutAccount(ctx.DB, &from)

	objects.PutEscrow(ctx.DB, &objects.Escrow{
		From:                 e.From,
		To:                   e.To,
		Agent:                e.Agent,
		EscrowID:             e.EscrowID,
		HBDAmount:            e.HbdAmount.Amount,
		HiveAmount:           e.HiveAmount.Amount,
		PendingFee:           e.Fee.Amount,
		PendingFeeIsHBD:      e.Fee.Symbol.Num == asset.HbdSymbol,
		RatificationDeadline: e.RatificationDeadline,
		EscrowExpiration:     e.EscrowExpiration,
	})
	return nil
}

type escrowApproveEvaluator struct{}

func (escrowApproveEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagEscrowApprove {
		return fmt.Errorf("escrowApproveEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	a := opArg.AsEscrowApprove()
	esc, err := objects.GetEscrow(ctx.DB, a.From, a.EscrowID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%d", ErrEscrowNotFound, a.From, a.EscrowID)
		}
		return err
	}
	// hive_evaluator_transfer.cpp:116-118 — to/agent must match the
	// escrow record, and ratification must not have passed.
	if a.To != esc.To {
		return fmt.Errorf("%w: to=%q escrow_to=%q", ErrEscrowMismatch, a.To, esc.To)
	}
	if a.Agent != esc.Agent {
		return fmt.Errorf("%w: agent=%q escrow_agent=%q", ErrEscrowMismatch, a.Agent, esc.Agent)
	}
	if esc.RatificationDeadline < ctx.HeadBlockTime {
		return ErrEscrowExpired
	}
	rejectEscrow := !a.Approve

	// hive_evaluator_transfer.cpp:122-145 — independent branches for
	// who==to / who==agent; each branch asserts the matching flag is
	// not yet set before flipping it.
	if a.Who == esc.To {
		if esc.ToApproved {
			return fmt.Errorf("%w: to=%q", ErrEscrowAlreadyApproved, a.To)
		}
		if !rejectEscrow {
			esc.ToApproved = true
		}
	}
	if a.Who == esc.Agent {
		if esc.AgentApproved {
			return fmt.Errorf("%w: agent=%q", ErrEscrowAlreadyApproved, a.Agent)
		}
		if !rejectEscrow {
			esc.AgentApproved = true
		}
	}

	if rejectEscrow {
		// Refund all balances back to from, decrement
		// pending_escrow_transfers, and remove the row.
		from, err := objects.GetAccount(ctx.DB, esc.From)
		if err != nil {
			return err
		}
		from.Balance += esc.HiveAmount
		// Fee refund: credit the asset symbol that was originally locked
		// (PendingFeeIsHBD tracked at escrow_transfer time). Pre-fix
		// always credited HIVE which silently miscredited HBD-fee
		// escrows.
		if esc.PendingFeeIsHBD {
			objects.AdjustHBDBalance(ctx.DB, &from, ctx.HeadBlockTime, esc.PendingFee+esc.HBDAmount)
		} else {
			from.Balance += esc.PendingFee
			objects.AdjustHBDBalance(ctx.DB, &from, ctx.HeadBlockTime, esc.HBDAmount)
		}
		objects.PutAccount(ctx.DB, &from)
		ctx.EmitEscrowRejected(string(esc.From), string(esc.To), string(esc.Agent), esc.EscrowID,
			esc.HBDAmount, esc.HiveAmount, esc.PendingFee, esc.PendingFeeIsHBD)
		objects.DeleteEscrowFull(ctx.DB, &esc)
		return nil
	}
	// On full approval (both flags set) the agent collects the fee in
	// whichever asset symbol was locked at escrow_transfer time.
	if esc.ToApproved && esc.AgentApproved {
		releasedFee := esc.PendingFee
		if releasedFee > 0 {
			agent, err := objects.GetAccount(ctx.DB, esc.Agent)
			if err == nil {
				if esc.PendingFeeIsHBD {
					objects.AdjustHBDBalance(ctx.DB, &agent, ctx.HeadBlockTime, releasedFee)
				} else {
					agent.Balance += releasedFee
				}
				objects.PutAccount(ctx.DB, &agent)
			}
			esc.PendingFee = 0
		}
		// Not gated on a non-zero fee: hived pushes this whenever both
		// parties have ratified (hive_evaluator_transfer.cpp:167-177). The
		// op reports that the escrow went ACTIVE, and a zero-fee escrow
		// going active is as much news as any other.
		ctx.EmitEscrowApproved(string(esc.From), string(esc.To), string(esc.Agent), esc.EscrowID,
			releasedFee, esc.PendingFeeIsHBD)
	}
	objects.PutEscrow(ctx.DB, &esc)
	return nil
}

type escrowDisputeEvaluator struct{}

func (escrowDisputeEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagEscrowDispute {
		return fmt.Errorf("escrowDisputeEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	d := opArg.AsEscrowDispute()
	// Verify from account exists (hive_evaluator_transfer.cpp:182).
	if has, err := objects.HasAccount(ctx.DB, d.From); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: from=%q", ErrAccountNotFound, d.From)
	}
	esc, err := objects.GetEscrow(ctx.DB, d.From, d.EscrowID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%d", ErrEscrowNotFound, d.From, d.EscrowID)
		}
		return err
	}
	// hive_evaluator_transfer.cpp:185-189 — must be before expiration,
	// fully approved, not already disputed, and to/agent must match.
	if ctx.HeadBlockTime >= esc.EscrowExpiration {
		return fmt.Errorf("escrow_dispute: dispute must happen before escrow_expiration")
	}
	if !(esc.ToApproved && esc.AgentApproved) {
		return ErrEscrowNotApproved
	}
	if esc.Disputed {
		return ErrEscrowAlreadyDisputed
	}
	if d.To != esc.To {
		return fmt.Errorf("%w: to=%q escrow_to=%q", ErrEscrowMismatch, d.To, esc.To)
	}
	if d.Agent != esc.Agent {
		return fmt.Errorf("%w: agent=%q escrow_agent=%q", ErrEscrowMismatch, d.Agent, esc.Agent)
	}
	esc.Disputed = true
	objects.PutEscrow(ctx.DB, &esc)
	return nil
}

type escrowReleaseEvaluator struct{}

func (escrowReleaseEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagEscrowRelease {
		return fmt.Errorf("escrowReleaseEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	r := opArg.AsEscrowRelease()
	// Verify from account exists.
	if has, err := objects.HasAccount(ctx.DB, r.From); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: from=%q", ErrAccountNotFound, r.From)
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
	// hive_evaluator_transfer.cpp:211-214 — to/agent must match,
	// receiver must be from or to, and the escrow must be fully
	// approved.
	if r.To != esc.To {
		return fmt.Errorf("%w: to=%q escrow_to=%q", ErrEscrowMismatch, r.To, esc.To)
	}
	if r.Agent != esc.Agent {
		return fmt.Errorf("%w: agent=%q escrow_agent=%q", ErrEscrowMismatch, r.Agent, esc.Agent)
	}
	if r.Receiver != esc.From && r.Receiver != esc.To {
		return fmt.Errorf("escrow_release: receiver must be 'from' (%q) or 'to' (%q)", esc.From, esc.To)
	}
	if !(esc.ToApproved && esc.AgentApproved) {
		return ErrEscrowNotApproved
	}
	// hive_evaluator_transfer.cpp:217-237 — permission matrix:
	//   disputed:        only agent can release, to either party
	//   not disputed, not expired:
	//     who=from → receiver must be to
	//     who=to   → receiver must be from
	//   expired and not disputed: either party can release to either
	if esc.Disputed {
		if r.Who != esc.Agent {
			return fmt.Errorf("escrow_release: only agent (%q) can release a disputed escrow", esc.Agent)
		}
	} else {
		if r.Who != esc.From && r.Who != esc.To {
			return fmt.Errorf("escrow_release: only 'from' (%q) or 'to' (%q) can release a non-disputed escrow", esc.From, esc.To)
		}
		if esc.EscrowExpiration > ctx.HeadBlockTime {
			if r.Who == esc.From && r.Receiver != esc.To {
				return fmt.Errorf("escrow_release: 'from' may only release to 'to' (%q)", esc.To)
			}
			if r.Who == esc.To && r.Receiver != esc.From {
				return fmt.Errorf("escrow_release: 'to' may only release to 'from' (%q)", esc.From)
			}
		}
	}

	receiver, err := objects.GetAccount(ctx.DB, r.Receiver)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: receiver=%q", ErrAccountNotFound, r.Receiver)
		}
		return err
	}
	receiver.Balance += r.HiveAmount.Amount
	objects.AdjustHBDBalance(ctx.DB, &receiver, ctx.HeadBlockTime, r.HbdAmount.Amount)
	objects.PutAccount(ctx.DB, &receiver)

	esc.HiveAmount -= r.HiveAmount.Amount
	esc.HBDAmount -= r.HbdAmount.Amount
	if esc.HiveAmount == 0 && esc.HBDAmount == 0 {
		objects.DeleteEscrowFull(ctx.DB, &esc)
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
