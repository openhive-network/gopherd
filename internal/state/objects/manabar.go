package objects

import "math/bits"

// RCRegenTimeSec — HIVE_RC_REGEN_TIME = 5 days. Mirrors libraries/
// protocol/include/hive/protocol/config.hpp:481.
const RCRegenTimeSec uint32 = 5 * 24 * 60 * 60

// ManabarParams parameterizes the regeneration computation for a
// manabar. Mirrors hive::chain::util::manabar_params.
type ManabarParams struct {
	MaxMana   int64
	RegenTime uint32
}

// EffectiveVestingShares returns the VESTS amount that backs both
// voting power and Resource Credits — the FALSE-variant
// (`exclude_weekly_power_down=false`) of
// account_object::get_effective_vesting_shares(). Pre-HF20 callers
// use this; HF20+ voting and RC use EffectiveVestingSharesExclWithdraw
// instead.
//
//	effective = vesting - delegated_out + received_in
func (a *Account) EffectiveVestingShares() int64 {
	v := a.VestingShares - a.DelegatedVestingShares + a.ReceivedVestingShares
	if v < 0 {
		return 0
	}
	return v
}

// EffectiveVestingSharesExclWithdraw is the TRUE-variant
// (`exclude_weekly_power_down=true`) of
// account_object::get_effective_vesting_shares(). It additionally
// subtracts the value of the NEXT scheduled power-down slice
// (manabar.hpp:107, rc_utility.cpp:318). HF20+ voting/RC paths use
// this so VESTS about to leave the vesting pool don't inflate
// current vote weight or RC max.
//
//	effective = vesting - delegated_out + received_in - next_withdrawal
//	next_withdrawal = min(vesting_withdraw_rate, to_withdraw - withdrawn)
func (a *Account) EffectiveVestingSharesExclWithdraw() int64 {
	v := a.VestingShares - a.DelegatedVestingShares + a.ReceivedVestingShares
	if a.VestingWithdrawRate > 0 && a.ToWithdraw > a.Withdrawn {
		next := a.VestingWithdrawRate
		remaining := a.ToWithdraw - a.Withdrawn
		if next > remaining {
			next = remaining
		}
		v -= next
	}
	if v < 0 {
		return 0
	}
	return v
}

// MaximumRC is the cap for the RC manabar. Mirrors
// account_object::get_maximum_rc() (account_object.hpp:130-136):
//
//	max_rc = effective_vesting_shares - delegated_rc_out
//	         + delegated_rc_in + rc_adjustment
//
// where rc_adjustment is HIVE_RC_HISTORICAL_ACCOUNT_CREATION_ADJUSTMENT
// (= 2020748973 = ~2020.748973 VESTS) seeded on every account at HF20
// activation. The adjustment recognizes the historical account-
// creation fee as permanent RC capacity, so dust-VESTS accounts still
// have enough to pay for trivial ops.
//
// RC is HF20+ only and always uses the "exclude weekly power-down"
// variant — VESTS scheduled for the next withdrawal don't contribute
// to RC capacity.
func (a *Account) MaximumRC() int64 {
	v := a.EffectiveVestingSharesExclWithdraw() + a.DelegatedRCIn - a.DelegatedRCOut + a.RCAdjustment
	if v < 0 {
		return 0
	}
	return v
}

// RegenerateMana mirrors util::manabar::regenerate_mana<skip_cap_regen>.
// Linearly tops up `current` toward `params.MaxMana` over
// `params.RegenTime`. Caller passes `now` (head_block_time).
//
// skipCapRegen=true matches hived's RC-mode regen which lets
// current_mana refill all the way even after a long absence.
//
// Returns the updated current_mana and last_update_time (the caller
// is expected to write them back to the account).
func RegenerateMana(current int64, lastUpdate uint32, params ManabarParams, now uint32, skipCapRegen bool) (int64, uint32) {
	if params.RegenTime == 0 {
		return current, now
	}
	// C++ clamps current to max BEFORE the dt==0 check — this is also the
	// path that zeroes a stale manabar when effective vests drop to 0
	// (max_mana=0), so it must not be short-circuited for MaxMana<=0.
	if current >= params.MaxMana {
		return params.MaxMana, now
	}
	if now <= lastUpdate {
		return current, lastUpdate
	}
	dt := now - lastUpdate
	if !skipCapRegen && dt > params.RegenTime {
		dt = params.RegenTime
	}
	// regen = max_mana × dt / regen_time. max_mana ≤ ~1e17 satoshi
	// (post-HF1 vesting); dt ≤ 5 days = 432000s. 1e17 × 432000 = 4.3e22
	// which overflows int64 (max 9.2e18). Use math/bits 128-bit
	// multiply + divide.
	hi, lo := bits.Mul64(uint64(params.MaxMana), uint64(dt))
	if hi >= uint64(params.RegenTime) {
		// Quotient would overflow uint64 — saturate.
		newMana := saturatedAdd(current, int64Max)
		if newMana > params.MaxMana {
			newMana = params.MaxMana
		}
		return newMana, now
	}
	regen, _ := bits.Div64(hi, lo, uint64(params.RegenTime))
	if regen > uint64(int64Max) {
		regen = uint64(int64Max)
	}
	newMana := saturatedAdd(current, int64(regen))
	if newMana > params.MaxMana {
		newMana = params.MaxMana
	}
	return newMana, now
}

// UseMana subtracts `cost` from `current`, saturating at minMana.
func UseMana(current, cost, minMana int64) int64 {
	r := saturatedSub(current, cost)
	if r < minMana {
		r = minMana
	}
	return r
}

// RegenerateRCMana refreshes the account's RC manabar in place — the
// account-level wrapper around RegenerateMana that uses the canonical
// RC parameters (max_mana = effective_vesting_shares, regen_time =
// 5 days, skip_cap_regen=true).
//
// Mirrors resource_credits::regenerate_rc_mana (rc_utility.cpp:318).
//
// Caller is responsible for HF gating (this is HF20+ only) and for
// writing the account back via PutAccount.
func RegenerateRCMana(a *Account, now uint32) {
	maxRC := a.MaximumRC()
	params := ManabarParams{MaxMana: maxRC, RegenTime: RCRegenTimeSec}
	newMana, newUpdate := RegenerateMana(a.RCManaCurrent, a.RCManaLastUpdate, params, now, true)
	a.RCManaCurrent = newMana
	a.RCManaLastUpdate = newUpdate
	a.LastMaxRC = maxRC
}

// InitRCManabar primes the manabar on a freshly created account.
// At creation a new account has zero VESTS, so max_rc = 0 and
// current = 0; we set last_update_time so subsequent regen has a
// defined origin.
//
// Caller is responsible for HF gating (HF20+ only).
func InitRCManabar(a *Account, now uint32) {
	a.RCManaCurrent = 0
	a.RCManaLastUpdate = now
	a.LastMaxRC = a.MaximumRC()
}

// HIVE_VOTING_MANA_REGENERATION_SECONDS = 5 days. Same as RC regen.
const VotingManaRegenSec uint32 = 5 * 24 * 60 * 60

// hf21StallBlock = HIVE_HF_21_STALL_BLOCK (0_21.hf). Downvote-manabar
// max_mana uses an overflow-prone int64 multiply at/before this block,
// uint128 after (util/manabar.hpp:120-135).
const hf21StallBlock uint32 = 35922615

// UpdateVotingManabar mirrors hived's util::update_manabar
// (chain/util/manabar.hpp) — regenerates the account's
// voting_manabar (and HF21+ downvote_manabar) to the current
// effective-vesting cap. Called at HF20+ before every vote and
// after any operation that changes effective_vesting_shares
// (claim_reward_balance, create_vesting, fill_vesting_withdraw,
// delegate_vesting_shares).
//
// downvotePoolPercent is dgpo.DownvotePoolPercent (0 pre-HF21,
// 2500 = 25% at HF21+). When non-zero the downvote_manabar's max
// is max_voting * pct / HIVE_100_PERCENT and the voting_manabar's
// max stays at max_voting; pre-HF21 the voting_manabar carries
// the entire pool.
//
// newMana is the just-credited VESTS delta (C++ update_manabar's
// new_mana): the voting bar gains the full delta and the downvote bar
// gains delta×pool_pct/10000, both applied AFTER regeneration via
// use_mana(-new_mana), so they can temporarily exceed the cap until
// the next regen clamps them. Callers crediting vests must invoke this
// BEFORE adding the vests to the account so the regen cap reflects
// pre-credit effective shares (mirrors adjust_account_vesting_balance).
//
// The manabar is in VESTS units post-HF20 — at HF20 activation hived
// reinterprets the stored pre-HF20 percent value as VESTS without
// migration.
func UpdateVotingManabar(a *Account, now uint32, downvotePoolPercent uint16, newMana int64, blockNum uint32) {
	maxMana := a.EffectiveVestingSharesExclWithdraw()
	regen := ManabarParams{MaxMana: maxMana, RegenTime: VotingManaRegenSec}
	cur, newUpdate := RegenerateMana(a.VotingManaCurrent, a.VotingManaLastUpdate, regen, now, false)
	a.VotingManaCurrent = UseMana(cur, -newMana, 0)
	a.VotingManaLastUpdate = newUpdate
	if downvotePoolPercent > 0 {
		// Downvote manabar max_mana. hived (util/manabar.hpp:120-135) gates
		// the multiply on HIVE_HF_21_STALL_BLOCK (35,922,615), NOT HF22:
		// blocks AFTER the stall widen to uint128; blocks AT/BEFORE it use a
		// plain int64 multiply that OVERFLOWS for whale/service downvoters
		// (effective_vests × pool_pct > 2^63) and wraps to a tiny max — the
		// "NOTIFYALERT! max mana overflow" path. Replicating the wrap is
		// required for accounts like ocdb that downvote in the HF21→stall
		// window: a non-overflowed (uint128) downvote bar there absorbs the
		// downvote instead of draining the voting bar, leaving residual
		// voting mana that inflates later downvotes (maysun net_rshares /
		// honeybee curation short at 36,139,951).
		var dvMax int64
		if blockNum > hf21StallBlock {
			dvMax = int64(u128MulDiv10000(maxMana, downvotePoolPercent))
		} else {
			dvMax = (maxMana * int64(downvotePoolPercent)) / 10000
		}
		dvRegen := ManabarParams{MaxMana: dvMax, RegenTime: VotingManaRegenSec}
		newDV, newDVUpdate := RegenerateMana(a.DownvoteManaCurrent, a.DownvoteManaLastUpdate, dvRegen, now, false)
		a.DownvoteManaCurrent = UseMana(newDV, (-newMana*int64(downvotePoolPercent))/10000, 0)
		a.DownvoteManaLastUpdate = newDVUpdate
	}
}

// u128MulDiv10000 computes v×pct/10000 in 128-bit like C++'s post-stall
// downvote max_mana (manabar.hpp uint128_t path); v×2500 overflows
// int64 for v ≳ 3.7e15 VESTS-sat, which whale accounts exceed.
func u128MulDiv10000(v int64, pct uint16) uint64 {
	hi, lo := bits.Mul64(uint64(v), uint64(pct))
	q, _ := bits.Div64(hi, lo, 10000)
	return q
}

const int64Max = int64(^uint64(0) >> 1)

func saturatedAdd(a, b int64) int64 {
	r := a + b
	if (b > 0 && r < a) || (b < 0 && r > a) {
		if b > 0 {
			return int64Max
		}
		return -int64Max - 1
	}
	return r
}

func saturatedSub(a, b int64) int64 {
	r := a - b
	if (b > 0 && r > a) || (b < 0 && r < a) {
		if b > 0 {
			return -int64Max - 1
		}
		return int64Max
	}
	return r
}

