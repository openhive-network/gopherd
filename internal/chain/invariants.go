package chain

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// InvariantResult holds the conservation sums recomputed from raw state
// versus the DGPO aggregates. The three Diff fields (HIVE / HBD / VESTS)
// are zero exactly when consensus state is internally consistent — i.e.
// when ghive matches what hived's database::validate_invariants() would
// assert. A nonzero diff means state has silently drifted: every op
// still applied cleanly, but the books no longer balance.
//
// This is the single most valuable drift-detection primitive because it
// needs NO reference hived. Run it every N blocks during replay and the
// FIRST block with a nonzero diff localizes the bug to an interval; a
// re-run with a tighter cadence pins the exact firing block. That turns
// a multi-day "account X is 91 sat short 2M blocks later" hunt into an
// "INVARIANT FAIL block=B law=HIVE Δ=…" abort at the source.
type InvariantResult struct {
	Block uint32

	// HIVE supply identity.
	HiveComputed int64
	HiveSupply   int64
	HiveDiff     int64

	// HBD supply identity.
	HBDComputed int64
	HBDSupply   int64
	HBDDiff     int64

	// VESTS identity (active + pending-reward vests).
	VestsComputed int64
	VestsSupply   int64
	VestsDiff     int64

	// Delegation cross-check: outstanding delegated VESTS must equal
	// received VESTS plus the vests in flight inside expiring-delegation
	// return objects. Nonzero ⇒ a delegation-accounting bug.
	DelegatedSum int64
	ReceivedSum  int64
	ExpiringSum  int64
	DelegDiff    int64

	Accounts int
}

// OK reports whether the three core supply conservation laws hold. These
// are the laws hived's validate_invariants() asserts at every block and
// that ghive maintains by construction — so a violation is a true
// self-inconsistency bug (a leaked/duplicated sat, a counter that wasn't
// bumped), never a false positive. The delegation cross-check is NOT in
// OK() because its exact value depends on in-flight return-object
// semantics that are noisy under partial/corrupted state; treat DelegDiff
// as a debugging hint via InfoString, not a hard assertion.
func (r InvariantResult) OK() bool {
	return r.HiveDiff == 0 && r.HBDDiff == 0 && r.VestsDiff == 0
}

// FailString returns a one-line, greppable description of every broken
// core law (empty when OK).
func (r InvariantResult) FailString() string {
	out := ""
	if r.HiveDiff != 0 {
		out += fmt.Sprintf(" HIVE(supply=%d computed=%d Δ=%+d)", r.HiveSupply, r.HiveComputed, r.HiveDiff)
	}
	if r.HBDDiff != 0 {
		out += fmt.Sprintf(" HBD(supply=%d computed=%d Δ=%+d)", r.HBDSupply, r.HBDComputed, r.HBDDiff)
	}
	if r.VestsDiff != 0 {
		out += fmt.Sprintf(" VESTS(supply=%d computed=%d Δ=%+d)", r.VestsSupply, r.VestsComputed, r.VestsDiff)
	}
	return out
}

// InfoString reports the (informational) delegation cross-check.
func (r InvariantResult) InfoString() string {
	if r.DelegDiff == 0 {
		return ""
	}
	return fmt.Sprintf("DELEG(delegated=%d received=%d expiring=%d Δ=%+d)",
		r.DelegatedSum, r.ReceivedSum, r.ExpiringSum, r.DelegDiff)
}

// CheckInvariants recomputes Hive's conservation identities from raw
// state and compares them against the DGPO aggregates. It is a faithful
// port of hived's database::validate_invariants() (database.cpp): it
// walks every object that can hold HIVE, HBD, or VESTS and asserts the
// totals equal the global counters.
//
// Cost is O(accounts) plus O(open orders/escrows/converts/savings), so
// it is meant to run every N blocks (bisection cadence), not necessarily
// every block. The caller owns the session; CheckInvariants only reads.
func CheckInvariants(s *state.Session) (InvariantResult, error) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return InvariantResult{}, err
	}

	var r InvariantResult
	r.Block = dgpo.HeadBlockNumber

	var hive, hbd, vests int64
	var deleg, recv, expiring int64

	// --- Accounts: liquid, savings, and unclaimed reward balances. ---
	if err := objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		hive += a.Balance + a.SavingsBalance + a.RewardHive
		hbd += a.HBDBalance + a.SavingsHBDBalance + a.RewardHBD
		vests += a.VestingShares + a.RewardVests
		deleg += a.DelegatedVestingShares
		recv += a.ReceivedVestingShares
		r.Accounts++
		return true, nil
	}); err != nil {
		return r, err
	}

	// --- Open limit orders: for_sale is locked HIVE or HBD. ---
	_ = objects.EnsureLimitOrderStore(s).RangeByExpiration(func(lo *objects.LimitOrder) (bool, error) {
		if lo.ForSaleSymbol == uint32(asset.HbdSymbol) {
			hbd += lo.ForSaleAmount
		} else {
			hive += lo.ForSaleAmount
		}
		return true, nil
	})

	// --- Convert requests: HBD locked awaiting settlement. ---
	objects.EnsureConvertRequestStore(s).RangeDueBefore(^uint32(0), func(c objects.ConvertRequest) bool {
		hbd += c.Amount
		return true
	})

	// --- Collateralized converts (HF26+): HIVE collateral escrowed. ---
	_ = objects.RangeAllCollateralizedConvert(s, func(c objects.CollateralizedConvert) bool {
		hive += c.Collateral
		return true
	})

	// --- Escrows: held HIVE + HBD, plus the pending fee in its symbol. ---
	_ = s.Range(objects.EscrowTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		e, err := objects.ReadEscrow(value)
		if err != nil {
			return true, nil
		}
		hive += e.HiveAmount
		hbd += e.HBDAmount
		if e.PendingFeeIsHBD {
			hbd += e.PendingFee
		} else {
			hive += e.PendingFee
		}
		return true, nil
	})

	// --- Savings withdrawals in flight. ---
	_ = s.Range(objects.SavingsWithdrawTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		w, err := objects.ReadSavingsWithdraw(value)
		if err != nil {
			return true, nil
		}
		if w.Symbol == uint32(asset.HbdSymbol) {
			hbd += w.Amount
		} else {
			hive += w.Amount
		}
		return true, nil
	})

	// --- Vesting-delegation return objects: VESTS in flight back to the
	// delegator. They are still part of the delegator's vesting_shares, so
	// they do NOT add to the VESTS identity — but they account for the gap
	// between delegated and received in the cross-check below. ---
	_ = s.Range(objects.VestingDelegationExpirationTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		e, err := objects.ReadVestingDelegationExpiration(value)
		if err != nil {
			return true, nil
		}
		expiring += e.VestingShares
		return true, nil
	})

	// --- Reward fund(s): post-HF17 HIVE reward pools live here. ---
	if rf, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
		hive += rf.RewardBalance + rf.DownvoteRewardBalance
	}

	// --- DGPO-held HIVE: the vesting fund, the legacy reward fund, and
	// the HIVE backing pending-reward vests. ---
	hive += dgpo.TotalVestingFundHive + dgpo.TotalRewardFundHive + dgpo.PendingRewardedVestingHive

	// NOTE: dgpo.DHFIntervalLedger is NOT added here. It is hived's
	// per-interval reporting accumulator for the dhf_funding virtual op
	// (dynamic_global_property_object::dhf_interval_ledger), reset hourly by
	// process_proposals. The HBD it tallies is ALREADY credited to the
	// treasury account balance (funds.go: treasury.HBDBalance += dhfHBD) AND
	// to current_hbd_supply — adding the ledger here double-counted it,
	// failing the HBD identity by the accumulated DHF amount once HF21
	// (block 35,921,786) began funding the treasury.

	r.HiveComputed = hive
	r.HiveSupply = dgpo.CurrentSupply
	r.HiveDiff = dgpo.CurrentSupply - hive

	r.HBDComputed = hbd
	r.HBDSupply = dgpo.CurrentHBDSupply
	r.HBDDiff = dgpo.CurrentHBDSupply - hbd

	r.VestsComputed = vests
	r.VestsSupply = dgpo.TotalVestingShares + dgpo.PendingRewardedVestingShares
	r.VestsDiff = r.VestsSupply - vests

	r.DelegatedSum = deleg
	r.ReceivedSum = recv
	r.ExpiringSum = expiring
	r.DelegDiff = deleg - recv - expiring

	return r, nil
}
