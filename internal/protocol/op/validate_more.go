package op

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"hived-go/internal/fcraw"
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

func isHive(a asset.Asset) bool  { return a.Symbol.Num == asset.HiveSymbol }
func isHbd(a asset.Asset) bool   { return a.Symbol.Num == asset.HbdSymbol }
func isVests(a asset.Asset) bool { return a.Symbol.Num == asset.VestsSymbol }

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
	if o.AmountToSell.Amount <= 0 {
		return fmt.Errorf("limit_order_create2.amount_to_sell: must be positive, got %d", o.AmountToSell.Amount)
	}
	if o.ExchangeRate.Base.Amount <= 0 || o.ExchangeRate.Quote.Amount <= 0 {
		return errors.New("limit_order_create2.exchange_rate: base/quote must be positive")
	}
	if o.AmountToSell.Symbol.Num == o.ExchangeRate.Quote.Symbol.Num {
		return errors.New("limit_order_create2: cannot sell and receive the same asset")
	}
	// Mirror hive_operations.cpp:620 — limit orders are restricted to the
	// HIVE/HBD market on mainnet.
	if err := requireSymbol("limit_order_create2.amount_to_sell", o.AmountToSell, asset.HiveSymbol, asset.HbdSymbol); err != nil {
		return err
	}
	if err := requireSymbol("limit_order_create2.exchange_rate.quote",
		asset.Asset{Amount: o.ExchangeRate.Quote.Amount, Symbol: o.ExchangeRate.Quote.Symbol},
		asset.HiveSymbol, asset.HbdSymbol); err != nil {
		return err
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

// Constants mirrored from libraries/protocol/include/hive/protocol/config.hpp.
//
//	HIVE_MAX_TRANSACTION_SIZE   = 1024*64                                = 65536
//	HIVE_MIN_BLOCK_SIZE_LIMIT   = HIVE_MAX_TRANSACTION_SIZE              = 65536
//	HIVE_MAX_BLOCK_SIZE         = HIVE_MAX_TRANSACTION_SIZE*INTERVAL*2K  = 65536*3*2000
//	HIVE_MAX_WITNESS_URL_LENGTH = 2048
//	HIVE_RD_DECAY_DENOM_SHIFT   = 36
//	HIVE_RD_MAX_POOL_BITS       = 64
//	HIVE_RD_MIN_DECAY_BITS      = 6
//	HIVE_RD_MIN_DECAY           = 1<<6                                   = 64
//	HIVE_RD_MAX_DECAY           = 0xFFFFFFFF
//	HIVE_RD_MIN_BUDGET          = 1
//	HIVE_RD_MAX_BUDGET          = min(BUDGET_1, BUDGET_2, BUDGET_3)
//	  BUDGET_1 = (1<<(64+6-36))-1 = (1<<34)-1     = 17_179_869_183
//	  BUDGET_2 = (1<<(64-36))-1   = (1<<28)-1     = 268_435_455
//	  BUDGET_3 = int32_max                        = 2_147_483_647
//	  → min                                       = 268_435_455
const (
	hiveMinBlockSizeLimit   uint32 = 64 * 1024
	hiveMaxBlockSize        uint32 = 64 * 1024 * 3 * 2000
	hiveMaxWitnessURLLength        = 2048
	hiveRDMinBudget         int32  = 1
	hiveRDMaxBudget         int32  = 268_435_455
	hiveRDMinDecay          uint32 = 64
	hiveRDMaxDecay          uint32 = 0xFFFFFFFF
)

// validateWitnessSetPropertyBlob unpacks a single (key,blob) entry and
// applies the property-specific rules from
// libraries/protocol/hive_operations.cpp::witness_set_properties_operation::validate().
//
// Unknown keys are accepted (the C++ code only inspects the known set).
func validateWitnessSetPropertyBlob(key string, blob []byte) error {
	switch key {
	case "account_creation_fee":
		r := fcraw.NewReader(blob)
		a, err := asset.ReadAsset(r)
		if err != nil {
			return fmt.Errorf("witness_set_properties.account_creation_fee: %w", err)
		}
		if !isHive(a) {
			return errors.New("witness_set_properties.account_creation_fee: must be in HIVE")
		}
		if a.Amount < 0 {
			return errors.New("witness_set_properties.account_creation_fee: smaller than minimum account creation fee")
		}
		if a.Amount > maxAccountCreationFeeSatoshi {
			return errors.New("witness_set_properties.account_creation_fee: greater than maximum account creation fee")
		}

	case "maximum_block_size":
		r := fcraw.NewReader(blob)
		v, err := r.ReadUint32()
		if err != nil {
			return fmt.Errorf("witness_set_properties.maximum_block_size: %w", err)
		}
		if v < hiveMinBlockSizeLimit {
			return fmt.Errorf("witness_set_properties.maximum_block_size: %d smaller than minimum %d", v, hiveMinBlockSizeLimit)
		}
		if v > hiveMaxBlockSize {
			return fmt.Errorf("witness_set_properties.maximum_block_size: %d cannot be more than 2MiB (%d)", v, hiveMaxBlockSize)
		}

	case "sbd_interest_rate", "hbd_interest_rate":
		r := fcraw.NewReader(blob)
		v, err := r.ReadUint16()
		if err != nil {
			return fmt.Errorf("witness_set_properties.hbd_interest_rate: %w", err)
		}
		if v > hivePercent {
			return fmt.Errorf("witness_set_properties.hbd_interest_rate: %d must not exceed 100%% (%d)", v, hivePercent)
		}

	case "new_signing_key":
		// C++ unpacks a 33-byte public_key_type purely to test
		// deserialization. Our wire form is the raw 33 bytes; just
		// enforce length.
		if len(blob) != 33 {
			return fmt.Errorf("witness_set_properties.new_signing_key: expected 33 bytes, got %d", len(blob))
		}

	case "sbd_exchange_rate", "hbd_exchange_rate":
		r := fcraw.NewReader(blob)
		p, err := asset.ReadPrice(r)
		if err != nil {
			return fmt.Errorf("witness_set_properties.hbd_exchange_rate: %w", err)
		}
		if !isHbd(p.Base) {
			return errors.New("witness_set_properties.hbd_exchange_rate: base part of price feed must be in HBD")
		}
		if !isHive(p.Quote) {
			return errors.New("witness_set_properties.hbd_exchange_rate: quote part of price feed must be in HIVE")
		}
		// price::validate(): base > 0, quote > 0, base.symbol != quote.symbol.
		if p.Base.Amount <= 0 {
			return errors.New("witness_set_properties.hbd_exchange_rate.base: amount must be > 0")
		}
		if p.Quote.Amount <= 0 {
			return errors.New("witness_set_properties.hbd_exchange_rate.quote: amount must be > 0")
		}
		if p.Base.Symbol.Num == p.Quote.Symbol.Num {
			return errors.New("witness_set_properties.hbd_exchange_rate: base and quote symbols cannot be the same")
		}

	case "url":
		r := fcraw.NewReader(blob)
		s, err := r.ReadString()
		if err != nil {
			return fmt.Errorf("witness_set_properties.url: %w", err)
		}
		if len(s) == 0 {
			return errors.New("witness_set_properties.url: URL size must be greater than 0")
		}
		if len(s) > hiveMaxWitnessURLLength {
			return fmt.Errorf("witness_set_properties.url: URL is too long (%d > %d)", len(s), hiveMaxWitnessURLLength)
		}
		if !utf8.ValidString(s) {
			return errors.New("witness_set_properties.url: URL is not valid UTF8")
		}

	case "account_subsidy_budget":
		r := fcraw.NewReader(blob)
		v, err := r.ReadInt32()
		if err != nil {
			return fmt.Errorf("witness_set_properties.account_subsidy_budget: %w", err)
		}
		if v < hiveRDMinBudget {
			return fmt.Errorf("witness_set_properties.account_subsidy_budget: %d must be at least %d", v, hiveRDMinBudget)
		}
		if v > hiveRDMaxBudget {
			return fmt.Errorf("witness_set_properties.account_subsidy_budget: %d must be at most %d", v, hiveRDMaxBudget)
		}

	case "account_subsidy_decay":
		r := fcraw.NewReader(blob)
		v, err := r.ReadUint32()
		if err != nil {
			return fmt.Errorf("witness_set_properties.account_subsidy_decay: %w", err)
		}
		if v < hiveRDMinDecay {
			return fmt.Errorf("witness_set_properties.account_subsidy_decay: %d must be at least %d", v, hiveRDMinDecay)
		}
		if v > hiveRDMaxDecay {
			return fmt.Errorf("witness_set_properties.account_subsidy_decay: %d must be at most %d", v, hiveRDMaxDecay)
		}
	}
	return nil
}

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
	for _, p := range o.Props {
		if err := validateWitnessSetPropertyBlob(p.Key, p.Value); err != nil {
			return err
		}
	}
	return nil
}

// ---- CommentOptions / SetWithdrawVestingRoute ------------------------

// HIVE_BENEFICIARY_LIMIT — beneficiary count fits in one wire byte.
// FC_REFLECT requires < 128. Mirrors config.hpp HIVE_BENEFICIARY_LIMIT.
const hiveBeneficiaryLimit = 128

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
	if err := validatePermlink("comment_options.permlink", o.Permlink); err != nil {
		return err
	}
	if err := validateCommentBeneficiaries(o.Beneficiaries); err != nil {
		return err
	}
	return nil
}

// validateCommentBeneficiaries mirrors C++ comment_payout_beneficiaries::validate()
// (libraries/protocol/hive_operations.cpp). Empty list is allowed at the
// CommentOptions level (extension not set); when present, every rule
// applies.
func validateCommentBeneficiaries(bs []CommentBeneficiary) error {
	if len(bs) == 0 {
		return nil
	}
	if len(bs) >= hiveBeneficiaryLimit {
		return fmt.Errorf("comment_options.beneficiaries: cannot specify more than %d beneficiaries", hiveBeneficiaryLimit-1)
	}
	var sum uint32
	var prev string
	for i, b := range bs {
		if err := validateAccountName("comment_options.beneficiaries.account", string(b.Account)); err != nil {
			return err
		}
		if b.Weight > uint16(hivePercent) {
			return fmt.Errorf("comment_options.beneficiaries[%d]: weight %d exceeds 100%%", i, b.Weight)
		}
		sum += uint32(b.Weight)
		if sum > uint32(hivePercent) {
			return errors.New("comment_options.beneficiaries: cannot allocate more than 100% of rewards")
		}
		if i > 0 {
			if string(b.Account) <= prev {
				return fmt.Errorf("comment_options.beneficiaries[%d]: must be sorted ascending by account (got %q after %q)", i, b.Account, prev)
			}
		}
		prev = string(b.Account)
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
	// Mirror C++ hive_operations.cpp:817 — validate_is_utf8(memo).
	// Recurrent transfer op-validate rejects non-UTF8 memos at validate
	// time. Pass-12 recurrent-transfer audit finding A2.
	if !utf8.ValidString(o.Memo) {
		return errors.New("recurrent_transfer.memo: not valid UTF-8")
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
	// C++ dhf_operations.cpp:21 — the DHF ops are the ONLY ones that require a
	// non-empty permlink, and they then apply validate_permlink on top.
	if o.Permlink == "" {
		return errors.New("create_proposal.permlink: must not be empty")
	}
	if err := validatePermlink("create_proposal.permlink", o.Permlink); err != nil {
		return err
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
