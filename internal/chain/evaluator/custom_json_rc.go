package evaluator

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state/objects"
)

// customJSONEvaluator handles the consensus-level custom_json ops.
// Most custom_json content is plugin-side (no consensus effect), but a
// few payload IDs DO mutate state — most notably "rc" (delegate_rc,
// HF26+). This evaluator dispatches on `id` and applies the rare
// consensus-touching variants; everything else is a no-op.
//
// Payload format for "rc":
//
//	["delegate_rc", { "from": "alice", "delegatees": ["bob","carol"], "max_rc": 5000000 }]
//
// Mirrors hive::protocol::rc_custom_operation in
// libraries/protocol/include/hive/protocol/hive_custom_operations.hpp.
type customJSONEvaluator struct{}

func (customJSONEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagCustomJSON {
		return fmt.Errorf("customJSONEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	cj := opArg.AsCustomJSON()
	switch cj.ID {
	case "rc":
		// RC delegations activate at HF26 (mirrors C++ delegate_rc_evaluator,
		// which silently no-ops pre-HF26). Pre-HF26 we still parse + return
		// nil so a malformed payload doesn't fail the tx.
		if !ctx.Hardforks.Has(hardfork.HF26) {
			return nil
		}
		return applyRCCustomJSON(ctx, *cj)
	}
	// All other custom_json IDs are plugin-data only (no chain state
	// effect). Mirrors hived's behavior — custom_operation_interpreter
	// plugins, not the chain, parse them.
	return nil
}

// applyRCCustomJSON parses the "rc" payload and dispatches the named
// sub-operation. Currently supports `delegate_rc`.
func applyRCCustomJSON(ctx *Context, cj op.CustomJSON) error {
	// Format: ["op_name", {...args...}]
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(cj.JSON), &raw); err != nil {
		// Tolerate malformed JSON — hived also no-ops on bad payload.
		return nil
	}
	if len(raw) != 2 {
		return nil
	}
	// op tag may be either the string "delegate_rc" or the numeric
	// variant index (0 for delegate_rc). Both forms appear in the
	// C++ tests (string for HIVE_RC_CUSTOM_OPERATION_ID, numeric for the
	// "alternative legacy" form).
	var name string
	if err := json.Unmarshal(raw[0], &name); err != nil {
		// try numeric
		var n int
		if err2 := json.Unmarshal(raw[0], &n); err2 == nil && n == 0 {
			name = "delegate_rc"
		} else {
			return nil
		}
	}
	switch name {
	case "delegate_rc":
		return applyDelegateRC(ctx, cj, raw[1])
	}
	return nil
}

// rcRegenSeconds — HIVE_RC_REGEN_TIME, 5 days. Mirrors C++.
const rcRegenSeconds uint32 = 5 * 24 * 60 * 60 // 432000

// maxAccountsPerDelegationOp — HIVE_RC_MAX_ACCOUNTS_PER_DELEGATION_OP.
const maxAccountsPerDelegationOp = 100

// maximumRC returns the effective max RC mana for an account, mirroring
// account_object::get_maximum_rc(only_delegable). Currently we treat
// rc_adjustment as 0 (we don't yet persist the per-account 1-time
// account-creation-fee compensation introduced in HF26).
//
//	total = effective_vesting - DelegatedRCOut + rc_adjustment
//	if !onlyDelegable: total += DelegatedRCIn
//
// Mirror C++ account_object::get_maximum_rc(bool only_delegable) at
// account_object.hpp:130-136. effective_vesting uses the
// excludeWeeklyPowerDown=true variant (RC always subtracts the next
// scheduled withdrawal slice).
func maximumRC(a *objects.Account, onlyDelegable bool) int64 {
	eff := a.EffectiveVestingSharesExclWithdraw()
	total := eff - a.DelegatedRCOut + a.RCAdjustment
	if !onlyDelegable {
		total += a.DelegatedRCIn
	}
	return total
}

// regenerateRCMana advances a.RCManaCurrent toward a.LastMaxRC based on
// elapsed time since RCManaLastUpdate. Mirrors util::manabar::regenerate_mana
// with skip_cap_regen=true (the C++ RC path passes <true>).
//
// Formula:
//
//	if current >= max: snap to max, set last_update = now, return
//	if dt == 0:        return
//	regen = max * dt / HIVE_RC_REGEN_TIME
//	current = sat_add(current, regen), clamped to max
//	last_update = now
func regenerateRCMana(a *objects.Account, now uint32) {
	maxMana := a.LastMaxRC
	if maxMana < 0 {
		maxMana = 0
	}
	if a.RCManaCurrent >= maxMana {
		a.RCManaCurrent = maxMana
		a.RCManaLastUpdate = now
		return
	}
	if now <= a.RCManaLastUpdate {
		return
	}
	dt := now - a.RCManaLastUpdate
	// skip_cap_regen=true (RC path) — do NOT cap dt to regen_time.
	// 128-bit intermediate to avoid overflow.
	regen := mul128Div64(uint64(maxMana), uint64(dt), uint64(rcRegenSeconds))
	if regen > uint64(1<<63-1) {
		regen = uint64(1<<63 - 1)
	}
	nc := a.RCManaCurrent + int64(regen)
	if nc < a.RCManaCurrent { // overflow guard
		nc = int64(1<<63 - 1)
	}
	if nc > maxMana {
		nc = maxMana
	}
	a.RCManaCurrent = nc
	a.RCManaLastUpdate = now
}

// mul128Div64 computes (a * b) / c with a 128-bit intermediate via
// math/bits. Returns 0 when c == 0 (defensive — callers guard against
// the degenerate case but we don't want a panic in dispatch).
//
// When the quotient overflows uint64, returns math.MaxUint64.
func mul128Div64(a, b, c uint64) uint64 {
	if c == 0 {
		return 0
	}
	hi, lo := bits.Mul64(a, b)
	if hi >= c {
		// Quotient would overflow uint64.
		return math.MaxUint64
	}
	q, _ := bits.Div64(hi, lo, c)
	return q
}

// applyDelegateRC handles a delegate_rc payload — set the delegation
// from `from` to each `delegatees` to `max_rc`. Mirrors
// delegate_rc_evaluator::do_apply (libraries/chain/rc/rc_utility.cpp).
//
// Validations (FC_ASSERT in C++):
//   1. op.validate(): from is valid account name, delegatees non-empty,
//      delegatees.size() <= HIVE_RC_MAX_ACCOUNTS_PER_DELEGATION_OP,
//      delegatee != from, max_rc >= 0.
//   2. from must appear in required_posting_auths.
//   3. min_delegation check: if max_rc != 0 and is_in_control,
//      max_rc >= (account_creation_fee/3) * vesting_share_price.
//   4. All delegatees must exist.
//   5. Creating delegation with max_rc==0 is forbidden.
//   6. Setting delegation to the same value is forbidden.
//   7. Total delta cannot exceed from_delegable_rc.
func applyDelegateRC(ctx *Context, cj op.CustomJSON, args json.RawMessage) error {
	// The args may be either the canonical object form
	//   { "from":..., "delegatees":[...], "max_rc":... }
	// or the legacy tuple-array form
	//   [ "alice", ["bob","carol"], N, {} ]
	// (the latter appears in apply_many_different). Try object first.
	var p struct {
		From       string   `json:"from"`
		Delegatees []string `json:"delegatees"`
		MaxRC      int64    `json:"max_rc"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.From == "" {
		// Try array form
		var tup []json.RawMessage
		if err := json.Unmarshal(args, &tup); err != nil || len(tup) < 3 {
			return nil
		}
		if err := json.Unmarshal(tup[0], &p.From); err != nil {
			return nil
		}
		if err := json.Unmarshal(tup[1], &p.Delegatees); err != nil {
			return nil
		}
		if err := json.Unmarshal(tup[2], &p.MaxRC); err != nil {
			return nil
		}
	}

	// 1. op.validate()
	if p.From == "" {
		return fmt.Errorf("delegate_rc: from is empty")
	}
	if !auth.IsValidAccountName(p.From) {
		return fmt.Errorf("delegate_rc: invalid from account name %q", p.From)
	}
	if p.MaxRC < 0 {
		return fmt.Errorf("delegate_rc: max_rc cannot be negative")
	}
	if len(p.Delegatees) == 0 {
		return fmt.Errorf("delegate_rc: must provide at least one delegatee")
	}
	if len(p.Delegatees) > maxAccountsPerDelegationOp {
		return fmt.Errorf("delegate_rc: cannot delegate to more than %d accounts", maxAccountsPerDelegationOp)
	}
	// Deduplicate (flat_set semantics).
	seen := make(map[string]struct{}, len(p.Delegatees))
	uniq := make([]string, 0, len(p.Delegatees))
	for _, d := range p.Delegatees {
		if d == p.From {
			return fmt.Errorf("delegate_rc: cannot delegate to self %q", d)
		}
		if !auth.IsValidAccountName(d) {
			return fmt.Errorf("delegate_rc: invalid delegatee account name %q", d)
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		uniq = append(uniq, d)
	}
	p.Delegatees = uniq

	// 2. Posting-auth gate.
	hasFrom := false
	for _, n := range cj.RequiredPostingAuths {
		if string(n) == p.From {
			hasFrom = true
			break
		}
	}
	if !hasFrom {
		return fmt.Errorf("delegate_rc: from %q not in required_posting_auths", p.From)
	}

	// 3. min_delegation check.
	if p.MaxRC != 0 {
		minDelegation, err := computeMinRCDelegation(ctx)
		if err == nil && p.MaxRC < minDelegation {
			return fmt.Errorf("delegate_rc: cannot delegate less than %d rc", minDelegation)
		}
	}

	// 4-7. Load accounts and apply.
	from, err := objects.GetAccount(ctx.DB, auth.AccountName(p.From))
	if err != nil {
		return fmt.Errorf("delegate_rc: from account %q not found: %w", p.From, err)
	}
	now := ctx.HeadBlockTime

	// Initialize from.LastMaxRC if it hasn't been touched yet, so the
	// regen helper has a sane max. Pre-HF26 the chain never updates
	// LastMaxRC; tests reach here at HF26 with vesting just funded, so
	// snap mana to max once on first contact.
	if from.LastMaxRC == 0 && from.RCManaCurrent == 0 {
		from.LastMaxRC = maximumRC(&from, false)
		from.RCManaCurrent = from.LastMaxRC
		from.RCManaLastUpdate = now
	}
	regenerateRCMana(&from, now)

	deltaTotal := int64(0)
	// Pre-load all delegatees so a missing one fails the whole op.
	type pair struct {
		acct *objects.Account
		prev int64
		has  bool
	}
	pairs := make([]pair, 0, len(p.Delegatees))
	for _, dn := range p.Delegatees {
		to, err := objects.GetAccount(ctx.DB, auth.AccountName(dn))
		if err != nil {
			return fmt.Errorf("delegate_rc: delegatee %q not found: %w", dn, err)
		}
		// Get prior delegation row (if any).
		prevAmt := int64(0)
		has := false
		if pd, err := objects.GetRCDirectDelegation(ctx.DB, from.Name, to.Name); err == nil {
			prevAmt = int64(pd.DelegatedRC)
			has = true
		}
		toCopy := to
		pairs = append(pairs, pair{acct: &toCopy, prev: prevAmt, has: has})
	}

	// Apply each pair.
	for i, dn := range p.Delegatees {
		pr := &pairs[i]
		to := pr.acct
		// Initialize to mana same as from above, for HF26-bootstrap accounts.
		if to.LastMaxRC == 0 && to.RCManaCurrent == 0 {
			to.LastMaxRC = maximumRC(to, false)
			to.RCManaCurrent = to.LastMaxRC
			to.RCManaLastUpdate = now
		}
		regenerateRCMana(to, now)

		delta := p.MaxRC - pr.prev
		if !pr.has {
			if p.MaxRC == 0 {
				return fmt.Errorf("delegate_rc: cannot create delegation to %q with max_rc=0", dn)
			}
			objects.PutRCDirectDelegationFull(ctx.DB, &objects.RCDirectDelegation{
				From:        from.Name,
				To:          to.Name,
				DelegatedRC: uint64(p.MaxRC),
			})
		} else {
			if pr.prev == p.MaxRC {
				return fmt.Errorf("delegate_rc: delegation to %q with same amount already exists", dn)
			}
			if p.MaxRC == 0 {
				objects.DeleteRCDirectDelegationFull(ctx.DB, from.Name, to.Name)
			} else {
				objects.PutRCDirectDelegationFull(ctx.DB, &objects.RCDirectDelegation{
					From:        from.Name,
					To:          to.Name,
					DelegatedRC: uint64(p.MaxRC),
				})
			}
		}
		// update_account_after_rc_delegation: mutate to's mana, last_max_rc, received_rc.
		to.DelegatedRCIn += delta
		to.RCManaCurrent += delta
		if to.RCManaCurrent < 0 {
			to.RCManaCurrent = 0
		}
		// last_max_rc = current max + delta
		toMax := maximumRC(to, false)
		// after adjusting DelegatedRCIn we already include delta;
		// so set last_max_rc to toMax directly.
		to.LastMaxRC = toMax
		objects.PutAccount(ctx.DB, to)

		deltaTotal += delta
	}

	// 7. After-the-fact delta_total check.
	fromDelegableMax := maximumRC(&from, true)
	fromDelegable := fromDelegableMax
	if from.RCManaCurrent < fromDelegable {
		fromDelegable = from.RCManaCurrent
	}
	if fromDelegable < deltaTotal {
		return fmt.Errorf("delegate_rc: account %q insufficient RC (have %d, needs %d)",
			p.From, fromDelegable, deltaTotal)
	}

	// Final from-account update.
	if deltaTotal > 0 {
		from.RCManaCurrent -= deltaTotal
	}
	from.DelegatedRCOut += deltaTotal
	if from.DelegatedRCOut < 0 {
		from.DelegatedRCOut = 0
	}
	from.LastMaxRC = maximumRC(&from, false)
	objects.PutAccount(ctx.DB, &from)
	return nil
}

// computeMinRCDelegation mirrors
//   min_delegation = ((account_creation_fee / 3) * vesting_share_price).amount
//
// vesting_share_price = TotalVestingShares / TotalVestingFundHive
// (with the genesis 1:1000 fallback when either is zero).
func computeMinRCDelegation(ctx *Context) (int64, error) {
	ws, err := objects.GetWitnessSchedule(ctx.DB)
	if err != nil {
		return 0, err
	}
	dgpo, err := objects.GetDGPO(ctx.DB)
	if err != nil {
		return 0, err
	}
	fee := ws.MedianAccountCreationFee
	if fee <= 0 {
		return 0, nil
	}
	hivePortion := fee / 3
	// price multiplies hive_amount by (total_vesting_shares / total_vesting_fund_hive).
	if dgpo.TotalVestingFundHive == 0 || dgpo.TotalVestingShares == 0 {
		// Genesis price: 1 HIVE = 1000 VESTS (at satoshi precision,
		// fund precision is 1e3 vs shares 1e6 → factor 1000).
		return hivePortion * 1000, nil
	}
	// (hivePortion * shares) / fund — guard overflow via mul128Div64.
	q := mul128Div64(uint64(hivePortion), uint64(dgpo.TotalVestingShares), uint64(dgpo.TotalVestingFundHive))
	if q > 1<<63-1 {
		q = 1<<63 - 1
	}
	return int64(q), nil
}

func init() {
	register(op.TagCustomJSON, customJSONEvaluator{})
}

// applyOneRCDelegation applies a single (from → to) delegation update,
// mutating the in-memory `from` and writing `to` back. Retained as the
// public-package API for callers that bypass the JSON parser; the
// evaluator path uses applyDelegateRC directly.
//
// Deprecated: use applyDelegateRC. Kept until external callers migrate.
func applyOneRCDelegation(ctx *Context, from, to *objects.Account, maxRC int64) error {
	if from.Name == to.Name || maxRC < 0 {
		return nil
	}
	prev, err := objects.GetRCDirectDelegation(ctx.DB, from.Name, to.Name)
	prevAmt := int64(0)
	if err == nil {
		prevAmt = int64(prev.DelegatedRC)
	}
	delta := maxRC - prevAmt
	if delta == 0 {
		return nil
	}
	from.DelegatedRCOut += delta
	to.DelegatedRCIn += delta
	if from.DelegatedRCOut < 0 {
		from.DelegatedRCOut = 0
	}
	if to.DelegatedRCIn < 0 {
		to.DelegatedRCIn = 0
	}
	if maxRC == 0 {
		objects.DeleteRCDirectDelegationFull(ctx.DB, from.Name, to.Name)
	} else {
		objects.PutRCDirectDelegationFull(ctx.DB, &objects.RCDirectDelegation{
			From: from.Name, To: to.Name, DelegatedRC: uint64(maxRC),
		})
	}
	objects.PutAccount(ctx.DB, to)
	return nil
}

// ensure unused-warn suppressed if applyOneRCDelegation isn't referenced.
var _ = applyOneRCDelegation
