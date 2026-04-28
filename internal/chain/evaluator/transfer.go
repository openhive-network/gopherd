package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Errors evaluators commonly return. Caller code can use errors.Is to
// detect the consensus reason for rejection.
var (
	ErrAccountNotFound      = errors.New("account not found")
	ErrInsufficientBalance  = errors.New("insufficient balance")
	ErrUnsupportedSymbol    = errors.New("unsupported asset symbol")
	ErrTransferAmountNotPos = errors.New("transfer amount must be positive")
)

// transferEvaluator implements the apply logic for op.Transfer.
//
// Mirrors hive::chain::transfer_evaluator::do_apply (libraries/chain/
// hive_evaluator.cpp), simplified to the balance-mutation core. We don't
// yet check post-HF condition flags or the savings-account variants —
// those land with their respective evaluators.
type transferEvaluator struct{}

func (transferEvaluator) Apply(ctx *Context, o op.Operation) error {
	tx, ok := o.(op.Transfer)
	if !ok {
		return fmt.Errorf("transferEvaluator: unexpected op type %T", o)
	}

	// Basic validation. Negative or zero transfers are rejected by the C++
	// validator; replicate.
	if tx.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}

	// Look up sender. The C++ code throws if the account doesn't exist.
	from, err := objects.GetAccount(ctx.DB, tx.From)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: from=%q", ErrAccountNotFound, tx.From)
		}
		return err
	}

	// Determine which balance field the transfer touches based on the
	// asset symbol. Only HIVE and HBD are valid for liquid transfers.
	var (
		debit  func(*objects.Account)
		credit func(*objects.Account)
	)
	switch tx.Amount.Symbol.Num {
	case asset.HiveSymbol:
		if from.Balance < tx.Amount.Amount {
			return fmt.Errorf("%w: %q has %d HIVE, needs %d", ErrInsufficientBalance, tx.From, from.Balance, tx.Amount.Amount)
		}
		debit = func(a *objects.Account) { a.Balance -= tx.Amount.Amount }
		credit = func(a *objects.Account) { a.Balance += tx.Amount.Amount }
	case asset.HbdSymbol:
		if from.HBDBalance < tx.Amount.Amount {
			return fmt.Errorf("%w: %q has %d HBD, needs %d", ErrInsufficientBalance, tx.From, from.HBDBalance, tx.Amount.Amount)
		}
		debit = func(a *objects.Account) { a.HBDBalance -= tx.Amount.Amount }
		credit = func(a *objects.Account) { a.HBDBalance += tx.Amount.Amount }
	default:
		return fmt.Errorf("%w: %s (transfer accepts HIVE or HBD only)", ErrUnsupportedSymbol, tx.Amount.Symbol.String())
	}

	debit(&from)
	objects.PutAccount(ctx.DB, &from)

	// Re-fetch destination to handle the self-transfer case correctly:
	// when From == To, the GetAccount call below sees the just-debited
	// value through the overlay, and the subsequent credit puts it back
	// to its pre-transfer balance — same observable effect as the C++
	// adjust_balance(from, -amount); adjust_balance(to, amount); pair.
	to, err := objects.GetAccount(ctx.DB, tx.To)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: to=%q", ErrAccountNotFound, tx.To)
		}
		return err
	}
	credit(&to)
	objects.PutAccount(ctx.DB, &to)
	return nil
}

func init() {
	register(op.TagTransfer, transferEvaluator{})
}
