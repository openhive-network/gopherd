package evaluator

import (
	"errors"
	"fmt"
	"os"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Errors specific to claim_account / create_claimed_account.
var (
	ErrNoPendingClaim = errors.New("creator has no pending claimed accounts")
)

// claimAccountEvaluator implements claim_account (HF20+). The creator
// pays a HIVE fee (or 0 with subsidy) to credit one
// pending_claimed_account that can later be redeemed via
// create_claimed_account for fee-free account creation.
//
// Hived also tracks account-subsidy resource pools and pending claim
// fees in DGPO; we don't model those yet, so the fee is accepted as
// long as it covers a non-negative HIVE amount.
type claimAccountEvaluator struct{}

func (claimAccountEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagClaimAccount {
		return fmt.Errorf("claimAccountEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	ca := opArg.AsClaimAccount()
	// HF20__1771 — claim_account is HF20+ only.
	if !ctx.Hardforks.Has(hardfork.HF20) {
		return fmt.Errorf("claim_account: requires HF20")
	}
	if ca.Fee.Symbol.Num != asset.HiveSymbol {
		return ErrInvalidAccountFee
	}
	if ca.Fee.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	creator, err := objects.GetAccount(ctx.DB, ca.Creator)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: creator=%q", ErrAccountNotFound, ca.Creator)
		}
		return err
	}
	if creator.Balance < ca.Fee.Amount {
		return fmt.Errorf("%w: %q has %d HIVE, fee %d", ErrInsufficientBalance, ca.Creator, creator.Balance, ca.Fee.Amount)
	}
	// hive_evaluator_account.cpp:407-445. Fee=0 ⇒ subsidized claim:
	// the current block's PRODUCER (not the creator) must be an elected
	// witness with ≥ HIVE_ACCOUNT_SUBSIDY_PRECISION (10000) in its
	// per-witness pool, AND the global pool must also cover 10000.
	// BOTH are decremented; a shortfall in either REJECTS the op (there
	// is no free fall-through). Fee>0 ⇒ must equal the median exactly.
	if ca.Fee.Amount == 0 {
		dgpo, derr := objects.GetDGPO(ctx.DB)
		if derr != nil {
			return derr
		}
		w, werr := objects.GetWitness(ctx.DB, dgpo.CurrentWitness)
		if werr != nil {
			return fmt.Errorf("claim_account: current witness %q not found", dgpo.CurrentWitness)
		}
		if w.Schedule != objects.WitnessScheduleElected {
			return fmt.Errorf("claim_account: subsidized accounts can only be claimed by elected witnesses (current %q)", dgpo.CurrentWitness)
		}
		if w.AvailableWitnessAccountSubsidies < 10000 {
			return fmt.Errorf("claim_account: witness %q does not have enough subsidized accounts to claim (%d)", dgpo.CurrentWitness, w.AvailableWitnessAccountSubsidies)
		}
		if dgpo.AvailableAccountSubsidies < 10000 {
			return fmt.Errorf("claim_account: not enough subsidized accounts to claim (%d)", dgpo.AvailableAccountSubsidies)
		}
		w.AvailableWitnessAccountSubsidies -= 10000
		objects.PutWitness(ctx.DB, &w)
		dgpo.AvailableAccountSubsidies -= 10000
		objects.PutDGPO(ctx.DB, &dgpo)
	} else if ws, wserr := objects.GetWitnessSchedule(ctx.DB); wserr == nil {
		if ca.Fee.Amount != ws.MedianAccountCreationFee {
			return fmt.Errorf("claim_account: must pay the exact creation fee or zero (paid %d, fee %d)", ca.Fee.Amount, ws.MedianAccountCreationFee)
		}
	}
	creator.Balance -= ca.Fee.Amount
	creator.PendingClaimedAccounts++
	objects.PutAccount(ctx.DB, &creator)
	// Mirror C++ hive_evaluator_account.cpp:440-444: the fee is credited
	// to NULL_ACCOUNT, then clearNullAccountBalance burns it from
	// current_supply next block. Without this credit, sum(balances)
	// drops while current_supply stays put — drift bucket: small HIVE
	// positive in dgpo, accumulating one fee per claim_account post-HF20.
	if ca.Fee.Amount > 0 {
		if nullAcct, nerr := objects.GetAccount(ctx.DB, "null"); nerr == nil {
			nullAcct.Balance += ca.Fee.Amount
			objects.PutAccount(ctx.DB, &nullAcct)
		}
	}
	return nil
}

// createClaimedAccountEvaluator implements create_claimed_account.
// Decrements the creator's pending_claimed_accounts and creates the
// new account with no fee.
type createClaimedAccountEvaluator struct{}

func (createClaimedAccountEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagCreateClaimedAccount {
		return fmt.Errorf("createClaimedAccountEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	cc := opArg.AsCreateClaimedAccount()
	// HF20__1771 — create_claimed_account is HF20+ only.
	if !ctx.Hardforks.Has(hardfork.HF20) {
		return fmt.Errorf("create_claimed_account: requires HF20")
	}
	creator, err := objects.GetAccount(ctx.DB, cc.Creator)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: creator=%q", ErrAccountNotFound, cc.Creator)
		}
		return err
	}
	// hived requires the creator to have a non-zero
	// pending_claimed_accounts counter (the slot was paid for via a
	// prior claim_account).
	if creator.PendingClaimedAccounts == 0 {
		return fmt.Errorf("%w: %q has no pending claimed accounts", ErrInsufficientBalance, cc.Creator)
	}
	creator.PendingClaimedAccounts--
	objects.PutAccount(ctx.DB, &creator)
	if has, err := objects.HasAccount(ctx.DB, cc.NewAccountName); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q", ErrAccountAlreadyExists, cc.NewAccountName)
	}
	// HF20+ VotingManaCurrent is VESTS (no vesting at creation = 0);
	// pre-HF20 it is percent (10000 = full).
	initialManaCC := int64(10000)
	if ctx.Hardforks.Has(hardfork.HF20) {
		initialManaCC = 0
	}
	newAcct := objects.Account{
		Name:                 cc.NewAccountName,
		Created:              ctx.HeadBlockTime,
		MemoKey:              cc.MemoKey,
		JSONMetadata:         cc.JSONMetadata,
		VotingManaCurrent:    initialManaCC,
		VotingManaLastUpdate: ctx.HeadBlockTime,
		// Stamp the downvote bar's last_update at creation too (C++
		// account_object.hpp:50-51); see account_create.go for the
		// over-regen bug this avoids.
		DownvoteManaLastUpdate: ctx.HeadBlockTime,
		RecoveryAccount:        defaultRecoveryAccount(ctx, cc.Creator),
	}
	// hived passes the SCHEDULE MEDIAN account_creation_fee here, not an op
	// fee — a claimed account is paid for with a token, but its RC adjustment
	// is still derived from what the fee would have been
	// (hive_evaluator_account.cpp:504).
	if ctx.Hardforks.Has(hardfork.HF20) {
		adj := int64(0)
		if dgpo, derr := objects.GetDGPO(ctx.DB); derr == nil {
			if ws, werr := objects.GetWitnessSchedule(ctx.DB); werr == nil {
				adj = rcAdjustmentFromFee(&dgpo, ws.MedianAccountCreationFee)
			}
		}
		objects.InitRCManabarWithAdjustment(&newAcct, adj, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &newAcct)
	objects.PutAccountAuthority(ctx.DB, &objects.AccountAuthority{
		Account: cc.NewAccountName,
		Owner:   cc.Owner,
		Active:  cc.Active,
		Posting: cc.Posting,
		// hive_evaluator.cpp:459-460 — both init to time_point_sec::min().
		LastOwnerUpdate:     0,
		PreviousOwnerUpdate: 0,
	})
	// A claimed account pays no fee, so both starting amounts are zero by
	// construction (hive_evaluator_account.cpp:509).
	ctx.EmitAccountCreated(string(cc.NewAccountName), string(cc.Creator), 0, 0)
	return nil
}

// accountCreateWithDelegationEvaluator implements
// account_create_with_delegation (HF17+). Creates the new account
// and records the VESTS delegation from creator to newcomer.
type accountCreateWithDelegationEvaluator struct{}

func (accountCreateWithDelegationEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagAccountCreateWithDelegation {
		return fmt.Errorf("accountCreateWithDelegationEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	ac := opArg.AsAccountCreateWithDelegation()
	// HF20__1760 deprecates account_create_with_delegation; the op must
	// be rejected post-HF20 (hive_evaluator_account.cpp:212).
	if ctx.Hardforks.Has(hardfork.HF20) {
		return fmt.Errorf("account_create_with_delegation: deprecated post-HF20")
	}
	if ac.Fee.Symbol.Num != asset.HiveSymbol {
		return ErrInvalidAccountFee
	}
	if ac.Fee.Amount < 0 || ac.Delegation.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	if ac.Delegation.Symbol.Num != asset.VestsSymbol {
		return fmt.Errorf("%w: delegation must be VESTS", ErrUnsupportedSymbol)
	}
	creator, err := objects.GetAccount(ctx.DB, ac.Creator)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: creator=%q", ErrAccountNotFound, ac.Creator)
		}
		return err
	}
	if creator.Balance < ac.Fee.Amount {
		return fmt.Errorf("%w: %q has %d HIVE, fee %d", ErrInsufficientBalance, ac.Creator, creator.Balance, ac.Fee.Amount)
	}
	availVests := creator.VestingShares - creator.DelegatedVestingShares
	if availVests < ac.Delegation.Amount {
		return fmt.Errorf("%w: %q has %d undelegated VESTS, delegation %d", ErrInsufficientBalance, ac.Creator, availVests, ac.Delegation.Amount)
	}
	if has, err := objects.HasAccount(ctx.DB, ac.NewAccountName); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q", ErrAccountAlreadyExists, ac.NewAccountName)
	}
	if err := verifyAuthorityAccountsExist(ctx, ac.Owner); err != nil {
		return err
	}
	if err := verifyAuthorityAccountsExist(ctx, ac.Active); err != nil {
		return err
	}
	if err := verifyAuthorityAccountsExist(ctx, ac.Posting); err != nil {
		return err
	}

	creator.Balance -= ac.Fee.Amount
	creator.DelegatedVestingShares += ac.Delegation.Amount
	objects.PutAccount(ctx.DB, &creator)

	newAcct := objects.Account{
		Name:                  ac.NewAccountName,
		Created:               ctx.HeadBlockTime,
		MemoKey:               ac.MemoKey,
		JSONMetadata:          ac.JSONMetadata,
		ReceivedVestingShares: ac.Delegation.Amount,
		VotingManaCurrent:     10000,
		VotingManaLastUpdate:  ctx.HeadBlockTime,
		// Stamp the downvote bar's last_update at creation too (C++
		// account_object.hpp:50-51). This account_create_with_delegation
		// path is exactly how nobilpcoinbpc was created; without the stamp
		// its downvote bar over-regenerated → the kimzwarch claim_reward
		// cascade. See account_create.go for the full rationale.
		DownvoteManaLastUpdate: ctx.HeadBlockTime,
		RecoveryAccount:        defaultRecoveryAccount(ctx, ac.Creator),
	}
	// hived passes the OP's fee here (hive_evaluator_account.cpp:260).
	if ctx.Hardforks.Has(hardfork.HF20) {
		adj := int64(0)
		if dgpo, derr := objects.GetDGPO(ctx.DB); derr == nil {
			adj = rcAdjustmentFromFee(&dgpo, ac.Fee.Amount)
		}
		objects.InitRCManabarWithAdjustment(&newAcct, adj, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &newAcct)
	objects.PutAccountAuthority(ctx.DB, &objects.AccountAuthority{
		Account: ac.NewAccountName,
		Owner:   ac.Owner,
		Active:  ac.Active,
		Posting: ac.Posting,
		// hive_evaluator.cpp init: time_point_sec::min().
		LastOwnerUpdate:     0,
		PreviousOwnerUpdate: 0,
	})
	// Convert the fee to starter VESTS for the new account, matching
	// hived's account_create_with_delegation_evaluator (libraries/chain/
	// hive_evaluator.cpp) which calls db.create_vesting2 on the fee.
	initialVestingShares := int64(0)
	if ac.Fee.Amount > 0 {
		var verr error
		initialVestingShares, verr = CreateVesting(ctx.DB, ac.NewAccountName, ac.Fee.Amount, false)
		if verr != nil {
			return verr
		}
	}
	// hive_evaluator_account.cpp:268 — record the vesting_delegation
	// with a min_delegation_time of head + 30 days. Pre-HF19__997
	// always creates the row; post-HF19 only when delegation > 0.
	// Reducing this delegation later goes through a longer return
	// period than ordinary delegate_vesting_shares — the rebate fires
	// at max(now + delegation_return_period, min_delegation_time).
	if ac.Delegation.Amount > 0 || !ctx.Hardforks.Has(hardfork.HF19) {
		objects.PutVestingDelegation(ctx.DB, &objects.VestingDelegation{
			Delegator:     ac.Creator,
			Delegatee:     ac.NewAccountName,
			VestingShares: ac.Delegation.Amount,
			MinDelegation: ctx.HeadBlockTime + createAccountDelegationTimeSec,
		})
		if vestTraceAccount(string(ac.Creator)) {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF DELEG-ACWD block=%d t=%d creator=%s new=%s amount=%d minDeleg=%d\n",
				ctx.BlockNum, ctx.HeadBlockTime, ac.Creator, ac.NewAccountName, ac.Delegation.Amount, ctx.HeadBlockTime+createAccountDelegationTimeSec)
		}
	}
	// Refresh the new account's RC cap now that VESTS (delegated +
	// from fee) are in place.
	if ctx.Hardforks.Has(hardfork.HF20) {
		if a, err := objects.GetAccount(ctx.DB, ac.NewAccountName); err == nil {
			objects.RegenerateRCMana(&a, ctx.HeadBlockTime)
			objects.PutAccount(ctx.DB, &a)
		}
		// Creator's effective_vests went down → refresh creator too.
		if creator, err := objects.GetAccount(ctx.DB, ac.Creator); err == nil {
			objects.RegenerateRCMana(&creator, ctx.HeadBlockTime)
			objects.PutAccount(ctx.DB, &creator)
		}
	}
	ctx.EmitAccountCreated(string(ac.NewAccountName), string(ac.Creator), initialVestingShares, ac.Delegation.Amount)
	return nil
}

// HIVE_CREATE_ACCOUNT_DELEGATION_TIME = 30 days. Mirror of
// libraries/protocol/include/hive/protocol/config.hpp:280.
const createAccountDelegationTimeSec uint32 = 30 * 24 * 60 * 60

func init() {
	register(op.TagClaimAccount, claimAccountEvaluator{})
	register(op.TagCreateClaimedAccount, createClaimedAccountEvaluator{})
	register(op.TagAccountCreateWithDelegation, accountCreateWithDelegationEvaluator{})
}
