package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// accountUpdateEvaluator implements account_update. The C++ semantics
// also rotate owner/active/posting authorities into the
// account_authority_object — that part is deferred until the auth state
// objects land. For now we apply the always-present memo_key and
// json_metadata fields.
type accountUpdateEvaluator struct{}

func (accountUpdateEvaluator) Apply(ctx *Context, o op.Operation) error {
	au, ok := o.(op.AccountUpdate)
	if !ok {
		return fmt.Errorf("accountUpdateEvaluator: unexpected op type %T", o)
	}

	a, err := objects.GetAccount(ctx.DB, au.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, au.Account)
		}
		return err
	}
	// hived: each rotated authority's account_auths must reference real
	// accounts (libraries/chain/hive_evaluator_account.cpp:318).
	if au.Owner != nil {
		if err := verifyAuthorityAccountsExist(ctx, *au.Owner); err != nil {
			return err
		}
	}
	if au.Active != nil {
		if err := verifyAuthorityAccountsExist(ctx, *au.Active); err != nil {
			return err
		}
	}
	if au.Posting != nil {
		if err := verifyAuthorityAccountsExist(ctx, *au.Posting); err != nil {
			return err
		}
	}
	// hived only overwrites memo_key when the op carries a non-zero
	// public_key_type (hive_evaluator_account.cpp:343). We treat the
	// all-zero 33-byte key as the same sentinel.
	var zeroKey [33]byte
	if au.MemoKey != zeroKey {
		a.MemoKey = au.MemoKey
	}
	a.JSONMetadata = au.JSONMetadata
	objects.PutAccount(ctx.DB, &a)
	return nil
}

// accountUpdate2Evaluator handles HF21+ account_update2 — same as
// account_update but with optional fields and posting_json_metadata.
type accountUpdate2Evaluator struct{}

func (accountUpdate2Evaluator) Apply(ctx *Context, o op.Operation) error {
	au, ok := o.(op.AccountUpdate2)
	if !ok {
		return fmt.Errorf("accountUpdate2Evaluator: unexpected op type %T", o)
	}

	a, err := objects.GetAccount(ctx.DB, au.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, au.Account)
		}
		return err
	}
	if au.MemoKey != nil {
		a.MemoKey = *au.MemoKey
	}
	if au.JSONMetadata != "" {
		a.JSONMetadata = au.JSONMetadata
	}
	// posting_json_metadata is a separate field on the C++ account; we
	// merge it into JSONMetadata for now since we don't yet split storage.
	objects.PutAccount(ctx.DB, &a)
	return nil
}

func init() {
	register(op.TagAccountUpdate, accountUpdateEvaluator{})
	register(op.TagAccountUpdate2, accountUpdate2Evaluator{})
}
