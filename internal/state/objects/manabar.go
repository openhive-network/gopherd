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
// voting power and Resource Credits. Mirrors
// account_object::get_effective_vesting_shares() —
//
//	vesting - delegated_out + received_in - withdrawing_pending
//
// We approximate "withdrawing_pending" as zero since our schema
// doesn't yet split out the next-week's-share. For RC purposes the
// drift is small (<1% of vested) and the value re-syncs on the next
// withdrawal payout.
func (a *Account) EffectiveVestingShares() int64 {
	v := a.VestingShares - a.DelegatedVestingShares + a.ReceivedVestingShares
	if v < 0 {
		return 0
	}
	return v
}

// MaximumRC is the cap for the RC manabar. Mirrors
// account_object::get_maximum_rc() (rc_objects.hpp). Pre-HF22 this
// equals effective_vesting_shares; post-HF22 it incorporates direct
// rc delegations: max_rc = effective_vesting + delegated_rc_in -
// delegated_rc_out.
func (a *Account) MaximumRC() int64 {
	v := a.EffectiveVestingShares() + a.DelegatedRCIn - a.DelegatedRCOut
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
	if params.RegenTime == 0 || params.MaxMana <= 0 {
		return current, now
	}
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

