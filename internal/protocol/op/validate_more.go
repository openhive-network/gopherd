package op

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
)

// Validate methods ported from libraries/protocol/hive_operations.cpp.
// Each mirrors the corresponding C++ validate() body.

// HIVE_100_PERCENT (config.hpp).
const hivePercent uint16 = 10000

// HIVE_MIN_RECURRENT_TRANSFERS_RECURRENCE = 24 hours (config.hpp).
const minRecurrentTransfersRecurrenceHours uint16 = 24

// HIVE_MAX_ACCOUNT_CREATION_FEE in HIVE-satoshi (config.hpp:189).
// = 1_000_000_000 (1,000,000.000 HIVE).
const maxAccountCreationFeeSatoshi int64 = 1_000_000_000

// ----- helpers -----

func isHive(a asset.Asset) bool   { return a.Symbol.Num == asset.HiveSymbol }
func isHbd(a asset.Asset) bool    { return a.Symbol.Num == asset.HbdSymbol }
func isVests(a asset.Asset) bool  { return a.Symbol.Num == asset.VestsSymbol }

// ---- WithdrawVesting --------------------------------------------------

func (o WithdrawVesting) Validate() error {
	if err := validateAccountName("withdraw_vesting.account", string(o.Account)); err != nil {
		return err
	}
	if !isVests(o.VestingShares) {
		return errors.New("withdraw_vesting.vesting_shares: amount must be VESTS")
	}
	return nil
}

// ---- AccountUpdate ----------------------------------------------------

func (o AccountUpdate) Validate() error {
	return validateAccountName("account_update.account", string(o.Account))
}

func (o AccountUpdate2) Validate() error {
	return validateAccountName("account_update2.account", string(o.Account))
}

// ---- Convert / CollateralizedConvert ---------------------------------

func (o Convert) Validate() error {
	if err := validateAccountName("convert.owner", string(o.Owner)); err != nil {
		return err
	}
	if !isHbd(o.Amount) {
		return errors.New("convert.amount: only HBD→HIVE supported")
	}
	if o.Amount.Amount <= 0 {
		return errors.New("convert.amount: must be positive")
	}
	return nil
}

func (o CollateralizedConvert) Validate() error {
	if err := validateAccountName("collateralized_convert.owner", string(o.Owner)); err != nil {
		return err
	}
	if !isHive(o.Amount) {
		return errors.New("collateralized_convert.amount: only HIVE→HBD supported")
	}
	if o.Amount.Amount <= 0 {
		return errors.New("collateralized_convert.amount: must be positive")
	}
	return nil
}

// ---- FeedPublish ------------------------------------------------------

func (o FeedPublish) Validate() error {
	if err := validateAccountName("feed_publish.publisher", string(o.Publisher)); err != nil {
		return err
	}
	bIsHive, bIsHbd := isHive(o.ExchangeRate.Base), isHbd(o.ExchangeRate.Base)
	qIsHive, qIsHbd := isHive(o.ExchangeRate.Quote), isHbd(o.ExchangeRate.Quote)
	if !((bIsHive && qIsHbd) || (bIsHbd && qIsHive)) {
		return errors.New("feed_publish.exchange_rate: must be a HIVE/HBD price")
	}
	if o.ExchangeRate.Base.Amount <= 0 || o.ExchangeRate.Quote.Amount <= 0 {
		return errors.New("feed_publish.exchange_rate: base and quote must be positive")
	}
	return nil
}

// ---- LimitOrderCancel / LimitOrderCreate2 ----------------------------

func (o LimitOrderCancel) Validate() error {
	return validateAccountName("limit_order_cancel.owner", string(o.Owner))
}

func (o LimitOrderCreate2) Validate() error {
	if err := validateAccountName("limit_order_create2.owner", string(o.Owner)); err != nil {
		return err
	}
	if o.AmountToSell.Symbol.Num != o.ExchangeRate.Base.Symbol.Num {
		return errors.New("limit_order_create2.amount_to_sell: must match exchange_rate.base symbol")
	}
	if o.ExchangeRate.Base.Amount <= 0 || o.ExchangeRate.Quote.Amount <= 0 {
		return errors.New("limit_order_create2.exchange_rate: base/quote must be positive")
	}
	return nil
}

// ---- Escrow family ----------------------------------------------------

func (o EscrowTransfer) Validate() error {
	if err := validateAccountName("escrow_transfer.from", string(o.From)); err != nil {
		return err
	}
	if err := validateAccountName("escrow_transfer.to", string(o.To)); err != nil {
		return err
	}
	if err := validateAccountName("escrow_transfer.agent", string(o.Agent)); err != nil {
		return err
	}
	if o.Fee.Amount < 0 {
		return errors.New("escrow_transfer.fee: cannot be negative")
	}
	if o.HbdAmount.Amount < 0 || o.HiveAmount.Amount < 0 {
		return errors.New("escrow_transfer: amounts cannot be negative")
	}
	if o.HbdAmount.Amount == 0 && o.HiveAmount.Amount == 0 {
		return errors.New("escrow_transfer: must transfer a non-zero amount")
	}
	if o.From == o.Agent || o.To == o.Agent {
		return errors.New("escrow_transfer.agent: must be a third party")
	}
	if !(isHive(o.Fee) || isHbd(o.Fee)) {
		return errors.New("escrow_transfer.fee: must be HIVE or HBD")
	}
	if !isHbd(o.HbdAmount) {
		return errors.New("escrow_transfer.hbd_amount: must be HBD")
	}
	if !isHive(o.HiveAmount) {
		return errors.New("escrow_transfer.hive_amount: must be HIVE")
	}
	if o.RatificationDeadline >= o.EscrowExpiration {
		return errors.New("escrow_transfer: ratification deadline must precede escrow expiration")
	}
	return nil
}

func (o EscrowApprove) Validate() error {
	for _, a := range []struct {
		label, name string
	}{
		{"escrow_approve.from", string(o.From)},
		{"escrow_approve.to", string(o.To)},
		{"escrow_approve.agent", string(o.Agent)},
		{"escrow_approve.who", string(o.Who)},
	} {
		if err := validateAccountName(a.label, a.name); err != nil {
			return err
		}
	}
	if o.Who != o.To && o.Who != o.Agent {
		return errors.New("escrow_approve.who: must be `to` or `agent`")
	}
	return nil
}

func (o EscrowDispute) Validate() error {
	for _, a := range []struct {
		label, name string
	}{
		{"escrow_dispute.from", string(o.From)},
		{"escrow_dispute.to", string(o.To)},
		{"escrow_dispute.agent", string(o.Agent)},
		{"escrow_dispute.who", string(o.Who)},
	} {
		if err := validateAccountName(a.label, a.name); err != nil {
			return err
		}
	}
	if o.Who != o.From && o.Who != o.To {
		return errors.New("escrow_dispute.who: must be `from` or `to`")
	}
	return nil
}

func (o EscrowRelease) Validate() error {
	for _, a := range []struct {
		label, name string
	}{
		{"escrow_release.from", string(o.From)},
		{"escrow_release.to", string(o.To)},
		{"escrow_release.agent", string(o.Agent)},
		{"escrow_release.who", string(o.Who)},
		{"escrow_release.receiver", string(o.Receiver)},
	} {
		if err := validateAccountName(a.label, a.name); err != nil {
			return err
		}
	}
	if o.Who != o.From && o.Who != o.To && o.Who != o.Agent {
		return errors.New("escrow_release.who: must be `from`, `to`, or `agent`")
	}
	if o.Receiver != o.From && o.Receiver != o.To {
		return errors.New("escrow_release.receiver: must be `from` or `to`")
	}
	if o.HbdAmount.Amount < 0 || o.HiveAmount.Amount < 0 {
		return errors.New("escrow_release: amounts cannot be negative")
	}
	if o.HbdAmount.Amount == 0 && o.HiveAmount.Amount == 0 {
		return errors.New("escrow_release: must release a non-zero amount")
	}
	if !isHbd(o.HbdAmount) {
		return errors.New("escrow_release.hbd_amount: must be HBD")
	}
	if !isHive(o.HiveAmount) {
		return errors.New("escrow_release.hive_amount: must be HIVE")
	}
	return nil
}

// ---- Savings family ---------------------------------------------------

func (o TransferToSavings) Validate() error {
	if err := validateAccountName("transfer_to_savings.from", string(o.From)); err != nil {
		return err
	}
	if err := validateAccountName("transfer_to_savings.to", string(o.To)); err != nil {
		return err
	}
	if o.Amount.Amount <= 0 {
		return errors.New("transfer_to_savings.amount: must be positive")
	}
	if !(isHive(o.Amount) || isHbd(o.Amount)) {
		return errors.New("transfer_to_savings.amount: must be HIVE or HBD")
	}
	if len(o.Memo) >= MaxMemoSize {
		return fmt.Errorf("transfer_to_savings.memo: %d bytes exceeds %d", len(o.Memo), MaxMemoSize-1)
	}
	return nil
}

func (o TransferFromSavings) Validate() error {
	if err := validateAccountName("transfer_from_savings.from", string(o.From)); err != nil {
		return err
	}
	if err := validateAccountName("transfer_from_savings.to", string(o.To)); err != nil {
		return err
	}
	if o.Amount.Amount <= 0 {
		return errors.New("transfer_from_savings.amount: must be positive")
	}
	if !(isHive(o.Amount) || isHbd(o.Amount)) {
		return errors.New("transfer_from_savings.amount: must be HIVE or HBD")
	}
	if len(o.Memo) >= MaxMemoSize {
		return fmt.Errorf("transfer_from_savings.memo: %d bytes exceeds %d", len(o.Memo), MaxMemoSize-1)
	}
	return nil
}

func (o CancelTransferFromSavings) Validate() error {
	return validateAccountName("cancel_transfer_from_savings.from", string(o.From))
}

// ---- ClaimAccount / CreateClaimedAccount / AccountCreateWithDelegation

func (o ClaimAccount) Validate() error {
	if err := validateAccountName("claim_account.creator", string(o.Creator)); err != nil {
		return err
	}
	if !isHive(o.Fee) {
		return errors.New("claim_account.fee: must be HIVE")
	}
	if o.Fee.Amount < 0 {
		return errors.New("claim_account.fee: cannot be negative")
	}
	if o.Fee.Amount > maxAccountCreationFeeSatoshi {
		return fmt.Errorf("claim_account.fee: %d exceeds max %d", o.Fee.Amount, maxAccountCreationFeeSatoshi)
	}
	return nil
}

func (o CreateClaimedAccount) Validate() error {
	if err := validateAccountName("create_claimed_account.creator", string(o.Creator)); err != nil {
		return err
	}
	if err := validateAccountName("create_claimed_account.new_account_name", string(o.NewAccountName)); err != nil {
		return err
	}
	return nil
}

func (o AccountCreateWithDelegation) Validate() error {
	if err := validateAccountName("account_create_with_delegation.creator", string(o.Creator)); err != nil {
		return err
	}
	if err := validateAccountName("account_create_with_delegation.new_account_name", string(o.NewAccountName)); err != nil {
		return err
	}
	if !isHive(o.Fee) {
		return errors.New("account_create_with_delegation.fee: must be HIVE")
	}
	if !isVests(o.Delegation) {
		return errors.New("account_create_with_delegation.delegation: must be VESTS")
	}
	return nil
}

// ---- ClaimRewardBalance / DelegateVestingShares ---------------------

func (o ClaimRewardBalance) Validate() error {
	if err := validateAccountName("claim_reward_balance.account", string(o.Account)); err != nil {
		return err
	}
	if !isHive(o.RewardHive) {
		return errors.New("claim_reward_balance.reward_hive: must be HIVE")
	}
	if !isHbd(o.RewardHbd) {
		return errors.New("claim_reward_balance.reward_hbd: must be HBD")
	}
	if !isVests(o.RewardVests) {
		return errors.New("claim_reward_balance.reward_vests: must be VESTS")
	}
	if o.RewardHive.Amount < 0 || o.RewardHbd.Amount < 0 || o.RewardVests.Amount < 0 {
		return errors.New("claim_reward_balance: cannot claim negative amount")
	}
	if o.RewardHive.Amount == 0 && o.RewardHbd.Amount == 0 && o.RewardVests.Amount == 0 {
		return errors.New("claim_reward_balance: must claim something")
	}
	return nil
}

func (o DelegateVestingShares) Validate() error {
	if err := validateAccountName("delegate_vesting_shares.delegator", string(o.Delegator)); err != nil {
		return err
	}
	if err := validateAccountName("delegate_vesting_shares.delegatee", string(o.Delegatee)); err != nil {
		return err
	}
	if o.Delegator == o.Delegatee {
		return errors.New("delegate_vesting_shares: cannot delegate to self")
	}
	if !isVests(o.VestingShares) {
		return errors.New("delegate_vesting_shares.vesting_shares: must be VESTS")
	}
	if o.VestingShares.Amount < 0 {
		return errors.New("delegate_vesting_shares.vesting_shares: cannot be negative")
	}
	return nil
}

// ---- WitnessSetProperties --------------------------------------------

func (o WitnessSetProperties) Validate() error {
	if err := validateAccountName("witness_set_properties.owner", string(o.Owner)); err != nil {
		return err
	}
	hasKey := false
	for _, p := range o.Props {
		if p.Key == "key" {
			hasKey = true
			break
		}
	}
	if !hasKey {
		return errors.New("witness_set_properties.props: 'key' (signing key) is required")
	}
	return nil
}

// ---- CommentOptions / SetWithdrawVestingRoute ------------------------

func (o CommentOptions) Validate() error {
	if err := validateAccountName("comment_options.author", string(o.Author)); err != nil {
		return err
	}
	if o.PercentHbd > hivePercent {
		return fmt.Errorf("comment_options.percent_hbd: %d > 100%%", o.PercentHbd)
	}
	if !isHbd(o.MaxAcceptedPayout) {
		return errors.New("comment_options.max_accepted_payout: must be HBD")
	}
	if o.MaxAcceptedPayout.Amount < 0 {
		return errors.New("comment_options.max_accepted_payout: cannot be negative")
	}
	if len(o.Permlink) == 0 || len(o.Permlink) > MaxPermlinkLength {
		return fmt.Errorf("comment_options.permlink: length %d not in (0, %d]", len(o.Permlink), MaxPermlinkLength)
	}
	return nil
}

func (o SetWithdrawVestingRoute) Validate() error {
	if err := validateAccountName("set_withdraw_vesting_route.from_account", string(o.FromAccount)); err != nil {
		return err
	}
	if err := validateAccountName("set_withdraw_vesting_route.to_account", string(o.ToAccount)); err != nil {
		return err
	}
	if o.Percent > hivePercent {
		return fmt.Errorf("set_withdraw_vesting_route.percent: %d > 100%%", o.Percent)
	}
	return nil
}

// ---- RecurrentTransfer ------------------------------------------------

func (o RecurrentTransfer) Validate() error {
	if err := validateAccountName("recurrent_transfer.from", string(o.From)); err != nil {
		return err
	}
	if err := validateAccountName("recurrent_transfer.to", string(o.To)); err != nil {
		return err
	}
	if isVests(o.Amount) {
		return errors.New("recurrent_transfer.amount: cannot be VESTS")
	}
	if o.Amount.Amount < 0 {
		return errors.New("recurrent_transfer.amount: cannot be negative")
	}
	if o.Recurrence < minRecurrentTransfersRecurrenceHours {
		return fmt.Errorf("recurrent_transfer.recurrence: %d < min %d hours",
			o.Recurrence, minRecurrentTransfersRecurrenceHours)
	}
	if len(o.Memo) >= MaxMemoSize {
		return fmt.Errorf("recurrent_transfer.memo: %d bytes exceeds %d", len(o.Memo), MaxMemoSize-1)
	}
	if o.From == o.To {
		return errors.New("recurrent_transfer: cannot transfer to self")
	}
	if o.Executions < 2 {
		return errors.New("recurrent_transfer.executions: must be at least 2")
	}
	return nil
}

// ---- DHF (proposals) --------------------------------------------------

// HIVE_PROPOSAL_SUBJECT_MAX_LENGTH = 80 (config.hpp).
const proposalSubjectMaxLength = 80

// HIVE_PROPOSAL_MAX_IDS_NUMBER = 5 (config.hpp).
const proposalMaxIDsNumber = 5

func (o CreateProposal) Validate() error {
	if err := validateAccountName("create_proposal.creator", string(o.Creator)); err != nil {
		return err
	}
	if err := validateAccountName("create_proposal.receiver", string(o.Receiver)); err != nil {
		return err
	}
	if o.EndDate <= o.StartDate {
		return errors.New("create_proposal: end_date must be > start_date")
	}
	if !isHbd(o.DailyPay) {
		return errors.New("create_proposal.daily_pay: must be HBD")
	}
	if o.DailyPay.Amount < 0 {
		return errors.New("create_proposal.daily_pay: cannot be negative")
	}
	if o.Subject == "" {
		return errors.New("create_proposal.subject: must not be empty")
	}
	if len(o.Subject) > proposalSubjectMaxLength {
		return fmt.Errorf("create_proposal.subject: %d chars exceeds %d",
			len(o.Subject), proposalSubjectMaxLength)
	}
	if o.Permlink == "" {
		return errors.New("create_proposal.permlink: must not be empty")
	}
	if len(o.Permlink) > MaxPermlinkLength {
		return fmt.Errorf("create_proposal.permlink: %d chars exceeds %d",
			len(o.Permlink), MaxPermlinkLength)
	}
	return nil
}

func (o UpdateProposalVotes) Validate() error {
	if err := validateAccountName("update_proposal_votes.voter", string(o.Voter)); err != nil {
		return err
	}
	if len(o.ProposalIDs) == 0 {
		return errors.New("update_proposal_votes: proposal_ids must not be empty")
	}
	if len(o.ProposalIDs) > proposalMaxIDsNumber {
		return fmt.Errorf("update_proposal_votes: %d ids exceeds %d",
			len(o.ProposalIDs), proposalMaxIDsNumber)
	}
	return nil
}

func (o RemoveProposal) Validate() error {
	if err := validateAccountName("remove_proposal.proposal_owner", string(o.ProposalOwner)); err != nil {
		return err
	}
	if len(o.ProposalIDs) == 0 {
		return errors.New("remove_proposal: proposal_ids must not be empty")
	}
	if len(o.ProposalIDs) > proposalMaxIDsNumber {
		return fmt.Errorf("remove_proposal: %d ids exceeds %d",
			len(o.ProposalIDs), proposalMaxIDsNumber)
	}
	return nil
}

// ---- Pow / Pow2 -------------------------------------------------------

func (o Pow) Validate() error {
	return validateAccountName("pow.worker_account", string(o.WorkerAccount))
}

func (o Pow2) Validate() error {
	worker := o.PowInput.WorkerAccount
	if o.WorkVariantTag == 1 {
		worker = o.EquihashWork.Input.WorkerAccount
	}
	return validateAccountName("pow2.worker_account", worker)
}
