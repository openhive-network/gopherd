package evaluator

import (
	"errors"
	"fmt"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// HiveOwnerUpdateLimit mirrors hived's HIVE_OWNER_UPDATE_LIMIT
// (libraries/protocol/include/hive/protocol/config.hpp:117) — 60
// minutes between successive owner-authority rotations.
const HiveOwnerUpdateLimit uint32 = 60 * 60

// OwnerAuthHistoryDefaultStartBlock mirrors hived's start block for
// owner_authority_history tracking. Mainnet replay uses this; tests that
// exercise account_recovery without pre-rolling 3M blocks set
// Context.OwnerAuthHistoryStartBlock to 1 instead.
//
// This was a MUTABLE PACKAGE GLOBAL with a SetOwnerAuthHistoryStartBlock
// setter that tests called concurrently, which the race detector flagged in
// internal/chain/testfixture. It never raced in production (nothing outside
// tests called the setter), but a consensus-gating value has no business
// living in unsynchronised global state: if anything ever set it while blocks
// were applying, the resulting divergence would be silent. It is now a const
// plus a per-Context override, like every other hardfork parameter.
const OwnerAuthHistoryDefaultStartBlock uint32 = 3_186_477

// ErrOwnerUpdateLimit is returned when an account_update / account_update2
// op tries to rotate the owner authority more frequently than
// HIVE_OWNER_UPDATE_LIMIT allows. Pre-HF26 the cap is once per hour; at
// HF26+ the cap widens to twice per hour (the prior-update timestamp is
// taken into account so two updates ~1 hour apart are accepted).
// Matches hive::chain::util::owner_update_limit_mgr::check semantics.
var ErrOwnerUpdateLimit = errors.New("owner authority update rate-limited")

// ownerUpdateLimitCheck returns nil if the proposed owner rotation at
// headBlockTime is allowed given the recorded previous_owner_update and
// last_owner_update timestamps, and the HF26 activation flag.
//
// Mirrors hive::chain::util::owner_update_limit_mgr::check(bool,
// time_point_sec, time_point_sec, time_point_sec) at
// libraries/chain/util/owner_update_limit_mgr.cpp:15-26 — the pre-HF26
// path is single-bound on last_time; HF26+ widens to "succeed if either
// last_time OR previous_time is more than LIMIT seconds ago".
func ownerUpdateLimitCheck(hf26 bool, headBlockTime, previousTime, lastTime uint32) bool {
	pass := func(refTime uint32) bool {
		// fc::time_point_sec subtraction yields a signed seconds delta;
		// guard against the unsigned wrap when refTime > head.
		if headBlockTime <= refTime {
			return false
		}
		return headBlockTime-refTime > HiveOwnerUpdateLimit
	}
	if hf26 {
		if pass(lastTime) {
			return true
		}
		return pass(previousTime)
	}
	return pass(lastTime)
}

// accountUpdateEvaluator implements account_update. The C++ semantics
// also rotate owner/active/posting authorities into the
// account_authority_object — that part is deferred until the auth state
// objects land. For now we apply the always-present memo_key and
// json_metadata fields.
type accountUpdateEvaluator struct{}

func (accountUpdateEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagAccountUpdate {
		return fmt.Errorf("accountUpdateEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	au := opArg.AsAccountUpdate()

	a, err := objects.GetAccount(ctx.DB, au.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, au.Account)
		}
		return err
	}
	// hive_evaluator_account.cpp:299: the temp account cannot be updated
	// (its authority is open, so anyone could otherwise rewrite it).
	if au.Account == hiveTempAccount {
		return fmt.Errorf("account_update: cannot update temp account")
	}
	// HF29 FIX_OPEN_AUTHORITY (hive_evaluator_account.cpp:308-314). Placed
	// ahead of the accounts-exist checks to match hived's ordering, so a
	// rejection reports the same reason theirs does when both would fail.
	if hf29Rule(ctx, hardfork.HF29) {
		if err := validateAccountUpdateAuthorities(au.Account, au.Owner, au.Active, au.Posting); err != nil {
			return err
		}
	}
	// hive_evaluator_account.cpp:305-313 (HF20): validate_auth_size on every
	// authority carried by the op, independent of HF29's open-authority rule.
	if ctx.Hardforks.Has(hardfork.HF20) {
		for _, a := range []*auth.Authority{au.Owner, au.Active, au.Posting} {
			if a == nil {
				continue
			}
			if err := validateAuthSize(*a, string(au.Account)); err != nil {
				return err
			}
		}
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
		// hive_evaluator.cpp:518-522 — once HF11 is active, gate the owner
		// rotation on HIVE_OWNER_UPDATE_LIMIT. Pre-HF26 the check looks at
		// last_owner_update only; HF26+ allows either previous_owner_update
		// or last_owner_update to satisfy the window (the "twice an hour"
		// widening).
		if ctx.Hardforks.Has(hardfork.HF11) {
			if !ownerUpdateLimitCheck(ctx.Hardforks.Has(hardfork.HF26),
				ctx.HeadBlockTime, authObj.PreviousOwnerUpdate, authObj.LastOwnerUpdate) {
				if ctx.Hardforks.Has(hardfork.HF26) {
					return fmt.Errorf("%w: Owner authority can only be updated twice an hour.", ErrOwnerUpdateLimit)
				}
				return fmt.Errorf("%w: Owner authority can only be updated once an hour.", ErrOwnerUpdateLimit)
			}
		}
		// Mirror C++ database.cpp:1670-1673 gate on
		// HIVE_OWNER_AUTH_HISTORY_TRACKING_START_BLOCK_NUM.
		if ctx.BlockNum >= ctx.ownerAuthHistoryStart() {
			objects.PutOwnerAuthorityHistory(ctx.DB, &objects.OwnerAuthorityHistory{
				Account:                au.Account,
				PreviousOwnerAuthority: authObj.Owner,
				LastValidTime:          ctx.HeadBlockTime,
			})
		}
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

func (accountUpdate2Evaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagAccountUpdate2 {
		return fmt.Errorf("accountUpdate2Evaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	au := opArg.AsAccountUpdate2()
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
	// HF29 FIX_OPEN_AUTHORITY (hive_evaluator_account.cpp:308-314). Placed
	// ahead of the accounts-exist checks to match hived's ordering, so a
	// rejection reports the same reason theirs does when both would fail.
	if hf29Rule(ctx, hardfork.HF29) {
		if err := validateAccountUpdateAuthorities(au.Account, au.Owner, au.Active, au.Posting); err != nil {
			return err
		}
	}
	// hive_evaluator_account.cpp:357: the temp account cannot be updated.
	if au.Account == hiveTempAccount {
		return fmt.Errorf("account_update2: cannot update temp account")
	}
	// hive_evaluator_account.cpp:367-369:
	//   if( o.memo_key && *o.memo_key != public_key_type() ) acc.memo_key = *o.memo_key;
	// The null key is a sentinel for "leave it", same as in account_update;
	// writing it zeroed the account's memo key.
	if au.MemoKey != nil && *au.MemoKey != (crypto.PublicKey{}) {
		a.MemoKey = *au.MemoKey
	}
	if au.JSONMetadata != "" {
		a.JSONMetadata = au.JSONMetadata
	}
	// hive_evaluator_account.cpp:368-373: every account named in a rotated
	// authority must exist (account_update has this; update2 lacked it).
	for _, pa := range []*auth.Authority{au.Owner, au.Active, au.Posting} {
		if pa == nil {
			continue
		}
		if err := verifyAuthorityAccountsExist(ctx, *pa); err != nil {
			return err
		}
	}
	// Rotate authorities on the account_authority_object.
	authObj, authErr := objects.GetAccountAuthority(ctx.DB, au.Account)
	if authErr != nil {
		authObj = objects.AccountAuthority{Account: au.Account}
	}
	if au.Owner != nil {
		// hive_evaluator.cpp:585-588 — account_update2 unconditionally
		// gates owner rotation on HIVE_OWNER_UPDATE_LIMIT (no HF11 check
		// because account_update2 itself is HF21+; HF11 is implied).
		// HF26+ widens the cap from once to twice per hour.
		if !ownerUpdateLimitCheck(ctx.Hardforks.Has(hardfork.HF26),
			ctx.HeadBlockTime, authObj.PreviousOwnerUpdate, authObj.LastOwnerUpdate) {
			if ctx.Hardforks.Has(hardfork.HF26) {
				return fmt.Errorf("%w: Owner authority can only be updated twice an hour.", ErrOwnerUpdateLimit)
			}
			return fmt.Errorf("%w: Owner authority can only be updated once an hour.", ErrOwnerUpdateLimit)
		}
		// Mirror C++ database.cpp:1670-1673 gate on
		// HIVE_OWNER_AUTH_HISTORY_TRACKING_START_BLOCK_NUM.
		if ctx.BlockNum >= ctx.ownerAuthHistoryStart() {
			objects.PutOwnerAuthorityHistory(ctx.DB, &objects.OwnerAuthorityHistory{
				Account:                au.Account,
				PreviousOwnerAuthority: authObj.Owner,
				LastValidTime:          ctx.HeadBlockTime,
			})
		}
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

// ownerAuthHistoryStart is the effective start block for
// owner_authority_history tracking: the per-Context override when set, else
// the mainnet default.
func (c *Context) ownerAuthHistoryStart() uint32 {
	if c.OwnerAuthHistoryStartBlock != 0 {
		return c.OwnerAuthHistoryStartBlock
	}
	return OwnerAuthHistoryDefaultStartBlock
}

// validateAccountUpdateAuthorities applies the HF29 open-authority rule to
// whichever of the three roles an account_update[2] actually carries. hived
// spells the three calls out at each site; a helper keeps account_update and
// account_update2 from drifting apart, which is how the two evaluators
// acquired their other differences.
func validateAccountUpdateAuthorities(account auth.AccountName, owner, active, posting *auth.Authority) error {
	if owner != nil {
		if err := validateAccountUpdateAuthority(*owner, string(account), "owner"); err != nil {
			return err
		}
	}
	if active != nil {
		if err := validateAccountUpdateAuthority(*active, string(account), "active"); err != nil {
			return err
		}
	}
	if posting != nil {
		if err := validateAccountUpdateAuthority(*posting, string(account), "posting"); err != nil {
			return err
		}
	}
	return nil
}

// hiveTempAccount is HIVE_TEMP_ACCOUNT ("temp"): an account with an open
// authority that hived refuses to let account_update / account_update2 touch
// (hive_evaluator_account.cpp:299, :357).
const hiveTempAccount = "temp"
