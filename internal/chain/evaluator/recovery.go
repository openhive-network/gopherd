package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_ACCOUNT_RECOVERY_REQUEST_EXPIRATION_PERIOD = 24 hours.
const recoveryRequestExpirationSec uint32 = 60 * 60 * 24

// HIVE_OWNER_AUTH_RECOVERY_PERIOD = 30 days.
const changeRecoveryEffectiveDelaySec uint32 = 60 * 60 * 24 * 30

type requestAccountRecoveryEvaluator struct{}

func (requestAccountRecoveryEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagRequestAccountRecovery {
		return fmt.Errorf("requestAccountRecoveryEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	rr := opArg.AsRequestAccountRecovery()
	if has, err := objects.HasAccount(ctx.DB, rr.AccountToRecover); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: %q", ErrAccountNotFound, rr.AccountToRecover)
	}
	objects.PutAccountRecoveryRequest(ctx.DB, &objects.AccountRecoveryRequest{
		AccountToRecover:     rr.AccountToRecover,
		NewOwnerAuthorityRaw: auth.AppendAuthority(nil, rr.NewOwnerAuthority),
		Expiration:           ctx.HeadBlockTime + recoveryRequestExpirationSec,
	})
	return nil
}

type recoverAccountEvaluator struct{}

func (recoverAccountEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagRecoverAccount {
		return fmt.Errorf("recoverAccountEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	r := opArg.AsRecoverAccount()
	req, err := objects.GetAccountRecoveryRequest(ctx.DB, r.AccountToRecover)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: no pending recovery request for %q", ErrAccountNotFound, r.AccountToRecover)
		}
		return err
	}
	if ctx.HeadBlockTime > req.Expiration {
		objects.DeleteAccountRecoveryRequestFull(ctx.DB, &req)
		return fmt.Errorf("recover_account: request expired")
	}
	// Rotate owner authority on the account_authority_object. Mirrors
	// hived's recover_account_evaluator (hive_evaluator_account.cpp:402).
	authObj, authErr := objects.GetAccountAuthority(ctx.DB, r.AccountToRecover)
	if authErr != nil {
		// Bootstrap if missing.
		authObj = objects.AccountAuthority{Account: r.AccountToRecover}
	}
	objects.PutOwnerAuthorityHistory(ctx.DB, &objects.OwnerAuthorityHistory{
		Account:                r.AccountToRecover,
		PreviousOwnerAuthority: authObj.Owner,
		LastValidTime:          ctx.HeadBlockTime,
	})
	authObj.Owner = r.NewOwnerAuthority
	authObj.PreviousOwnerUpdate = authObj.LastOwnerUpdate
	authObj.LastOwnerUpdate = ctx.HeadBlockTime
	objects.PutAccountAuthority(ctx.DB, &authObj)
	if a, aErr := objects.GetAccount(ctx.DB, r.AccountToRecover); aErr == nil {
		a.PreviousOwnerUpdate = a.LastOwnerUpdate
		a.LastOwnerUpdate = ctx.HeadBlockTime
		a.BlockLastAccountRecovery = ctx.BlockNum
		objects.PutAccount(ctx.DB, &a)
	}
	objects.DeleteAccountRecoveryRequestFull(ctx.DB, &req)
	return nil
}

type changeRecoveryAccountEvaluator struct{}

func (changeRecoveryAccountEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagChangeRecoveryAccount {
		return fmt.Errorf("changeRecoveryAccountEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	cr := opArg.AsChangeRecoveryAccount()
	if has, err := objects.HasAccount(ctx.DB, cr.AccountToRecover); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: %q", ErrAccountNotFound, cr.AccountToRecover)
	}
	objects.PutChangeRecoveryRequest(ctx.DB, &objects.ChangeRecoveryAccountRequest{
		AccountToRecover: cr.AccountToRecover,
		RecoveryAccount:  cr.NewRecoveryAccount,
		EffectiveOn:      ctx.HeadBlockTime + changeRecoveryEffectiveDelaySec,
	})
	return nil
}

func init() {
	register(op.TagRequestAccountRecovery, requestAccountRecoveryEvaluator{})
	register(op.TagRecoverAccount, recoverAccountEvaluator{})
	register(op.TagChangeRecoveryAccount, changeRecoveryAccountEvaluator{})
}
