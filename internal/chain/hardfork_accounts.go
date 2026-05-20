package chain

import (
	"errors"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// ErrCollateralizedConvertInClearAccount mirrors hived's FC_ASSERT in
// clear_account when a collateralized_convert_request exists for the
// cleared account. Hived raises an exception; in our test helpers we
// surface this as a typed error so the parity test can match
// HIVE_REQUIRE_THROW.
var ErrCollateralizedConvertInClearAccount = errors.New("Collateralized convert requests not handled by clear_account")

// treasuryNameForClear picks the right treasury account name for
// clear_account / restore_accounts. clear_account runs once at HF23,
// where the treasury is still "steem.dao"; restore_accounts runs at
// HF24, where it has been renamed to "hive.fund". We resolve
// dynamically by checking which account row exists.
func treasuryNameForClear(s *state.Session) string {
	if has, _ := objects.HasAccount(s, "hive.fund"); has {
		if has2, _ := objects.HasAccount(s, "steem.dao"); !has2 {
			return "hive.fund"
		}
	}
	if has, _ := objects.HasAccount(s, "steem.dao"); has {
		return "steem.dao"
	}
	return "hive.fund"
}

// clearAccounts walks `names`, looks up each account, and (if it
// exists) drains it via clearAccount. Mirrors database::clear_accounts
// (libraries/chain/database.cpp:2275). Used at HF23 to wipe the
// Steemit Inc. / Tron-puppet accounts.
func clearAccounts(s *state.Session, names []string) {
	for _, name := range names {
		if has, _ := objects.HasAccount(s, name); !has {
			continue
		}
		_ = clearAccount(s, name)
	}
}

// ClearAccount is the exported public entry point for tests. Internal
// callers should use clearAccount/clearAccounts.
func ClearAccount(s *state.Session, name string) error {
	return clearAccount(s, name)
}

// clearAccount drains an account's balances and pending state into the
// treasury, mirroring database::clear_account (database.cpp:2288).
//
// Post-conditions on the account row:
//   - balance, hbd_balance, savings_balance, savings_hbd_balance: 0
//   - reward_*: 0
//   - vesting_shares, delegated_vesting_shares: 0
//   - vesting_withdraw_rate, to_withdraw, withdrawn: 0
//   - voting/downvote manabars: zeroed
//   - delayed_votes / sum_delayed_votes (HF24+): zeroed
//
// Secondary state cleared:
//   - all vesting_delegation rows where delegator == name
//   - all vesting_delegation_expiration rows where delegator == name
//   - all open escrow rows where from == name (assets returned)
//   - all open limit_order rows where seller == name (for_sale returned)
//   - all open convert_request rows where owner == name (HBD refunded)
//   - all open savings_withdraw rows where from == name (refunded)
//   - assert: no collateralized_convert_request rows for owner.
//
// gatherBalance is called at the end to snapshot the (totalHive,
// totalHBD) pair that crossed to the treasury, so restoreAccounts at
// HF24 can re-credit the same amounts from the treasury.
func clearAccount(s *state.Session, name string) error {
	account, err := objects.GetAccount(s, name)
	if err != nil {
		return err
	}
	dgpo, _ := objects.GetDGPO(s)
	blockTime := dgpo.Time

	treasuryName := treasuryNameForClear(s)
	if string(account.Name) == treasuryName {
		return errors.New("Can't clear treasury account")
	}

	if objects.HasAnyCollateralizedConvertForOwner(s, account.Name) {
		return ErrCollateralizedConvertInClearAccount
	}

	treasury, terr := objects.GetAccount(s, treasuryName)
	if terr != nil {
		treasury = objects.Account{
			Name:                 auth.AccountName(treasuryName),
			Created:              blockTime,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: blockTime,
		}
	}

	// Track pre-clear totals that cross to the treasury. Mirrors
	// hived's total_transferred_hive / total_transferred_hbd counters
	// in database.cpp:2298-2536.
	var totalTransferredHive int64
	var totalTransferredHBD int64

	prefix := state.AppendAccountName(nil, account.Name)

	// --- VESTING DELEGATIONS originated by this delegator.
	type pendingDelegation struct {
		delegatee auth.AccountName
		vests     int64
	}
	var delegations []pendingDelegation
	_ = s.Range(objects.VestingDelegationTable, state.PrimaryIndex, prefix, func(_, value []byte) (bool, error) {
		d, derr := objects.ReadVestingDelegation(value)
		if derr != nil {
			return false, derr
		}
		if d.Delegator != account.Name {
			return false, nil
		}
		delegations = append(delegations, pendingDelegation{
			delegatee: d.Delegatee,
			vests:     d.VestingShares,
		})
		return true, nil
	})
	for _, dp := range delegations {
		if da, derr := objects.GetAccount(s, string(dp.delegatee)); derr == nil {
			da.ReceivedVestingShares -= dp.vests
			if da.ReceivedVestingShares < 0 {
				da.ReceivedVestingShares = 0
			}
			objects.PutAccount(s, &da)
		}
		objects.DeleteVestingDelegation(s, account.Name, dp.delegatee)
	}

	// --- VESTING DELEGATION EXPIRATIONS for this delegator.
	type pendingExp struct {
		id         uint32
		expiration uint32
		vests      int64
	}
	var expirations []pendingExp
	_ = s.Range(objects.VestingDelegationExpirationTable, state.PrimaryIndex, prefix, func(_, value []byte) (bool, error) {
		e, eerr := objects.ReadVestingDelegationExpiration(value)
		if eerr != nil {
			return false, eerr
		}
		if e.Delegator != account.Name {
			return false, nil
		}
		expirations = append(expirations, pendingExp{
			id: e.ID, expiration: e.Expiration, vests: e.VestingShares,
		})
		return true, nil
	})
	for _, ep := range expirations {
		objects.DeleteVestingDelegationExpirationFull(s, &objects.VestingDelegationExpiration{
			ID: ep.id, Delegator: account.Name, Expiration: ep.expiration, VestingShares: ep.vests,
		})
	}

	// --- VESTING_SHARES → HIVE at vesting_share_price → treasury.
	if account.VestingShares > 0 {
		// Mirror C++ database.cpp:1536: BEFORE clearing the vesting
		// field, call adjust_proxied_witness_votes(account, -vesting)
		// so the cleared account's witness votes are removed from every
		// witness it voted for (and from any proxy chain). Without this,
		// the ~40 HF23 cleared accounts' votes (often 50M+ VESTS each)
		// remain in the witness tallies, distorting top-21 election at
		// the very next schedule rebuild — cascade-divergent producers
		// thereafter.
		objects.AdjustProxiedWitnessVotes(s, &account, -account.VestingShares)
		hiveValue := vestsToHive(account.VestingShares, &dgpo)
		dgpo.TotalVestingFundHive -= hiveValue
		dgpo.TotalVestingShares -= account.VestingShares
		treasury.Balance += hiveValue
		account.VestingShares = 0
		account.DelegatedVestingShares = 0
		account.VestingWithdrawRate = 0
		account.NextVestingWithdrawal = 0
		account.ToWithdraw = 0
		account.Withdrawn = 0
		account.VotingManaCurrent = 0
		account.DownvoteManaCurrent = 0
		if hasHF24(s) {
			account.DelayedVotes = nil
			account.SumDelayedVotes = 0
		}
	}

	// --- ESCROWS where from == account: refund into account.
	type pendingEscrow struct {
		escrowID     uint32
		ratification uint32
		hbd, hive    int64
		fee          int64
		feeIsHBD     bool
	}
	var escrows []pendingEscrow
	_ = s.Range(objects.EscrowTable, state.PrimaryIndex, prefix, func(_, value []byte) (bool, error) {
		e, eerr := objects.ReadEscrow(value)
		if eerr != nil {
			return false, eerr
		}
		if e.From != account.Name {
			return false, nil
		}
		escrows = append(escrows, pendingEscrow{
			escrowID: e.EscrowID, ratification: e.RatificationDeadline,
			hbd: e.HBDAmount, hive: e.HiveAmount,
			fee: e.PendingFee, feeIsHBD: e.PendingFeeIsHBD,
		})
		return true, nil
	})
	for _, ep := range escrows {
		account.Balance += ep.hive
		account.HBDBalance += ep.hbd
		if ep.feeIsHBD {
			account.HBDBalance += ep.fee
		} else {
			account.Balance += ep.fee
		}
		objects.DeleteEscrowFull(s, &objects.Escrow{
			From: account.Name, EscrowID: ep.escrowID,
			RatificationDeadline: ep.ratification,
		})
	}

	// --- OPEN LIMIT ORDERS owned by this account: refund for_sale.
	type pendingOrder struct {
		orderID uint32
		amount  int64
		isHBD   bool
	}
	var orders []pendingOrder
	objects.EnsureLimitOrderStore(s).RangeByOwner(account.Name, func(lo objects.LimitOrder) bool {
		orders = append(orders, pendingOrder{
			orderID: lo.OrderID,
			amount:  lo.ForSaleAmount,
			isHBD:   asset.AssetNum(lo.ForSaleSymbol) == asset.HbdSymbol,
		})
		return true
	})
	for _, ord := range orders {
		if ord.isHBD {
			account.HBDBalance += ord.amount
		} else {
			account.Balance += ord.amount
		}
		objects.DeleteLimitOrder(s, account.Name, ord.orderID)
	}

	// --- OPEN CONVERT REQUESTS owned by this account: refund HBD.
	type pendingConvert struct {
		requestID uint32
		amount    int64
	}
	var converts []pendingConvert
	objects.EnsureConvertRequestStore(s).RangeByOwner(account.Name, func(c objects.ConvertRequest) bool {
		converts = append(converts, pendingConvert{
			requestID: c.RequestID,
			amount:    c.Amount,
		})
		return true
	})
	for _, cp := range converts {
		account.HBDBalance += cp.amount
		objects.DeleteConvertRequestFull(s, &objects.ConvertRequest{
			Owner: account.Name, RequestID: cp.requestID,
		})
	}

	// --- SAVINGS WITHDRAWALS from this account: refund the locked sum.
	type pendingSavings struct {
		requestID uint32
		complete  uint32
		amount    int64
		isHBD     bool
	}
	var savingsW []pendingSavings
	_ = s.Range(objects.SavingsWithdrawTable, state.PrimaryIndex, prefix, func(_, value []byte) (bool, error) {
		w, werr := objects.ReadSavingsWithdraw(value)
		if werr != nil {
			return false, werr
		}
		if w.From != account.Name {
			return false, nil
		}
		savingsW = append(savingsW, pendingSavings{
			requestID: w.RequestID, complete: w.Complete,
			amount: w.Amount, isHBD: asset.AssetNum(w.Symbol) == asset.HbdSymbol,
		})
		return true, nil
	})
	for _, sp := range savingsW {
		if sp.isHBD {
			account.HBDBalance += sp.amount
		} else {
			account.Balance += sp.amount
		}
		objects.DeleteSavingsWithdrawFull(s, &objects.SavingsWithdraw{
			From: account.Name, RequestID: sp.requestID, Complete: sp.complete,
		})
	}

	// --- HBD interest touch (HF24+): trigger any pending hbd_seconds
	// payout BEFORE moving the balance out. Mirrors
	// adjust_balance(account, asset(0, HBD_SYMBOL)) in database.cpp:2487.
	if hasHF24(s) && blockTime != 0 {
		objects.AdjustHBDBalance(s, &account, blockTime, 0)
		objects.AdjustSavingsHBDBalance(s, &account, blockTime, 0)
	}

	// --- SAVINGS → treasury.
	if account.SavingsBalance > 0 {
		totalTransferredHive += account.SavingsBalance
		treasury.Balance += account.SavingsBalance
		account.SavingsBalance = 0
	}
	if account.SavingsHBDBalance > 0 {
		totalTransferredHBD += account.SavingsHBDBalance
		objects.AdjustSavingsHBDBalance(s, &treasury, blockTime, account.SavingsHBDBalance)
		account.SavingsHBDBalance = 0
	}

	// --- LIQUID BALANCES → treasury.
	if account.Balance > 0 {
		totalTransferredHive += account.Balance
		treasury.Balance += account.Balance
		account.Balance = 0
	}
	if account.HBDBalance > 0 {
		hbdSnap := account.HBDBalance
		totalTransferredHBD += hbdSnap
		objects.AdjustHBDBalance(s, &treasury, blockTime, hbdSnap)
		account.HBDBalance = 0
	}

	// --- REWARD BALANCES → treasury.
	if account.RewardHive > 0 {
		totalTransferredHive += account.RewardHive
		treasury.Balance += account.RewardHive
		account.RewardHive = 0
	}
	if account.RewardHBD > 0 {
		totalTransferredHBD += account.RewardHBD
		objects.AdjustHBDBalance(s, &treasury, blockTime, account.RewardHBD)
		account.RewardHBD = 0
	}
	if account.RewardVests > 0 || account.RewardVestingHive > 0 {
		// Mirror C++ database.cpp:1604-1612 / 2517-2525: the treasury
		// receives the per-account HIVE backing (account.get_vest_rewards_as_hive
		// = account.RewardVestingHive) directly — NOT vestsToHive() at the
		// current vesting price, since the per-account pending price has
		// long since diverged from the regular price by HF23. Then
		// decrement both pending-pool counters and zero both account-side
		// reward-vest fields.
		treasury.Balance += account.RewardVestingHive
		dgpo.PendingRewardedVestingShares -= account.RewardVests
		dgpo.PendingRewardedVestingHive -= account.RewardVestingHive
		account.RewardVests = 0
		account.RewardVestingHive = 0
	}

	// Snapshot for restore_accounts (HF24). hived's vop records
	// hive_transferred = total_transferred_hive, hbd_transferred =
	// total_transferred_hbd, and gather_balance pushes that pair into
	// hardfork_property_object.h23_balances (database.cpp:2533-2538).
	gatherBalance(s, account.Name, totalTransferredHive, totalTransferredHBD)

	objects.PutAccount(s, &account)
	objects.PutAccount(s, &treasury)
	objects.PutDGPO(s, &dgpo)
	return nil
}

// gatherBalance records the (balance, hbd_balance) pair drained from
// `name` so restore_accounts can re-credit the same amounts at HF24.
// Mirrors database::gather_balance (database.cpp:2267).
//
// Note: hived's hardfork_property_object embeds h23_balances inline; we
// promote it to its own table (H23BalanceTable) to keep the serialized
// HPO schema unchanged.
func gatherBalance(s *state.Session, name auth.AccountName, balance, hbdBalance int64) {
	objects.PutH23Balance(s, &objects.H23Balance{
		Account: name, Balance: balance, HBDBalance: hbdBalance,
	})
}

// GatherBalance is the exported public entry point for tests,
// mirroring hived's database::gather_balance.
func GatherBalance(s *state.Session, name string, balance, hbdBalance int64) {
	gatherBalance(s, auth.AccountName(name), balance, hbdBalance)
}

// restoreAccounts is the HF24 counterpart to clearAccounts. For each
// name in `names`, looks up the h23_balances snapshot (recorded at HF23
// by gather_balance), debits the treasury, and credits the restored
// account with the same (balance, hbd_balance) pair.
//
// Mirrors database::restore_accounts (database.cpp:2228).
func restoreAccounts(s *state.Session, names []string) {
	treasuryName := treasuryNameForClear(s)
	treasury, terr := objects.GetAccount(s, treasuryName)
	if terr != nil {
		return
	}
	dgpo, _ := objects.GetDGPO(s)
	blockTime := dgpo.Time

	for _, name := range names {
		acctName := auth.AccountName(name)
		snap, err := objects.GetH23Balance(s, acctName)
		if err != nil {
			continue
		}
		acct, aerr := objects.GetAccount(s, name)
		if aerr != nil {
			continue
		}
		if snap.Balance > 0 {
			treasury.Balance -= snap.Balance
			acct.Balance += snap.Balance
		}
		if snap.HBDBalance > 0 {
			objects.AdjustHBDBalance(s, &treasury, blockTime, -snap.HBDBalance)
			objects.AdjustHBDBalance(s, &acct, blockTime, snap.HBDBalance)
		}
		objects.PutAccount(s, &acct)
	}
	objects.PutAccount(s, &treasury)
}

// RestoreAccounts is the exported public entry point for tests.
// Production callers route through processHardforks at HF24.
func RestoreAccounts(s *state.Session, names []string) {
	restoreAccounts(s, names)
}

// hasHF24 reports whether HF24 has been processed (i.e. its activation
// hook has fired). Probes HardforkPropertyObject.LastHardfork to avoid
// an import cycle with the hardfork package.
func hasHF24(s *state.Session) bool {
	hpo, err := objects.GetHardforkPropertyObject(s)
	if err != nil {
		return false
	}
	return hpo.LastHardfork >= 24
}

// consolidateTreasuryBalance moves all assets from the old treasury
// (steem.dao) into the new one (hive.fund) at HF24. Mirrors
// database::consolidate_treasury_balance (database.cpp:1267).
func consolidateTreasuryBalance(s *state.Session, blockTime uint32) {
	const (
		oldTreasuryName = "steem.dao"
		newTreasuryName = "hive.fund"
	)
	old, oerr := objects.GetAccount(s, oldTreasuryName)
	if oerr != nil {
		return
	}
	newAcct, nerr := objects.GetAccount(s, newTreasuryName)
	if nerr != nil {
		newAcct = objects.Account{
			Name:                 newTreasuryName,
			Created:              blockTime,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: blockTime,
		}
	}
	newAcct.Balance += old.Balance
	old.Balance = 0
	newAcct.SavingsBalance += old.SavingsBalance
	old.SavingsBalance = 0
	objects.AdjustHBDBalance(s, &newAcct, blockTime, old.HBDBalance)
	old.HBDBalance = 0
	objects.AdjustSavingsHBDBalance(s, &newAcct, blockTime, old.SavingsHBDBalance)
	old.SavingsHBDBalance = 0

	if old.VestingShares > 0 {
		// Defensive parity with C++: even though treasury accounts are
		// locked and have VoteRevoked=true (so AdjustProxiedWitnessVotes
		// is normally a no-op), call it before zeroing VestingShares so
		// any edge-case dangling vote is removed cleanly.
		objects.AdjustProxiedWitnessVotes(s, &old, -old.VestingShares)
		dgpo, _ := objects.GetDGPO(s)
		hiveValue := vestsToHive(old.VestingShares, &dgpo)
		newAcct.Balance += hiveValue
		dgpo.TotalVestingShares -= old.VestingShares
		dgpo.TotalVestingFundHive -= hiveValue
		objects.PutDGPO(s, &dgpo)
		old.VestingShares = 0
	}

	newAcct.RewardHive += old.RewardHive
	old.RewardHive = 0
	newAcct.RewardHBD += old.RewardHBD
	old.RewardHBD = 0
	newAcct.RewardVests += old.RewardVests
	old.RewardVests = 0

	objects.PutAccount(s, &old)
	objects.PutAccount(s, &newAcct)
}
