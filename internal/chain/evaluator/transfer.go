package evaluator

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// convertHiveToHBDByMedian mirrors C++ asset_symbol::operator*(price)
// for HIVE×median → HBD. base = HBD, quote = HIVE (post-HF20 normalized).
// Pre-HF20 the orientation can be reversed; we handle both. Native
// uint128 math replaces a math/big triple-alloc per call.
func convertHiveToHBDByMedian(hiveAmount int64, p asset.Price) int64 {
	if p.Base.Amount <= 0 || p.Quote.Amount <= 0 {
		return 0
	}
	var num, den int64
	switch {
	case p.Base.Symbol.Num == asset.HbdSymbol && p.Quote.Symbol.Num == asset.HiveSymbol:
		num, den = p.Base.Amount, p.Quote.Amount
	case p.Base.Symbol.Num == asset.HiveSymbol && p.Quote.Symbol.Num == asset.HbdSymbol:
		num, den = p.Quote.Amount, p.Base.Amount
	default:
		return 0
	}
	hi, lo := bits.Mul64(uint64(hiveAmount), uint64(num))
	d := uint64(den)
	if hi >= d {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, d)
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// isTreasuryAccount reports whether name is the canonical DHF treasury
// account at the given hardfork set. Mirrors hived's
// database::is_treasury (database.cpp).
//   - pre-HF24: "steem.dao"
//   - HF24+:    "hive.fund" (the rename was atomic at HF24)
//
// Hived also clears the OLD treasury post-HF24 — we keep both names
// matching as treasury throughout for safety on the boundary block.
func isTreasuryAccount(name string, hf hardfork.Set) bool {
	if hf.Has(hardfork.HF24) {
		return name == "hive.fund"
	}
	return name == "steem.dao"
}

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

	// HF1_24 treasury HIVE→HBD conversion (hive_evaluator_transfer.cpp:263).
	// Sending HIVE to treasury post-HF24 implicitly converts to HBD at the
	// current median price, supply moves accordingly.
	if ctx.Hardforks.Has(hardfork.HF24) && tx.Amount.Symbol.Num == asset.HiveSymbol && isTreasuryAccount(tx.To, ctx.Hardforks) {
		if from.Balance < tx.Amount.Amount {
			return fmt.Errorf("%w: %q has %d HIVE, needs %d", ErrInsufficientBalance, tx.From, from.Balance, tx.Amount.Amount)
		}
		fh, ferr := objects.GetFeedHistory(ctx.DB)
		if ferr != nil || fh.CurrentMedianHistory.Base.Amount == 0 {
			return fmt.Errorf("transfer: send HIVE to treasury requires a price feed")
		}
		convertedAmt := convertHiveToHBDByMedian(tx.Amount.Amount, fh.CurrentMedianHistory)
		from.Balance -= tx.Amount.Amount
		objects.PutAccount(ctx.DB, &from)

		to, terr := objects.GetAccount(ctx.DB, tx.To)
		if terr != nil {
			return fmt.Errorf("%w: to=%q (treasury)", ErrAccountNotFound, tx.To)
		}
		objects.AdjustHBDBalance(ctx.DB, &to, ctx.HeadBlockTime, convertedAmt)
		objects.PutAccount(ctx.DB, &to)

		// adjust_supply(-HIVE) and adjust_supply(+HBD) — supply totals
		// in DGPO.
		dgpo, derr := objects.GetDGPO(ctx.DB)
		if derr == nil {
			dgpo.CurrentSupply -= tx.Amount.Amount
			if convertedAmt > 0 {
				dgpo.CurrentHBDSupply += convertedAmt
			}
			objects.PutDGPO(ctx.DB, &dgpo)
		}
		return nil
	}

	// HF0_21__3343: post-HF21, treasury accounts can only receive HBD
	// (the HF24 HIVE→HBD path above handles HF24+). Hive sending HIVE
	// to treasury pre-HF24 is rejected.
	if ctx.Hardforks.Has(hardfork.HF21) && isTreasuryAccount(tx.To, ctx.Hardforks) {
		if tx.Amount.Symbol.Num != asset.HbdSymbol {
			return fmt.Errorf("transfer: can only send HBD or HIVE-via-HF24 to treasury %q", tx.To)
		}
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
		debit = func(a *objects.Account) { objects.AdjustHBDBalance(ctx.DB, a, ctx.HeadBlockTime, -tx.Amount.Amount) }
		credit = func(a *objects.Account) { objects.AdjustHBDBalance(ctx.DB, a, ctx.HeadBlockTime, tx.Amount.Amount) }
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
