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

// defaultRecoveryAccount returns the value the new account's
// recovery_account field should take at creation. Mirror C++
// hive_evaluator_account.cpp:134-142:
//
//   - HIVE_TEMP_ACCOUNT creator → empty (no recovery agent)
//   - pre-HF11             → "steem"
//   - post-HF11            → creator
//
// post-HF11 is the dominant on-chain case; the "steem" pre-HF11
// default is the original legacy behavior.
func defaultRecoveryAccount(ctx *Context, creator auth.AccountName) auth.AccountName {
	if creator == "temp" {
		return ""
	}
	if ctx.Hardforks.Has(hardfork.HF11) {
		return creator
	}
	return "steem"
}

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

// checkAuthorityAccountsExistAndCorrect mirrors hived's
// check_authority_accounts_exist (hive_evaluator_account.cpp:89). Pre-HF15
// posting authorities silently dropped any account_auth referencing a
// non-existent account instead of throwing. Returns the cleaned-up
// authority (may be the same as input when nothing was dropped).
func checkAuthorityAccountsExistAndCorrect(ctx *Context, a auth.Authority) (auth.Authority, error) {
	cleaned := a
	hadInvalid := false
	keep := cleaned.AccountAuths[:0]
	for _, aa := range cleaned.AccountAuths {
		has, err := objects.HasAccount(ctx.DB, aa.Account)
		if err != nil {
			return cleaned, err
		}
		if has {
			keep = append(keep, aa)
		} else {
			hadInvalid = true
		}
	}
	if hadInvalid {
		// Don't mutate the input slice header; return a copy with the
		// filtered list so callers can swap in the corrected authority.
		corrected := auth.Authority{
			WeightThreshold: cleaned.WeightThreshold,
			AccountAuths:    append([]auth.AccountAuth(nil), keep...),
			KeyAuths:        cleaned.KeyAuths,
		}
		return corrected, nil
	}
	return cleaned, nil
}

// ErrAccountAlreadyExists is returned by account_create when the chosen
// new_account_name is already taken.
var ErrAccountAlreadyExists = errors.New("account already exists")

// ErrInvalidAccountFee is returned when the creator pays in a non-HIVE
// asset, since account creation fees are denominated in HIVE.
var ErrInvalidAccountFee = errors.New("account creation fee must be HIVE")

type accountCreateEvaluator struct{}

func (accountCreateEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagAccountCreate {
		return fmt.Errorf("accountCreateEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	ac := opArg.AsAccountCreate()

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
		// Mirror C++ hive_evaluator_account.cpp:134-142: recovery_account
		// defaults to "steem" pre-HF11, creator account post-HF11, or
		// empty when creator is HIVE_TEMP_ACCOUNT. The post-HF11 case
		// is the dominant on-chain path.
		RecoveryAccount: defaultRecoveryAccount(ctx, ac.Creator),
	}
	// HF20+: prime the RC manabar at creation so subsequent regen
	// has a defined origin. max_rc starts at 0 (no VESTS yet).
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.InitRCManabar(&newAcct, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &newAcct)
	// Persist the per-account authority record. Mirrors hived's
	// create<account_authority_object> in account_create_evaluator. The
	// hived initializer (hive_evaluator.cpp:354-355) sets both timestamps
	// to fc::time_point_sec::min() = 0, NOT the creation time — this
	// matters once HIVE_OWNER_UPDATE_LIMIT is enforced (HF11+), as the
	// gate compares `head_block_time - last_owner_update > LIMIT`. With
	// the C++-correct zero init, the first owner rotation after
	// account_create is always allowed.
	objects.PutAccountAuthority(ctx.DB, &objects.AccountAuthority{
		Account:             ac.NewAccountName,
		Owner:               ac.Owner,
		Active:              ac.Active,
		Posting:             ac.Posting,
		LastOwnerUpdate:     0,
		PreviousOwnerUpdate: 0,
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
