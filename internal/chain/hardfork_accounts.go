package chain

import (
	"errors"
	"fmt"
	"math/bits"
	"os"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
	dvPoolPct := dgpo.DownvotePoolPercent
	clearBlockNum := dgpo.HeadBlockNumber
	for _, dp := range delegations {
		if da, derr := objects.GetAccount(s, string(dp.delegatee)); derr == nil {
			// Mirror hived clear_account (database.cpp:1523-1534): regenerate
			// the delegatee's manabars (with the cap STILL including this
			// delegation), drop the received vesting, then CONSUME mana equal
			// to the removed delegation from BOTH bars (use_mana, clamped at
			// 0 — hived's default min_mana). Go previously only reduced
			// ReceivedVestingShares, leaving the delegatee's voting mana too
			// high: a delegatee of a cleared account kept ~delegation-worth of
			// extra mana, inflating the rshares of EVERY later vote it cast.
			// That drifted comment net_rshares post-HF23 (advocatus's votes
			// ran ~0.6% hot → rollie1212/0ppkt12xw1g net_rshares +15,504,515
			// → viktory88's curation share 1 sat-HIVE low → claim_reward
			// short 1,958,106 VESTS at 41,905,867).
			objects.UpdateVotingManabar(&da, blockTime, dvPoolPct, 0, clearBlockNum)
			da.ReceivedVestingShares -= dp.vests
			if da.ReceivedVestingShares < 0 {
				da.ReceivedVestingShares = 0
			}
			da.VotingManaCurrent = objects.UseMana(da.VotingManaCurrent, dp.vests, 0)
			if dvPoolPct > 0 {
				// downvote cost = delegation × downvote_pool_percent / 100%,
				// computed in 128-bit like hived (uint128_t × pct) to avoid
				// int64 overflow for whale delegations.
				dvHi, dvLo := bits.Mul64(uint64(dp.vests), uint64(dvPoolPct))
				dvCost, _ := bits.Div64(dvHi, dvLo, 10000)
				da.DownvoteManaCurrent = objects.UseMana(da.DownvoteManaCurrent, int64(dvCost), 0)
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
		// Mirror C++ database.cpp:1559 — clear_account calls
		// util::update_manabar(cprops, a) BEFORE zeroing current_mana.
		// That regen has one essential side effect: it stamps BOTH
		// manabars' last_update_time = head_block_time. Go used to set
		// current_mana = 0 while leaving last_update at its stale
		// pre-HF23 value, so the cleared account's manabar then
		// regenerated from that earlier timestamp — over-counting the
		// pre-HF23→HF23 gap (~1000s) as regen time. For an account that
		// keeps voting post-HF23 via received delegations (cleared
		// accounts keep their received_vesting), this inflated voting
		// mana by ~cap×gap/regen_time (e.g. cnfund: ~48B VESTS), which
		// flipped its later vote rshares (+965,189,103 on
		// strawhat/2e6ppq @41,945,011) → reward-fund drift → curator
		// claim short → claim_reward_balance failures (kimzwarch
		// @41,984,169 et al.). Regenerate at the still-full cap (vesting
		// not yet zeroed) so the timestamp lands; the value is discarded
		// by the current_mana = 0 below.
		objects.UpdateVotingManabar(&account, blockTime, dvPoolPct, 0, clearBlockNum)
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
	// hived clear_account (database.cpp:1614-1615) runs these adjust calls
	// UNCONDITIONALLY — even when the balance is 0 — because the account-side
	// AdjustSavingsHBDBalance carries the savings-HBD interest payout/reset that
	// keeps hbd_seconds + savings_hbd_last_interest_payment aligned with hived
	// (reset to the clear block). Gating on `> 0` left 0-savings cleared accounts
	// (e.g. coininstant) with STALE interest state from years earlier (seconds
	// ~4e10, last_payment=2019), phase-shifting their later payout schedule vs
	// hived. Dormant while hbd_interest_rate=0, but once witnesses raised it to
	// 300 (~block 52M, ahead of HF25) that cost coininstant the 51.9M savings-
	// interest payment hived made → 987 sat short at transfer_from_savings block
	// 52,117,581. (hived reset coininstant to seconds=0 / last_int=HF23 time @42M;
	// Go left it stale — confirmed via hived parity-42M API.)
	// The treasury credit is the bug1 fix: cleared savings HBD → treasury LIQUID
	// (not savings), else 296,435 sat is stranded out of the DHF budget base
	// (budget = treasury.HBDBalance/100/24, LIQUID only) → fullalt.dhf short @51,950,208.
	{
		savHBDSnap := account.SavingsHBDBalance
		totalTransferredHBD += savHBDSnap
		objects.AdjustSavingsHBDBalance(s, &account, blockTime, -savHBDSnap)
		objects.AdjustHBDBalance(s, &treasury, blockTime, savHBDSnap)
	}

	// --- LIQUID BALANCES → treasury.
	if account.Balance > 0 {
		totalTransferredHive += account.Balance
		treasury.Balance += account.Balance
		account.Balance = 0
	}
	// Unconditional like hived (database.cpp:1155), same reasoning as savings
	// above: the account-side AdjustHBDBalance resets liquid hbd_seconds/
	// last_interest_payment even at 0 balance, keeping the schedule aligned.
	{
		hbdSnap := account.HBDBalance
		totalTransferredHBD += hbdSnap
		objects.AdjustHBDBalance(s, &account, blockTime, -hbdSnap)
		objects.AdjustHBDBalance(s, &treasury, blockTime, hbdSnap)
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
	if os.Getenv("CLEARTRACE") != "" {
		fmt.Fprintf(os.Stderr, "CLEARTRACE name=%s hive=%d hbd=%d\n", account.Name, totalTransferredHive, totalTransferredHBD)
	}
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

// lockAccount mirrors C++ database::lock_account (database.cpp:1379-1411)
// — replaces an account's owner/active/posting authorities with a
// 1-threshold empty authority (no keys, no account auths), self-assigns
// the recovery_account, zeroes the memo_key, and removes any pending
// recovery / change-recovery requests.
//
// hived calls this at:
//   - HF21 after creating the steem.dao treasury (database_hardfork.cpp:427)
//   - HF24 after creating the hive.fund treasury (database_hardfork.cpp:528)
//
// Locking renders the treasury account permanently non-spendable:
// signing any tx requires meeting the authority threshold (1) with
// either a key or an account auth, neither of which the locked
// authority offers.
func lockAccount(s *state.Session, accountName auth.AccountName, blockTime uint32) {
	// Empty authority — weight_threshold = 1 but no signers can meet it.
	locked := auth.Authority{WeightThreshold: 1}

	authObj, err := objects.GetAccountAuthority(s, accountName)
	if err != nil {
		// Create a fresh locked authority record.
		objects.PutAccountAuthority(s, &objects.AccountAuthority{
			Account:             accountName,
			Owner:               locked,
			Active:              locked,
			Posting:             locked,
			LastOwnerUpdate:     blockTime,
			PreviousOwnerUpdate: blockTime,
		})
	} else {
		authObj.Owner = locked
		authObj.Active = locked
		authObj.Posting = locked
		authObj.PreviousOwnerUpdate = authObj.LastOwnerUpdate
		authObj.LastOwnerUpdate = blockTime
		objects.PutAccountAuthority(s, &authObj)
	}

	// Self-assign recovery_account + zero memo_key on the account record.
	a, err := objects.GetAccount(s, accountName)
	if err == nil {
		a.RecoveryAccount = accountName
		a.MemoKey = [33]byte{}
		a.PreviousOwnerUpdate = a.LastOwnerUpdate
		a.LastOwnerUpdate = blockTime
		objects.PutAccount(s, &a)
	}
	// Note: gopherd does not yet store account_recovery_request or
	// change_recovery_account_request as standalone tables — those
	// rows are derived from in-flight evaluator state. If/when that
	// changes, remove pending requests for `accountName` here to
	// mirror C++ database.cpp:1404-1410.
}
