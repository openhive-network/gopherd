package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// verifyAuthorityAccountsExist mirrors hived's verify_authority_accounts_exist
// (libraries/chain/hive_evaluator_account.cpp:80). Each name appearing in the
// authority's account_auths list must already exist in the account table.
// is_required=true (the post-HF15 mode we use here) makes any missing account
// a hard error.
func verifyAuthorityAccountsExist(ctx *Context, a auth.Authority) error {
	for _, aa := range a.AccountAuths {
		if has, err := objects.HasAccount(ctx.DB, aa.Account); err != nil {
			return err
		} else if !has {
			return fmt.Errorf("%w: authority references missing %q", ErrAccountNotFound, aa.Account)
		}
	}
	return nil
}

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

	// hived: verify_authority_accounts_exist for each of owner/active/posting.
	// Without this we silently accept account_creates that hived rejects;
	// the chain then has different account counts than hived.
	if err := verifyAuthorityAccountsExist(ctx, ac.Owner); err != nil {
		return err
	}
	if err := verifyAuthorityAccountsExist(ctx, ac.Active); err != nil {
		return err
	}
	if err := verifyAuthorityAccountsExist(ctx, ac.Posting); err != nil {
		return err
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
		Name:                 ac.NewAccountName,
		Created:              ctx.HeadBlockTime,
		MemoKey:              ac.MemoKey,
		JSONMetadata:         ac.JSONMetadata,
		VotingManaCurrent:    10000, // full mana at creation
		VotingManaLastUpdate: ctx.HeadBlockTime,
	}
	// HF20+: prime the RC manabar at creation so subsequent regen
	// has a defined origin. max_rc starts at 0 (no VESTS yet).
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.InitRCManabar(&newAcct, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &newAcct)
	// Persist the per-account authority record. Mirrors hived's
	// create<account_authority_object> in account_create_evaluator.
	objects.PutAccountAuthority(ctx.DB, &objects.AccountAuthority{
		Account:             ac.NewAccountName,
		Owner:               ac.Owner,
		Active:              ac.Active,
		Posting:             ac.Posting,
		LastOwnerUpdate:     ctx.HeadBlockTime,
		PreviousOwnerUpdate: ctx.HeadBlockTime,
	})
	if ac.Fee.Amount > 0 {
		if _, err := CreateVesting(ctx.DB, ac.NewAccountName, ac.Fee.Amount, false); err != nil {
			return err
		}
		// After CreateVesting credits VESTS to the new account, refresh
		// its RC cap.
		if ctx.Hardforks.Has(hardfork.HF20) {
			if a, err := objects.GetAccount(ctx.DB, ac.NewAccountName); err == nil {
				objects.RegenerateRCMana(&a, ctx.HeadBlockTime)
				objects.PutAccount(ctx.DB, &a)
			}
		}
	}
	return nil
}

func init() {
	register(op.TagAccountCreate, accountCreateEvaluator{})
}
