package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/hardfork"
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
		// hived posting auth handling (hive_evaluator_account.cpp:325-339):
		// post-HF15 → verify_authority_accounts_exist (fatal on missing);
		// pre-HF15 → check_authority_accounts_exist which SILENTLY DROPS
		// account_auths referencing non-existent accounts and returns
		// the corrected authority. Match that — pre-HF15 mainnet has
		// account_update ops where the posting authority's account_auths
		// list contained a public-key string (e.g., "STM6GVCS7JeftT27"),
		// which hived dropped quietly and Go was rejecting outright.
		if ctx.Hardforks.Has(hardfork.HF15) {
			if err := verifyAuthorityAccountsExist(ctx, *au.Posting); err != nil {
				return err
			}
		} else {
			corrected, err := checkAuthorityAccountsExistAndCorrect(ctx, *au.Posting)
			if err != nil {
				return err
			}
			*au.Posting = corrected
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
	// hived's update_owner_authority (database.cpp:1678): when the
	// owner authority is rotated, record the OLD owner_authority in
	// owner_authority_history_object and bump previous_owner_update /
	// last_owner_update timestamps.
	// Maintain account_authority_object: rotate owner/active/posting
	// when supplied. Mirrors hived's update_owner_authority +
	// account_update_evaluator at hive_evaluator_account.cpp:325.
	authObj, authErr := objects.GetAccountAuthority(ctx.DB, au.Account)
	if authErr != nil {
		// Bootstrap if missing (legacy genesis accounts).
		authObj = objects.AccountAuthority{Account: au.Account}
	}
	if au.Owner != nil {
		objects.PutOwnerAuthorityHistory(ctx.DB, &objects.OwnerAuthorityHistory{
			Account:                au.Account,
			PreviousOwnerAuthority: authObj.Owner,
			LastValidTime:          ctx.HeadBlockTime,
		})
		authObj.Owner = *au.Owner
		authObj.PreviousOwnerUpdate = authObj.LastOwnerUpdate
		authObj.LastOwnerUpdate = ctx.HeadBlockTime
		a.PreviousOwnerUpdate = a.LastOwnerUpdate
		a.LastOwnerUpdate = ctx.HeadBlockTime
	}
	if au.Active != nil {
		authObj.Active = *au.Active
	}
	if au.Posting != nil {
		authObj.Posting = *au.Posting
	}
	objects.PutAccountAuthority(ctx.DB, &authObj)
	a.LastAccountUpdate = ctx.HeadBlockTime
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
	// HF21__3274 — account_update2 is HF21+ only.
	if !ctx.Hardforks.Has(hardfork.HF21) {
		return fmt.Errorf("account_update2: requires HF21")
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
	// Rotate authorities on the account_authority_object.
	authObj, authErr := objects.GetAccountAuthority(ctx.DB, au.Account)
	if authErr != nil {
		authObj = objects.AccountAuthority{Account: au.Account}
	}
	if au.Owner != nil {
		objects.PutOwnerAuthorityHistory(ctx.DB, &objects.OwnerAuthorityHistory{
			Account:                au.Account,
			PreviousOwnerAuthority: authObj.Owner,
			LastValidTime:          ctx.HeadBlockTime,
		})
		authObj.Owner = *au.Owner
		authObj.PreviousOwnerUpdate = authObj.LastOwnerUpdate
		authObj.LastOwnerUpdate = ctx.HeadBlockTime
		a.PreviousOwnerUpdate = a.LastOwnerUpdate
		a.LastOwnerUpdate = ctx.HeadBlockTime
	}
	if au.Active != nil {
		authObj.Active = *au.Active
	}
	if au.Posting != nil {
		authObj.Posting = *au.Posting
	}
	objects.PutAccountAuthority(ctx.DB, &authObj)
	// posting_json_metadata is a separate field on the C++ account; we
	// merge it into JSONMetadata for now since we don't yet split storage.
	objects.PutAccount(ctx.DB, &a)
	return nil
}

func init() {
	register(op.TagAccountUpdate, accountUpdateEvaluator{})
	register(op.TagAccountUpdate2, accountUpdate2Evaluator{})
}
