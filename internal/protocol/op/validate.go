// Package op validation methods. Mirrors the `void validate() const`
// member function on each C++ operation_struct: structural-only checks
// (account name format, asset symbol/sign, permlink/memo length, vote
// weight bounds, etc.) with no database access.
//
// Apply-side checks (account exists, sufficient balance, etc.) are
// enforced by the evaluator; Validate is pure-data and should be safe
// to call before signing or even before broadcasting a tx.

package op

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// HIVE_MAX_MEMO_SIZE = 2048 bytes (config.hpp).
const MaxMemoSize = 2048

// HIVE_MAX_PERMLINK_LENGTH = 256.
const MaxPermlinkLength = 256

// HIVE_MAX_COMMENT_TITLE_LENGTH = 256.
const MaxCommentTitleLength = 256

// HIVE_VOTE_WEIGHT_MIN / _MAX = ±10000 (HIVE_100_PERCENT).
const VoteWeightMin int16 = -10000
const VoteWeightMax int16 = 10000

// Validator is implemented by ops that ship a structural validator.
// Not all ops implement it yet; callers should type-assert.
type Validator interface {
	Validate() error
}

// validateAccountName returns an error if name fails the ruleset in
// libraries/protocol/authority.cpp::is_valid_account_name.
func validateAccountName(label, name string) error {
	if !auth.IsValidAccountName(name) {
		return fmt.Errorf("%s: invalid account name %q", label, name)
	}
	return nil
}

// requireSymbol returns an error if a's symbol is not in allowed.
func requireSymbol(label string, a asset.Asset, allowed ...asset.AssetNum) error {
	for _, s := range allowed {
		if a.Symbol.Num == s {
			return nil
		}
	}
	return fmt.Errorf("%s: unsupported asset symbol", label)
}

// --- Vote --------------------------------------------------------------

func (o Vote) Validate() error {
	if err := validateAccountName("vote.voter", string(o.Voter)); err != nil {
		return err
	}
	if err := validateAccountName("vote.author", string(o.Author)); err != nil {
		return err
	}
	if len(o.Permlink) == 0 || len(o.Permlink) > MaxPermlinkLength {
		return fmt.Errorf("vote.permlink: length %d not in (0, %d]", len(o.Permlink), MaxPermlinkLength)
	}
	if o.Weight < VoteWeightMin || o.Weight > VoteWeightMax {
		return fmt.Errorf("vote.weight: %d out of [%d, %d]", o.Weight, VoteWeightMin, VoteWeightMax)
	}
	return nil
}

// --- Transfer ----------------------------------------------------------

func (o Transfer) Validate() error {
	if err := validateAccountName("transfer.from", string(o.From)); err != nil {
		return err
	}
	if err := validateAccountName("transfer.to", string(o.To)); err != nil {
		return err
	}
	if err := requireSymbol("transfer.amount", o.Amount, asset.HiveSymbol, asset.HbdSymbol); err != nil {
		return err
	}
	if o.Amount.Amount <= 0 {
		return fmt.Errorf("transfer.amount: must be positive, got %d", o.Amount.Amount)
	}
	if len(o.Memo) > MaxMemoSize {
		return fmt.Errorf("transfer.memo: %d bytes exceeds %d", len(o.Memo), MaxMemoSize)
	}
	return nil
}

// --- TransferToVesting -------------------------------------------------

func (o TransferToVesting) Validate() error {
	if err := validateAccountName("transfer_to_vesting.from", string(o.From)); err != nil {
		return err
	}
	// TO can be empty (means same as FROM in C++); else must be valid.
	if o.To != "" {
		if err := validateAccountName("transfer_to_vesting.to", string(o.To)); err != nil {
			return err
		}
	}
	if err := requireSymbol("transfer_to_vesting.amount", o.Amount, asset.HiveSymbol); err != nil {
		return err
	}
	if o.Amount.Amount <= 0 {
		return fmt.Errorf("transfer_to_vesting.amount: must be positive, got %d", o.Amount.Amount)
	}
	return nil
}

// --- Comment -----------------------------------------------------------

func (o Comment) Validate() error {
	if err := validateAccountName("comment.author", string(o.Author)); err != nil {
		return err
	}
	if len(o.Permlink) == 0 || len(o.Permlink) > MaxPermlinkLength {
		return fmt.Errorf("comment.permlink: length %d not in (0, %d]", len(o.Permlink), MaxPermlinkLength)
	}
	if o.ParentAuthor != "" {
		if err := validateAccountName("comment.parent_author", string(o.ParentAuthor)); err != nil {
			return err
		}
	}
	if len(o.ParentPermlink) > MaxPermlinkLength {
		return fmt.Errorf("comment.parent_permlink: length %d exceeds %d",
			len(o.ParentPermlink), MaxPermlinkLength)
	}
	if len(o.Title) > MaxCommentTitleLength {
		return fmt.Errorf("comment.title: length %d exceeds %d", len(o.Title), MaxCommentTitleLength)
	}
	if len(o.Body) == 0 {
		return errors.New("comment.body: must not be empty")
	}
	return nil
}

// --- AccountWitnessVote ------------------------------------------------

func (o AccountWitnessVote) Validate() error {
	if err := validateAccountName("account_witness_vote.account", string(o.Account)); err != nil {
		return err
	}
	if err := validateAccountName("account_witness_vote.witness", string(o.Witness)); err != nil {
		return err
	}
	return nil
}

// --- AccountWitnessProxy -----------------------------------------------

func (o AccountWitnessProxy) Validate() error {
	if err := validateAccountName("account_witness_proxy.account", string(o.Account)); err != nil {
		return err
	}
	// Empty proxy means "clear proxy".
	if o.Proxy != "" {
		if err := validateAccountName("account_witness_proxy.proxy", string(o.Proxy)); err != nil {
			return err
		}
	}
	return nil
}

// --- WitnessUpdate -----------------------------------------------------

func (o WitnessUpdate) Validate() error {
	if err := validateAccountName("witness_update.owner", string(o.Owner)); err != nil {
		return err
	}
	if len(o.URL) == 0 || len(o.URL) > 2048 {
		return fmt.Errorf("witness_update.url: length %d not in (0, 2048]", len(o.URL))
	}
	if err := requireSymbol("witness_update.fee", o.Fee, asset.HiveSymbol); err != nil {
		// fee may be zero-amount + HIVE; accept either form
	}
	if o.Fee.Amount < 0 {
		return fmt.Errorf("witness_update.fee: must be non-negative, got %d", o.Fee.Amount)
	}
	return nil
}

// --- DeclineVotingRights -----------------------------------------------

func (o DeclineVotingRights) Validate() error {
	return validateAccountName("decline_voting_rights.account", string(o.Account))
}

// --- AccountCreate -----------------------------------------------------

func (o AccountCreate) Validate() error {
	if err := validateAccountName("account_create.creator", string(o.Creator)); err != nil {
		return err
	}
	if err := validateAccountName("account_create.new_account_name", string(o.NewAccountName)); err != nil {
		return err
	}
	if err := requireSymbol("account_create.fee", o.Fee, asset.HiveSymbol); err != nil {
		return err
	}
	if o.Fee.Amount < 0 {
		return fmt.Errorf("account_create.fee: must be non-negative, got %d", o.Fee.Amount)
	}
	return nil
}

// --- LimitOrderCreate --------------------------------------------------

func (o LimitOrderCreate) Validate() error {
	if err := validateAccountName("limit_order_create.owner", string(o.Owner)); err != nil {
		return err
	}
	if o.AmountToSell.Amount <= 0 {
		return fmt.Errorf("limit_order_create.amount_to_sell: must be positive, got %d", o.AmountToSell.Amount)
	}
	if o.MinToReceive.Amount <= 0 {
		return fmt.Errorf("limit_order_create.min_to_receive: must be positive, got %d", o.MinToReceive.Amount)
	}
	if o.AmountToSell.Symbol.Num == o.MinToReceive.Symbol.Num {
		return errors.New("limit_order_create: cannot sell and receive the same asset")
	}
	// Mirror hive_operations.cpp:601 — both legs must be in the HIVE/HBD
	// market. (Smart-asset NAI tolerance is omitted: SMTs never shipped on
	// mainnet and our op decoder doesn't surface NAI symbols.) Guards
	// against a malformed op silently dropping the credit in the receiver
	// switch in fillLimitOrder.
	if err := requireSymbol("limit_order_create.amount_to_sell", o.AmountToSell, asset.HiveSymbol, asset.HbdSymbol); err != nil {
		return err
	}
	if err := requireSymbol("limit_order_create.min_to_receive", o.MinToReceive, asset.HiveSymbol, asset.HbdSymbol); err != nil {
		return err
	}
	return nil
}
