package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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

func (claimAccountEvaluator) Apply(ctx *Context, o op.Operation) error {
	ca, ok := o.(op.ClaimAccount)
	if !ok {
		return fmt.Errorf("claimAccountEvaluator: unexpected op type %T", o)
	}
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
	// HF20+: Fee=0 ⇒ subsidy. The current block's producer pays
	// FIRST from THEIR per-witness pool (if creator IS that witness),
	// else from the global pool. Both pools must have ≥
	// HIVE_ACCOUNT_SUBSIDY_PRECISION (10000) units.
	if ca.Fee.Amount == 0 {
		dgpo, derr := objects.GetDGPO(ctx.DB)
		if derr == nil {
			// Try per-witness pool if creator is the current producer.
			if string(ca.Creator) == dgpo.CurrentWitness {
				if w, werr := objects.GetWitness(ctx.DB, ca.Creator); werr == nil &&
					w.AvailableWitnessAccountSubsidies >= 10000 {
					w.AvailableWitnessAccountSubsidies -= 10000
					objects.PutWitness(ctx.DB, &w)
					creator.PendingClaimedAccounts++
					objects.PutAccount(ctx.DB, &creator)
					return nil
				}
			}
			// Fallback: global pool.
			if dgpo.AvailableAccountSubsidies >= 10000 {
				dgpo.AvailableAccountSubsidies -= 10000
				objects.PutDGPO(ctx.DB, &dgpo)
				creator.PendingClaimedAccounts++
				objects.PutAccount(ctx.DB, &creator)
				return nil
			}
		}
	}
	if creator.Balance < ca.Fee.Amount {
		return fmt.Errorf("%w: %q has %d HIVE, fee %d", ErrInsufficientBalance, ca.Creator, creator.Balance, ca.Fee.Amount)
	}
	creator.Balance -= ca.Fee.Amount
	creator.PendingClaimedAccounts++
	objects.PutAccount(ctx.DB, &creator)
	return nil
}

// createClaimedAccountEvaluator implements create_claimed_account.
// Decrements the creator's pending_claimed_accounts and creates the
// new account with no fee.
type createClaimedAccountEvaluator struct{}

func (createClaimedAccountEvaluator) Apply(ctx *Context, o op.Operation) error {
	cc, ok := o.(op.CreateClaimedAccount)
	if !ok {
		return fmt.Errorf("createClaimedAccountEvaluator: unexpected op type %T", o)
	}
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
	newAcct := objects.Account{
		Name:                 cc.NewAccountName,
		Created:              ctx.HeadBlockTime,
		MemoKey:              cc.MemoKey,
		JSONMetadata:         cc.JSONMetadata,
		VotingManaCurrent:    10000,
		VotingManaLastUpdate: ctx.HeadBlockTime,
	}
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.InitRCManabar(&newAcct, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &newAcct)
	objects.PutAccountAuthority(ctx.DB, &objects.AccountAuthority{
		Account:             cc.NewAccountName,
		Owner:               cc.Owner,
		Active:              cc.Active,
		Posting:             cc.Posting,
		LastOwnerUpdate:     ctx.HeadBlockTime,
		PreviousOwnerUpdate: ctx.HeadBlockTime,
	})
	return nil
}

// accountCreateWithDelegationEvaluator implements
// account_create_with_delegation (HF17+). Creates the new account
// and records the VESTS delegation from creator to newcomer.
type accountCreateWithDelegationEvaluator struct{}

func (accountCreateWithDelegationEvaluator) Apply(ctx *Context, o op.Operation) error {
	ac, ok := o.(op.AccountCreateWithDelegation)
	if !ok {
		return fmt.Errorf("accountCreateWithDelegationEvaluator: unexpected op type %T", o)
	}
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
		return fmt.Errorf("%w: creator has %d undelegated VESTS, delegation %d", ErrInsufficientBalance, availVests, ac.Delegation.Amount)
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
	}
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.InitRCManabar(&newAcct, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &newAcct)
	objects.PutAccountAuthority(ctx.DB, &objects.AccountAuthority{
		Account:             ac.NewAccountName,
		Owner:               ac.Owner,
		Active:              ac.Active,
		Posting:             ac.Posting,
		LastOwnerUpdate:     ctx.HeadBlockTime,
		PreviousOwnerUpdate: ctx.HeadBlockTime,
	})
	// Convert the fee to starter VESTS for the new account, matching
	// hived's account_create_with_delegation_evaluator (libraries/chain/
	// hive_evaluator.cpp) which calls db.create_vesting2 on the fee.
	if ac.Fee.Amount > 0 {
		if _, err := CreateVesting(ctx.DB, ac.NewAccountName, ac.Fee.Amount, false); err != nil {
			return err
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
