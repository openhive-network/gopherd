package evaluator

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state/objects"
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
	// hive_evaluator.cpp:340-343: from HF26 (SOLIDIFY_OLD_SOFTFORKS) the JSON
	// may not exceed HIVE_CUSTOM_OP_DATA_MAX_LENGTH (8192) bytes.
	if ctx.Hardforks.Has(hardfork.HF26) && len(cj.JSON) > customOpDataMaxLength {
		return fmt.Errorf("custom_json: operation JSON must be at most %d bytes", customOpDataMaxLength)
	}
	switch cj.ID {
	case "rc":
		// RC delegations activate at HF26 (mirrors C++ delegate_rc_evaluator,
		// which silently no-ops pre-HF26). Pre-HF26 we still parse + return
		// nil so a malformed payload doesn't fail the tx.
		if !ctx.Hardforks.Has(hardfork.HF26) {
			return nil
		}
		if ctx.InControl {
			return applyRCCustomJSON(ctx, *cj)
		}
		// hive_evaluator.cpp:355-370: custom_json_evaluator catches every
		// fc::exception the "rc" interpreter throws and rethrows it only
		// when is_in_control(). Inside a block the operation becomes a
		// no-op, and generic_custom_operation_interpreter::apply_operations
		// ran it under its own undo session, so whatever it had already
		// written is rolled back. That covers ALL failures, not just the
		// payload checks: a missing delegatee, an over-delegation, the
		// "same amount already exists" case. A block hived accepted with
		// such a delegate_rc inside must not fail here.
		sp := ctx.DB.Snapshot()
		if err := applyRCCustomJSON(ctx, *cj); err != nil {
			ctx.DB.Restore(sp)
			return nil
		}
		ctx.DB.DiscardSavepoint(sp)
		return nil
	}
	// All other custom_json IDs are plugin-data only (no chain state
	// effect). Mirrors hived's behavior — custom_operation_interpreter
	// plugins, not the chain, parse them.
	return nil
}

// applyRCCustomJSON interprets an "rc" custom_json and applies every
// delegate_rc it carries. Mirrors generic_custom_operation_interpreter::apply
// and apply_operations (generic_custom_operation_interpreter.hpp:184-251):
//
//   - the payload is ONE operation or a LIST of them — a list is recognised by
//     its first element being an array. Wallets batch dozens of delegations
//     into one custom_json this way, e.g.
//     [["delegate_rc",{...}],["delegate_rc",{...}]];
//   - every operation is validated before any is applied;
//   - the union of their required posting authorities must equal the
//     custom_json's required_posting_auths, and since delegate_rc needs no
//     active authority, required_auths must be empty;
//   - the operations then apply in order, all-or-nothing: hived runs them in
//     one undo session, and any failure undoes the whole batch.
//
// Every failure is returned. customJSONEvaluator decides what it means: fatal
// at admission and block production, a rolled-back no-op inside a block.
func applyRCCustomJSON(ctx *Context, cj op.CustomJSON) error {
	ops, err := ParseRCCustomJSON(cj.JSON, ctx.InControl)
	if err != nil {
		return err
	}
	for i := range ops {
		if err := validateDelegateRC(&ops[i]); err != nil {
			return err
		}
	}
	posting := make(map[string]struct{}, len(cj.RequiredPostingAuths))
	for _, a := range cj.RequiredPostingAuths {
		posting[string(a)] = struct{}{}
	}
	inner := make(map[string]struct{}, len(ops))
	for i := range ops {
		inner[ops[i].From] = struct{}{}
	}
	if len(cj.RequiredAuths) != 0 || !sameAccountSet(inner, posting) {
		return fmt.Errorf("delegate_rc: required authorities of the custom_json do not match its operations")
	}
	for i := range ops {
		if err := applyDelegateRC(ctx, &ops[i]); err != nil {
			return err
		}
	}
	return nil
}

func sameAccountSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// DelegateRCParams is one decoded delegate_rc_operation. Delegatees hold the
// flat_set the operation declares: deduplicated and in sorted order.
type DelegateRCParams struct {
	From       string
	Delegatees []string
	MaxRC      int64
}

// ParseRCCustomJSON decodes the payload of an "rc" custom_json into its
// delegate_rc operations, accepting every form hived does
// (custom_op_from_variant, generic_custom_operation_interpreter.hpp:60-145):
// a single operation or a list; each one either the legacy pair
// [name_or_id, value] or the object {"type": name_or_id, "value": value}. The
// legacy name is "delegate_rc", the object form's "delegate_rc_operation", and
// the numeric id 0. Pairs and objects with extra elements are refused in
// control and tolerated otherwise, as hived does.
//
// RC cost accounting parses the same payload, so it uses this too: what is
// charged then always matches what is applied.
func ParseRCCustomJSON(payload string, inControl bool) ([]DelegateRCParams, error) {
	var top json.RawMessage
	if err := json.Unmarshal([]byte(payload), &top); err != nil {
		return nil, fmt.Errorf("rc custom_json: %w", err)
	}
	elems := []json.RawMessage{top}
	var list []json.RawMessage
	if isJSONArray(top) && json.Unmarshal(top, &list) == nil && len(list) > 0 && isJSONArray(list[0]) {
		elems = list
	}
	out := make([]DelegateRCParams, 0, len(elems))
	for _, e := range elems {
		p, err := parseRCCustomOp(e, inControl)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseRCCustomOp(e json.RawMessage, inControl bool) (DelegateRCParams, error) {
	var tag, value json.RawMessage
	if isJSONArray(e) {
		var pair []json.RawMessage
		if err := json.Unmarshal(e, &pair); err != nil {
			return DelegateRCParams{}, fmt.Errorf("rc custom_json: %w", err)
		}
		if len(pair) != 2 && (inControl || len(pair) < 2) {
			return DelegateRCParams{}, fmt.Errorf("rc custom_json: expected pair of values: [ operation_name_or_id, operation ]")
		}
		tag, value = pair[0], pair[1]
		if err := checkRCOpTag(tag, "delegate_rc"); err != nil {
			return DelegateRCParams{}, err
		}
	} else {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e, &obj); err != nil {
			return DelegateRCParams{}, fmt.Errorf("rc custom_json: input data have to be treated as object")
		}
		var okT, okV bool
		tag, okT = obj["type"]
		value, okV = obj["value"]
		if !okT || !okV {
			return DelegateRCParams{}, fmt.Errorf("rc custom_json: type and value fields are required")
		}
		if len(obj) != 2 && inControl {
			return DelegateRCParams{}, fmt.Errorf("rc custom_json: expected pair of values: { \"type\":operation_name_or_id, \"value\":operation }")
		}
		if err := checkRCOpTag(tag, "delegate_rc_operation"); err != nil {
			return DelegateRCParams{}, err
		}
	}
	return decodeDelegateRC(value)
}

// checkRCOpTag accepts the operation's numeric id (0, the only member of
// rc_custom_operation) or its name in the given serialization.
func checkRCOpTag(tag json.RawMessage, name string) error {
	var n json.Number
	if err := json.Unmarshal(tag, &n); err == nil {
		if n.String() != "0" {
			return fmt.Errorf("rc custom_json: invalid operation id %s", n)
		}
		return nil
	}
	var str string
	if err := json.Unmarshal(tag, &str); err != nil || str != name {
		return fmt.Errorf("rc custom_json: invalid operation name %s", tag)
	}
	return nil
}

// decodeDelegateRC reads delegate_rc_operation's reflected fields. Unknown
// fields (extensions) are ignored and missing ones keep their zero value, as
// fc's from_variant does; validateDelegateRC then refuses what is invalid.
// max_rc is an int64, which fc reads from a JSON number or a numeric string.
// The tuple form [from, [delegatees], max_rc, extensions] is accepted as well.
func decodeDelegateRC(value json.RawMessage) (DelegateRCParams, error) {
	var p DelegateRCParams
	var fromRaw, delRaw, maxRaw json.RawMessage
	if isJSONArray(value) {
		var tup []json.RawMessage
		if err := json.Unmarshal(value, &tup); err != nil || len(tup) < 3 {
			return p, fmt.Errorf("delegate_rc: malformed operation")
		}
		fromRaw, delRaw, maxRaw = tup[0], tup[1], tup[2]
	} else {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(value, &obj); err != nil {
			return p, fmt.Errorf("delegate_rc: malformed operation: %w", err)
		}
		fromRaw, delRaw, maxRaw = obj["from"], obj["delegatees"], obj["max_rc"]
	}
	if fromRaw != nil {
		if err := json.Unmarshal(fromRaw, &p.From); err != nil {
			return p, fmt.Errorf("delegate_rc: from: %w", err)
		}
	}
	if delRaw != nil {
		var ds []string
		if err := json.Unmarshal(delRaw, &ds); err != nil {
			return p, fmt.Errorf("delegate_rc: delegatees: %w", err)
		}
		sort.Strings(ds)
		for i, d := range ds {
			if i == 0 || d != ds[i-1] {
				p.Delegatees = append(p.Delegatees, d)
			}
		}
	}
	if maxRaw != nil {
		v, err := jsonInt64(maxRaw)
		if err != nil {
			return p, fmt.Errorf("delegate_rc: max_rc: %w", err)
		}
		p.MaxRC = v
	}
	return p, nil
}

func jsonInt64(raw json.RawMessage) (int64, error) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.Int64()
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, err
	}
	return strconv.ParseInt(str, 10, 64)
}

func isJSONArray(raw json.RawMessage) bool {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return c == '['
	}
	return false
}

// validateDelegateRC is delegate_rc_operation::validate
// (hive_custom_operations.cpp:12-32).
func validateDelegateRC(p *DelegateRCParams) error {
	if !auth.IsValidAccountName(p.From) {
		return fmt.Errorf("delegate_rc: invalid from account name %q", p.From)
	}
	if len(p.Delegatees) == 0 {
		return fmt.Errorf("delegate_rc: must provide at least one delegatee")
	}
	if len(p.Delegatees) > maxAccountsPerDelegationOp {
		return fmt.Errorf("delegate_rc: cannot delegate to more than %d accounts", maxAccountsPerDelegationOp)
	}
	for _, d := range p.Delegatees {
		if !auth.IsValidAccountName(d) {
			return fmt.Errorf("delegate_rc: invalid delegatee account name %q", d)
		}
		if d == p.From {
			return fmt.Errorf("delegate_rc: cannot delegate to self %q", d)
		}
	}
	if p.MaxRC < 0 {
		return fmt.Errorf("delegate_rc: max_rc cannot be negative")
	}
	return nil
}

// rcRegenSeconds — HIVE_RC_REGEN_TIME, 5 days. Mirrors C++.
const rcRegenSeconds uint32 = 5 * 24 * 60 * 60 // 432000

// ErrRCOverflow is returned when delegate_rc arithmetic would overflow int64.
var ErrRCOverflow = errors.New("delegate_rc: RC amount overflow")

// safeAddRC adds two RC quantities the way hived does. hived carries these
// through fc::safe<int64_t> (share_type), which THROWS on overflow; Go's int64
// wraps silently (hive commit 8cb83d0e3 extended the same hardening to
// update_account_after_rc_delegation).
//
// This is not theoretical. delegate_rc applies one max_rc to up to 100
// delegatees, so a large max_rc makes the running delta_total wrap NEGATIVE —
// and a negative total sails through the "does the delegator have enough RC"
// check. hived rejects such a transaction, which is why no block contains one
// and replay is unaffected; the cost of wrapping is at the head, where we
// would admit and then build a block on a transaction every other node
// refuses. Same shape as the custom-op cap that produced a 579-tx block the
// network threw away: enforce on PRODUCE what the network enforces on RECEIVE.
func safeAddRC(a, b int64) (int64, error) {
	sum := a + b
	// Overflow iff the operands share a sign and the result differs from it.
	if (a > 0 && b > 0 && sum < 0) || (a < 0 && b < 0 && sum >= 0) {
		return 0, fmt.Errorf("%w: %d + %d", ErrRCOverflow, a, b)
	}
	return sum, nil
}

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
	objects.RegenerateRCMana(a, now)
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

// applyDelegateRC applies one validated delegate_rc_operation: set the
// delegation from p.From to each delegatee to p.MaxRC. Mirrors
// delegate_rc_evaluator::do_apply (libraries/chain/rc/rc_utility.cpp).
//
// Checks (FC_ASSERT in C++), after validateDelegateRC and the authority check
// in applyRCCustomJSON:
//  3. min_delegation check: if max_rc != 0 and is_in_control,
//     max_rc >= (account_creation_fee/3) * vesting_share_price.
//  4. All delegatees must exist.
//  5. Creating delegation with max_rc==0 is forbidden.
//  6. Setting delegation to the same value is forbidden.
//  7. Total delta cannot exceed from_delegable_rc.
//
// Inside a block a failure here is not fatal: hived's custom_json_evaluator
// swallows the exception and the interpreter's undo session reverts the whole
// custom_json (see customJSONEvaluator.Apply). Proof on mainnet: block
// 69,612,950 carries a delegatee "ogechukwu-martha\t" with a trailing tab —
// hived included the tx as a no-op.
func applyDelegateRC(ctx *Context, p *DelegateRCParams) error {
	// 3. min_delegation check — C++ gates it on `_db.is_in_control()`
	// (rc_utility.cpp:49-55): enforced only when a tx is first admitted
	// at the head. Blocks legitimately contain below-minimum delegations
	// (first at 68,676,609, ~100 blocks after HF26); replay accepts them.
	if ctx.InControl && p.MaxRC != 0 {
		minDelegation, merr := computeMinRCDelegation(ctx)
		if merr == nil && p.MaxRC < minDelegation {
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
				// C++ asserts unconditionally — but hived only reaches
				// the create path when ITS state has no delegation. Our
				// partial RC model can lack a delegation hived kept
				// (clip divergence, jarvie 68,769,486): in replay a
				// delete-of-missing is a convergent no-op.
				if ctx.InControl {
					return fmt.Errorf("delegate_rc: cannot create delegation to %q with max_rc=0", dn)
				}
				continue
			}
			objects.PutRCDirectDelegationFull(ctx.DB, &objects.RCDirectDelegation{
				From:        from.Name,
				To:          to.Name,
				DelegatedRC: uint64(p.MaxRC),
			})
		} else {
			if pr.prev == p.MaxRC {
				// Same replay-parity reasoning as delete-of-missing: if
				// hived applied this op, ITS prior amount differed; our
				// row disagreeing means our clip/state diverged — treat
				// the (identical-amount) update as a no-op.
				if ctx.InControl {
					return fmt.Errorf("delegate_rc: delegation to %q with same amount already exists", dn)
				}
				continue
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
		// received_rc and current_mana are share_type in hived — overflow throws
		// rather than wrapping.
		newIn, err := safeAddRC(to.DelegatedRCIn, delta)
		if err != nil {
			return err
		}
		to.DelegatedRCIn = newIn
		newMana, err := safeAddRC(to.RCManaCurrent, delta)
		if err != nil {
			return err
		}
		to.RCManaCurrent = newMana
		if to.RCManaCurrent < 0 {
			to.RCManaCurrent = 0
		}
		// last_max_rc = current max + delta
		toMax := maximumRC(to, false)
		// after adjusting DelegatedRCIn we already include delta;
		// so set last_max_rc to toMax directly.
		to.LastMaxRC = toMax
		objects.PutAccount(ctx.DB, to)

		newTotal, err := safeAddRC(deltaTotal, delta)
		if err != nil {
			return err
		}
		deltaTotal = newTotal
	}

	// 7. After-the-fact delta_total check — enforced only InControl.
	// C++'s `from_delegable_rc >= delta_total` assert (rc_utility.cpp:107)
	// is unconditional, and inside a block its failure is swallowed by
	// custom_json_evaluator (the op becomes a no-op). But a hived witness
	// generates its block in control, so a delegate_rc that reached a
	// mainnet block passed this assert under hived's RC state at the time.
	// Re-evaluating it here against OUR mana can only go wrong in one
	// direction: a small RC divergence turns a delegation hived applied
	// into a no-op (first such false rejection seen: anhdaden146 at
	// 68,691,291 — Go said 626G available, hived's state allowed 1T).
	// Applying it unconditionally in a block is therefore the convergent
	// choice, and the same reasoning covers the create-with-0 and
	// same-amount skips above. Revisit once a post-HF26 paired replay
	// shows the RC state matching.
	if ctx.InControl {
		fromDelegableMax := maximumRC(&from, true)
		fromDelegable := fromDelegableMax
		if from.RCManaCurrent < fromDelegable {
			fromDelegable = from.RCManaCurrent
		}
		if fromDelegable < deltaTotal {
			return fmt.Errorf("delegate_rc: account %q insufficient RC (have %d, needs %d)",
				p.From, fromDelegable, deltaTotal)
		}
	}

	// Final from-account update.
	if deltaTotal > 0 {
		from.RCManaCurrent -= deltaTotal
		if from.RCManaCurrent < 0 {
			from.RCManaCurrent = 0
		}
	}
	newOut, err := safeAddRC(from.DelegatedRCOut, deltaTotal)
	if err != nil {
		return err
	}
	from.DelegatedRCOut = newOut
	if from.DelegatedRCOut < 0 {
		from.DelegatedRCOut = 0
	}
	from.LastMaxRC = maximumRC(&from, false)
	objects.PutAccount(ctx.DB, &from)
	return nil
}

// computeMinRCDelegation mirrors
//
//	min_delegation = ((account_creation_fee / 3) * vesting_share_price).amount
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
