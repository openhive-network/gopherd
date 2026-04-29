package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// ErrAccountAlreadyExists is returned by account_create when the chosen
// new_account_name is already taken.
var ErrAccountAlreadyExists = errors.New("account already exists")

// ErrInvalidAccountFee is returned when the creator pays in a non-HIVE
// asset, since account creation fees are denominated in HIVE.
var ErrInvalidAccountFee = errors.New("account creation fee must be HIVE")

type accountCreateEvaluator struct{}

func (accountCreateEvaluator) Apply(ctx *Context, o op.Operation) error {
	ac, ok := o.(op.AccountCreate)
	if !ok {
		return fmt.Errorf("accountCreateEvaluator: unexpected op type %T", o)
	}

	if ac.Fee.Symbol.Num != asset.HiveSymbol {
		return ErrInvalidAccountFee
	}
	if ac.Fee.Amount < 0 {
		return ErrTransferAmountNotPos
	}

	// Creator must exist with enough HIVE to cover the fee.
	creator, err := objects.GetAccount(ctx.DB, ac.Creator)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: creator=%q", ErrAccountNotFound, ac.Creator)
		}
		return err
	}
	if creator.Balance < ac.Fee.Amount {
		return fmt.Errorf("%w: %q has %d HIVE, fee is %d", ErrInsufficientBalance, ac.Creator, creator.Balance, ac.Fee.Amount)
	}

	// New account name must NOT already exist.
	if has, err := objects.HasAccount(ctx.DB, ac.NewAccountName); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q", ErrAccountAlreadyExists, ac.NewAccountName)
	}

	// Debit creator.
	creator.Balance -= ac.Fee.Amount
	objects.PutAccount(ctx.DB, &creator)

	// Create the new account first so CreateVesting can credit it.
	// hived's account_create_evaluator (libraries/chain/hive_evaluator.cpp)
	// runs `db.create_vesting(new_account, o.fee)` immediately after,
	// converting the creator's fee into starter VESTS for the new
	// account at the current vesting price.
	newAcct := objects.Account{
		Name:         ac.NewAccountName,
		Created:      ctx.BlockTime,
		MemoKey:      ac.MemoKey,
		JSONMetadata: ac.JSONMetadata,
	}
	objects.PutAccount(ctx.DB, &newAcct)
	if ac.Fee.Amount > 0 {
		if _, err := CreateVesting(ctx.DB, ac.NewAccountName, ac.Fee.Amount, false); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	register(op.TagAccountCreate, accountCreateEvaluator{})
}
