package evaluator

import (
	"bytes"
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

// authoritiesEqual reports whether two authority structs are equal in
// the FC sense — same weight_threshold and same (sorted) sequence of
// account_auths and key_auths. We compare by their canonical FC binary
// encoding so nil vs empty-slice fields round-trip identically (which
// `reflect.DeepEqual` would not).
func authoritiesEqual(a, b auth.Authority) bool {
	if a.WeightThreshold != b.WeightThreshold {
		return false
	}
	ab := auth.AppendAuthority(nil, a)
	bb := auth.AppendAuthority(nil, b)
	if len(ab) != len(bb) {
		return false
	}
	for i := range ab {
		if ab[i] != bb[i] {
			return false
		}
	}
	return true
}

// findRecentOwnerAuthority scans owner_authority_history rows for
// `account` and returns true iff any row's PreviousOwnerAuthority
// equals `recent` AND that row's LastValidTime is within
// HIVE_OWNER_AUTH_RECOVERY_PERIOD of headBlockTime. Mirrors the
// matching loop in C++ recover_account_evaluator (hive_evaluator.cpp:2609)
// combined with the periodic cleanup pass (database.cpp:3702-3710) that
// would otherwise have already removed stale rows.
func findRecentOwnerAuthority(s *state.Session, account auth.AccountName, recent auth.Authority, headBlockTime uint32) bool {
	prefix := state.AppendAccountName(nil, account)
	found := false
	_ = s.Range(objects.OwnerAuthorityHistoryTable, state.PrimaryIndex, prefix,
		func(_, value []byte) (bool, error) {
			row, err := objects.ReadOwnerAuthorityHistory(value)
			if err != nil {
				return true, nil
			}
			if row.Account != account {
				return false, nil
			}
			// Skip rows older than HIVE_OWNER_AUTH_RECOVERY_PERIOD —
			// they would have been deleted by C++'s periodic cleanup.
			if row.LastValidTime+changeRecoveryEffectiveDelaySec < headBlockTime {
				return true, nil
			}
			if authoritiesEqual(row.PreviousOwnerAuthority, recent) {
				found = true
				return false, nil
			}
			return true, nil
		})
	return found
}

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
	// Mirror C++ hive_evaluator_account.cpp:541 — the proposed
	// new_owner_authority on the op MUST match the one recorded with
	// the recovery request. Without this check, anyone holding a
	// valid recent_owner_authority could recover the account to ANY
	// new key, bypassing the request's stated intent. Gopherd
	// previously stored NewOwnerAuthorityRaw on the request but
	// never read it back.
	opNewOwnerRaw := auth.AppendAuthority(nil, r.NewOwnerAuthority)
	if !bytes.Equal(opNewOwnerRaw, req.NewOwnerAuthorityRaw) {
		return fmt.Errorf("recover_account: new_owner_authority does not match the recovery request")
	}
	// Match recent_owner_authority against the recorded
	// owner_authority_history rows for this account, skipping any row
	// whose last_valid_time is older than HIVE_OWNER_AUTH_RECOVERY_PERIOD
	// (30 days). In C++ those stale rows are removed by a periodic
	// cleanup pass in account_recovery_processing
	// (database.cpp:3702-3710); the matching loop in
	// recover_account_evaluator (hive_evaluator.cpp:2609-2620) then
	// only sees fresh rows. We don't run a separate cleanup, so we
	// apply the same age filter inline here — semantically equivalent.
	if !findRecentOwnerAuthority(ctx.DB, r.AccountToRecover, r.RecentOwnerAuthority, ctx.HeadBlockTime) {
		return fmt.Errorf("recover_account: recent authority not found in owner_authority_history (or expired)")
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
