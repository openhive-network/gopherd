package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Errors specific to witness evaluators.
var (
	ErrWitnessNotFound        = errors.New("witness not found")
	ErrTooManyWitnessVotes    = errors.New("account has too many active witness votes")
	ErrAlreadyVotedForWitness = errors.New("account already votes for this witness")
	ErrNotVotedForWitness     = errors.New("account does not currently vote for this witness")
	ErrCantVoteWithProxy      = errors.New("account witness vote not allowed while a proxy is set")
	ErrSelfProxy              = errors.New("cannot proxy to self")
)

// HIVE_MAX_ACCOUNT_WITNESS_VOTES from libraries/protocol/include/hive/protocol/config.hpp.
const maxAccountWitnessVotes uint16 = 30

// HIVE_GOVERNANCE_VOTE_EXPIRATION_PERIOD = 365 days (HF22+). Each
// governance action (witness vote, witness proxy, proposal vote)
// extends the account's expiration by this much from now.
const governanceVoteExpirationPeriodSec uint32 = 365 * 24 * 60 * 60

// HIVE_HARDFORK_1_25_TIME (mainnet, libraries/protocol/hardfork.d/1_25.hf:10).
const hardfork125TimeSec uint32 = 1625061600

// HARDFORK_1_25_FIRST_GOVERNANCE_VOTE_EXPIRE_TIMESTAMP — the earliest
// expiration time anyone can have post-HF25. Calculated as HF25_TIME +
// 1y. Pre-HF25 votes get redistributed under this anchor.
const hardfork125FirstGovernanceExpireTimestamp uint32 = hardfork125TimeSec + governanceVoteExpirationPeriodSec

// HIVE_HARDFORK_1_25_MAX_OLD_GOVERNANCE_VOTE_EXPIRE_SHIFT — half of one
// year. Used to spread pre-HF25 vote expirations across a 6-month
// window starting at HARDFORK_1_25_FIRST_GOVERNANCE_VOTE_EXPIRE_TIMESTAMP
// instead of clustering them on the HF25 anniversary.
const hardfork125MaxGovernanceExpireShiftSec uint32 = governanceVoteExpirationPeriodSec / 2

// updateGovernanceVoteExpirationTs mirrors C++'s
// account_object::update_governance_vote_expiration_ts (account_object.hpp:280):
//
//	expiration = vote_time + HIVE_GOVERNANCE_VOTE_EXPIRATION_PERIOD
//	if (expiration < HARDFORK_1_25_FIRST_GOVERNANCE_VOTE_EXPIRE_TIMESTAMP)
//	    expiration = HARDFORK_1_25_FIRST_GOVERNANCE_VOTE_EXPIRE_TIMESTAMP
//	               + (expiration % HIVE_HARDFORK_1_25_MAX_OLD_GOVERNANCE_VOTE_EXPIRE_SHIFT)
//
// The redistribution branch spreads pre-HF25 vote expirations across a
// 6-month window so they don't all expire on the HF25 anniversary.
//
// CRITICAL replay semantics: C++ runs this helper for EVERY governance
// action since genesis with the HISTORICAL head time, so for pre-HF25
// actions the redistribution fires every time and the LAST action's
// modulo wins — the value can even REGRESS between actions (the modulo
// is not monotone in voteTime). An earlier Go version applied the spread
// only when stamping a zero field and clamped regressions; that froze
// each account's expiry at its FIRST pre-HF25 action's spread slot and
// diverged from hived for every account with 2+ pre-HF25 governance
// actions — shifting expiry blocks after ~65.7M, corrupting witness
// vote totals → feed schedule → conversion medians (the 65,820,053
// hbdstabilizer wall).
func updateGovernanceVoteExpirationTs(a *objects.Account, voteTime uint32) {
	expiration := voteTime + governanceVoteExpirationPeriodSec
	if expiration < hardfork125FirstGovernanceExpireTimestamp {
		expiration = hardfork125FirstGovernanceExpireTimestamp + (expiration % hardfork125MaxGovernanceExpireShiftSec)
	}
	a.GovernanceVoteExpiration = expiration
}

// witnessUpdateEvaluator implements witness_update.
//
// Hived semantics: creates the witness record if missing, otherwise
// updates url / block_signing_key / props in place. The fee is debited
// only when the witness is being created (HF6 onward) — earlier blocks
// always charged the fee. We mirror the post-HF6 semantics for now.
type witnessUpdateEvaluator struct{}

func (witnessUpdateEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagWitnessUpdate {
		return fmt.Errorf("witnessUpdateEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	wu := opArg.AsWitnessUpdate()
	// hived's witness_update_evaluator only verifies the owner exists.
	// The op's `fee` field is IGNORED — there is no on-chain fee for
	// becoming a witness; the field is a historical artifact. Charging it
	// would lock out witnesses with low HIVE balances. See
	// libraries/chain/hive_evaluator.cpp::witness_update_evaluator::do_apply.
	if has, err := objects.HasAccount(ctx.DB, wu.Owner); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: owner=%q", ErrAccountNotFound, wu.Owner)
	}

	exists, err := objects.HasWitness(ctx.DB, wu.Owner)
	if err != nil {
		return err
	}

	// hived's witness_update_evaluator at hive_evaluator.cpp:139 modifies
	// the existing witness in place, only setting url, signing_key, and
	// chain_properties — every other field is left alone. Mirror that
	// by loading prev and overlaying just those four fields. Earlier
	// versions of this evaluator built a fresh Witness{} and copied a
	// hand-picked subset of fields back from prev, which silently
	// dropped HBDExchangeRate / LastHBDExchangeUpdate every time a
	// witness called witness_update — that broke update_median_feed
	// (rate=0/0 is filtered) and was the source of the post-HF7
	// cashout-amount drift that surfaced as acidsun's HBD short at
	// block 2,904,813.
	var w objects.Witness
	if exists {
		prev, err := objects.GetWitness(ctx.DB, wu.Owner)
		if err != nil {
			return err
		}
		w = prev
		w.URL = wu.URL
		w.BlockSigningKey = wu.BlockSigningKey
		w.AccountCreationFee = wu.Props.AccountCreationFee.Amount
		w.MaximumBlockSize = wu.Props.MaximumBlockSize
		w.HBDInterestRate = wu.Props.HBDInterestRate
	} else {
		w = objects.Witness{
			Owner:              wu.Owner,
			Created:            ctx.HeadBlockTime,
			URL:                wu.URL,
			BlockSigningKey:    wu.BlockSigningKey,
			AccountCreationFee: wu.Props.AccountCreationFee.Amount,
			MaximumBlockSize:   wu.Props.MaximumBlockSize,
			HBDInterestRate:    wu.Props.HBDInterestRate,
			// chain_properties defaults (witness_objects.hpp:54-61) —
			// legacy witness_update can't carry subsidy fields, so they
			// stay at defaults until witness_set_properties (HF20+).
			AccountSubsidyBudget: 797,
			AccountSubsidyDecay:  347321,
		}
		// New witness: seed VirtualScheduledTime to
		// (current_virtual_time + LAP_LENGTH2 / (votes+1)) mod 2^128.
		// Mirrors witness_schedule.cpp:35-38 — the modular wrap is
		// load-bearing: starting from a non-zero current_virtual_time
		// the addition overflows uint128 by LAP_LENGTH2 - 1, placing
		// the new witness JUST BEFORE current_virtual_time so they're
		// the next timeshare candidate.
		seedVirtualScheduledTime(ctx, &w)
	}
	objects.PutWitness(ctx.DB, &w)
	return nil
}

// seedVirtualScheduledTime initializes the virtual scheduling fields on
// a freshly-created witness to match C++ witness_object's default-init
// (witness_objects.hpp:147-149):
//
//	virtual_last_update    = 0
//	virtual_position       = 0
//	virtual_scheduled_time = fc::uint128_max_value()  (= 2^128 - 1)
//
// This puts the new witness at the END of the by_schedule_time index,
// so they're NOT immediately visited by update_witness_schedule's
// timeshare loop. Their VST gets adjusted to a real value via
// adjust_witness_vote when a voter casts a witness vote on them (or
// when their proxied power propagates).
//
// (Pre-fix this function computed VST = current_vt - 1, putting fresh
// witnesses at the FRONT of the index. With votes = 0, the rebuild's
// VST advance = LAP/1 = LAP, overflowing newVT + LAP and triggering a
// chain-wide reset — observed at 2,423 spurious resets between genesis
// and block 2.92 M.)
func seedVirtualScheduledTime(ctx *Context, w *objects.Witness) {
	w.VirtualLastUpdate = [16]byte{}
	w.VirtualPosition = [16]byte{}
	// virtual_scheduled_time = uint128_max_value
	for i := range w.VirtualScheduledTime {
		w.VirtualScheduledTime[i] = 0xff
	}
}

func decodeUint128BE(b [16]byte) (hi, lo uint64) {
	for i := 0; i < 8; i++ {
		hi = (hi << 8) | uint64(b[i])
	}
	for i := 8; i < 16; i++ {
		lo = (lo << 8) | uint64(b[i])
	}
	return
}

func encodeUint128BE(hi, lo uint64) [16]byte {
	var out [16]byte
	for i := 7; i >= 0; i-- {
		out[i] = byte(hi)
		hi >>= 8
	}
	for i := 15; i >= 8; i-- {
		out[i] = byte(lo)
		lo >>= 8
	}
	return out
}

// witnessScheduleVirtualTimeBytes is a tiny helper that returns the
// current schedule's virtual_time as the 16-byte big-endian encoding,
// or zero when no schedule exists yet.
func (c *Context) witnessScheduleVirtualTimeBytes() [16]byte {
	if ws, err := objects.GetWitnessSchedule(c.DB); err == nil {
		return ws.CurrentVirtualTime
	}
	return [16]byte{}
}

// witnessSetPropertiesEvaluator implements HF20+ witness_set_properties.
//
// Mirrors witness_set_properties_evaluator::do_apply at
// libraries/chain/hive_evaluator.cpp:113. hived parses each known key
// out of the flat_map<string, vector<char>> with fc::raw::unpack_from_vector.
// The "key" prop is a self-attestation: it MUST equal the witness's
// current signing key, since witness_set_properties has no signature
// metadata of its own — the chain trusts the op only because the sender
// proves they hold the witness's current signing key.
//
// HF24 (block 47,797,680) renames sbd_* aliases to hbd_*; pre-HF24 ops
// use the sbd_* names, post-HF24 ops use hbd_* (sbd_* is still tolerated
// per C++ fallback at hive_evaluator.cpp:150). We accept either name
// throughout — the first present wins.
type witnessSetPropertiesEvaluator struct{}

func (witnessSetPropertiesEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagWitnessSetProperties {
		return fmt.Errorf("witnessSetPropertiesEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	wsp := opArg.AsWitnessSetProperties()
	// HF20__1620 — witness_set_properties is HF20+ only.
	if !ctx.Hardforks.Has(hardfork.HF20) {
		return fmt.Errorf("witness_set_properties: requires HF20")
	}
	w, err := objects.GetWitness(ctx.DB, wsp.Owner)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: owner=%q (witness_set_properties before witness_update)", ErrWitnessNotFound, wsp.Owner)
		}
		return err
	}

	// Collect props into a map for ergonomic lookup. C++ uses flat_map.
	props := make(map[string][]byte, len(wsp.Props))
	for _, p := range wsp.Props {
		props[p.Key] = p.Value
	}

	// "key" is mandatory and verifies witness identity.
	keyVal, hasKey := props["key"]
	if !hasKey || len(keyVal) < 33 {
		return fmt.Errorf("witness_set_properties: missing 'key' prop for %q", wsp.Owner)
	}
	var supplied [33]byte
	copy(supplied[:], keyVal[:33])
	if supplied != w.BlockSigningKey {
		return fmt.Errorf("witness_set_properties: 'key' does not match witness signing key for %q", wsp.Owner)
	}

	// account_creation_fee: legacy_chain_properties::account_creation_fee
	// is a HIVE asset (8 bytes amount LE + 8 bytes symbol_struct).
	if v, ok := props["account_creation_fee"]; ok && len(v) >= 8 {
		w.AccountCreationFee = int64(le64(v[:8]))
	}

	// maximum_block_size: uint32.
	if v, ok := props["maximum_block_size"]; ok && len(v) >= 4 {
		w.MaximumBlockSize = le32(v[:4])
	}

	// hbd_interest_rate (or sbd_interest_rate pre-HF24): uint16.
	if v, ok := props["hbd_interest_rate"]; ok && len(v) >= 2 {
		w.HBDInterestRate = uint16(v[0]) | uint16(v[1])<<8
	} else if v, ok := props["sbd_interest_rate"]; ok && len(v) >= 2 {
		w.HBDInterestRate = uint16(v[0]) | uint16(v[1])<<8
	}

	// account_subsidy_budget: int32 (HIVE_MIN_ACCOUNT_SUBSIDY_BUDGET = 1,
	// HIVE_MAX_ACCOUNT_SUBSIDY_BUDGET = HIVE_100_PERCENT * HIVE_BLOCKS_PER_DAY).
	if v, ok := props["account_subsidy_budget"]; ok && len(v) >= 4 {
		w.AccountSubsidyBudget = int32(le32(v[:4]))
	}

	// account_subsidy_decay: uint32 (per-block exponential decay rate).
	if v, ok := props["account_subsidy_decay"]; ok && len(v) >= 4 {
		w.AccountSubsidyDecay = le32(v[:4])
	}

	// new_signing_key: rotates BlockSigningKey. Note that "key" is the
	// *current* signing key (already verified above); "new_signing_key"
	// is the replacement.
	if v, ok := props["new_signing_key"]; ok && len(v) >= 33 {
		copy(w.BlockSigningKey[:], v[:33])
	}

	// hbd_exchange_rate (or sbd_exchange_rate pre-HF24): packed price.
	// Updates LastHBDExchangeUpdate to head_block_time per C++ line 178.
	exchangeVal, hasExchange := props["hbd_exchange_rate"]
	if !hasExchange {
		exchangeVal, hasExchange = props["sbd_exchange_rate"]
	}
	if hasExchange {
		// Price serializes as base.amount(8) + base.symbol(8) +
		// quote.amount(8) + quote.symbol(8) = 32 bytes minimum.
		if p, perr := decodeFCPrice(exchangeVal); perr == nil {
			w.HBDExchangeRate = p
			w.LastHBDExchangeUpdate = ctx.HeadBlockTime
		}
	}

	// url: string — the raw value bytes ARE the string per fc::raw::pack
	// of std::string (varuint len + bytes). pack_from_vector strips
	// the length prefix, so we follow suit.
	if v, ok := props["url"]; ok {
		// Strip leading varuint length if present (C++ fc::raw::pack of
		// string prepends the length); fall back to raw bytes if not.
		if s, derr := decodeFCString(v); derr == nil {
			w.URL = s
		} else {
			w.URL = string(v)
		}
	}

	objects.PutWitness(ctx.DB, &w)
	return nil
}

// decodeFCPrice parses an fc::raw-packed price (base + quote, each an
// asset = int64 amount LE + 8-byte symbol). Returns the decoded price
// or an error if the buffer is too short.
func decodeFCPrice(buf []byte) (asset.Price, error) {
	if len(buf) < 32 {
		return asset.Price{}, fmt.Errorf("price: short buffer (%d)", len(buf))
	}
	r := fcraw.NewReader(buf)
	var p asset.Price
	var err error
	if p, err = asset.ReadPrice(r); err != nil {
		return p, err
	}
	return p, nil
}

// decodeFCString parses an fc::raw-packed std::string (varuint length +
// bytes). Returns the decoded string.
func decodeFCString(buf []byte) (string, error) {
	r := fcraw.NewReader(buf)
	return r.ReadString()
}

// accountWitnessVoteEvaluator implements account_witness_vote.
type accountWitnessVoteEvaluator struct{}

func (accountWitnessVoteEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagAccountWitnessVote {
		return fmt.Errorf("accountWitnessVoteEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	wv := opArg.AsAccountWitnessVote()

	voter, err := objects.GetAccount(ctx.DB, wv.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, wv.Account)
		}
		return err
	}
	// hive_evaluator.cpp:267-268 — proxy check FIRST, then can_vote.
	if voter.Proxy != "" {
		return ErrCantVoteWithProxy
	}
	if !voter.CanVote() {
		return fmt.Errorf("account_witness_vote: %q has declined voting rights", wv.Account)
	}
	// hive_evaluator.cpp:269 — UNCONDITIONAL refresh of governance vote
	// expiration BEFORE the vote delta. Pre-HF22 the field is set but
	// unread (idempotent on consensus); HF22+ it gates expiry sweeps.
	updateGovernanceVoteExpirationTs(&voter, ctx.HeadBlockTime)

	w, err := objects.GetWitness(ctx.DB, wv.Witness)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: witness=%q", ErrWitnessNotFound, wv.Witness)
		}
		return err
	}

	had, err := objects.HasWitnessVote(ctx.DB, wv.Account, wv.Witness)
	if err != nil {
		return err
	}

	// Effective vote weight: pre-HF24 == VestingShares; HF24+ subtract
	// SumDelayedVotes. PLUS the voter's proxied_vsf_votes — vests
	// proxied THROUGH this voter from other accounts. Mirrors
	// account_object::get_governance_vote_power() = direct + accumulate
	// (proxied_vsf_votes).
	effectiveVests := directGovernanceVotePower(ctx, &voter)
	for _, v := range voter.ProxiedVSFVotes {
		effectiveVests += v
	}

	if wv.Approve {
		if had {
			return fmt.Errorf("%w: %q -> %q", ErrAlreadyVotedForWitness, wv.Account, wv.Witness)
		}
		// hived only enforces the 30-vote cap post-HF2 (the upstream
		// commit a49e13be78bf… pushed fminerten over the limit before
		// the gate landed). hive_evaluator.cpp:281.
		if ctx.Hardforks.Has(hardfork.HF2) && voter.WitnessesVotedFor >= maxAccountWitnessVotes {
			return fmt.Errorf("%w: %q has %d/%d", ErrTooManyWitnessVotes, wv.Account, voter.WitnessesVotedFor, maxAccountWitnessVotes)
		}
		objects.PutWitnessVote(ctx.DB, wv.Account, wv.Witness)
		voter.WitnessesVotedFor++
		applyWitnessVoteDelta(ctx, &voter, &w, effectiveVests)
	} else {
		if !had {
			return fmt.Errorf("%w: %q -> %q", ErrNotVotedForWitness, wv.Account, wv.Witness)
		}
		// Order matches hive_evaluator.cpp:1406-1419: do the
		// witness-side delta FIRST, then decrement witnesses_voted_for,
		// then remove the witness_vote row LAST. The HF2-era branch of
		// applyWitnessVoteDelta calls adjust_proxied_witness_votes,
		// which walks every witness_vote row the voter holds — if we
		// removed THIS row first, the walk skips this witness and the
		// -delta never lands on it. Concrete repro: at block 934,695
		// steemit unvoted bue; deleting first dropped the 2.6e17
		// decrement so bue's votes stayed inflated by steemit's full
		// vest, flipping the schedule shuffle for ~17K rounds until
		// the cascade healed at block 986,075.
		applyWitnessVoteDelta(ctx, &voter, &w, -effectiveVests)
		if voter.WitnessesVotedFor > 0 {
			voter.WitnessesVotedFor--
		}
		objects.DeleteWitnessVote(ctx.DB, wv.Account, wv.Witness)
	}
	objects.PutAccount(ctx.DB, &voter)
	objects.PutWitness(ctx.DB, &w)
	return nil
}

// applyWitnessVoteDelta mirrors the per-hardfork branch in
// hive_evaluator.cpp:289-299:
//
//	pre-HF2  : adjust THIS witness directly (w.votes += delta)
//	HF2-HF3  : adjust_proxied_witness_votes(voter, delta) — since voter
//	           has no proxy this collapses to walking ALL witnesses voter
//	           votes for and adding delta to each (i.e. the bug HF3 fixed)
//	HF3+     : adjust THIS witness directly
//
// The pre-HF2 / HF3+ paths mutate `w` in place (caller persists). The
// HF2-HF3 path walks the witness_vote table directly via the proxied
// helper, which writes through to storage; the caller's local `w` is
// stale afterwards, so on that path we re-read from storage so the
// caller's PutWitness doesn't overwrite the table-walk's mutation.
func applyWitnessVoteDelta(ctx *Context, voter *objects.Account, w *objects.Witness, delta int64) {
	// Mirror hive_evaluator.cpp:289-299:
	//   pre-HF2  : w.votes += delta (NO virtual_position/VST update)
	//   HF2 only : adjust_proxied_witness_votes(voter, delta) — walks
	//              every witness_vote row for voter, calling
	//              adjust_witness_vote on each (full VST update)
	//   HF3+     : adjust_witness_vote(w, delta) — full VST update on
	//              just this witness
	if ctx.Hardforks.Has(hardfork.HF3) {
		objects.AdjustWitnessVote(ctx.DB, w, delta, ctx.Hardforks)
		return
	}
	if ctx.Hardforks.Has(hardfork.HF2) {
		objects.AdjustProxiedWitnessVotes(ctx.DB, voter, delta)
		if fresh, ferr := objects.GetWitness(ctx.DB, w.Owner); ferr == nil {
			*w = fresh
		}
		return
	}
	// pre-HF2: just bump votes; do NOT advance virtual_position or VST.
	// C++'s pre-HF2 path (hive_evaluator.cpp:295-298) does plain
	// `w.votes -= voter.get_governance_vote_power()` and accepts negative
	// values (`safe<int64_t>` only asserts on overflow, not on going
	// below zero). Clamping to 0 here drops the un-vote subtraction so a
	// later re-vote NET-ADDS the voter's full vest a second time —
	// directly drove block 905,964/905,966 steemit unvote+revote of
	// nextgencrypto inflating Go's votes by ~2.598e17 sat-VESTS and
	// poisoning every witness schedule pick from block 933,600 onwards.
	w.TotalVotesVests += delta
}

// accountWitnessProxyEvaluator implements account_witness_proxy.
//
// hived's flow (libraries/chain/hive_evaluator.cpp:210):
//   - nullify_proxied_witness_votes(account)             [always]
//   - if SETTING a non-empty proxy:
//       - check for proxy-loop (chain ≤ HIVE_MAX_PROXY_RECURSION_DEPTH=4)
//       - clear_witness_votes(account)        [removes direct votes]
//       - set proxy
//       - re-add the account's voting weight up the new proxy chain
//   - if CLEARING the proxy (proxy == ""): just clear the proxy field
//
// proxied_vsf_votes propagation is fully modeled via
// NullifyProxiedWitnessVotes + ApplyProxyAtDepth (proxy_votes.go),
// which recurse up the proxy chain to MaxProxyRecursionDepth and
// adjust each proxy's per-depth slot.
type accountWitnessProxyEvaluator struct{}

func (accountWitnessProxyEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagAccountWitnessProxy {
		return fmt.Errorf("accountWitnessProxyEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	pr := opArg.AsAccountWitnessProxy()
	if pr.Account == pr.Proxy {
		return ErrSelfProxy
	}
	a, err := objects.GetAccount(ctx.DB, pr.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, pr.Account)
		}
		return err
	}
	if !a.CanVote() {
		return fmt.Errorf("account_witness_proxy: %q has declined voting rights", pr.Account)
	}
	// hive_evaluator.cpp:218-232 — when SETTING a new proxy, walk the
	// chain above new_proxy looking for loops AND enforcing
	// proxy_chain.size() <= HIVE_MAX_PROXY_RECURSION_DEPTH (4).
	if pr.Proxy != "" {
		newProxy, err := objects.GetAccount(ctx.DB, pr.Proxy)
		if err != nil {
			if err == state.ErrNotFound {
				return fmt.Errorf("%w: proxy=%q", ErrAccountNotFound, pr.Proxy)
			}
			return err
		}
		// proxy_chain starts {account, new_proxy} → size 2.
		chain := map[auth.AccountName]struct{}{
			pr.Account: {},
			pr.Proxy:   {},
		}
		cur := newProxy
		for cur.Proxy != "" {
			if _, dup := chain[cur.Proxy]; dup {
				return fmt.Errorf("account_witness_proxy: %q would create a proxy loop", pr.Account)
			}
			chain[cur.Proxy] = struct{}{}
			if len(chain) > objects.MaxProxyRecursionDepth {
				return fmt.Errorf("account_witness_proxy: proxy_chain.size() <= HIVE_MAX_PROXY_RECURSION_DEPTH")
			}
			next, err := objects.GetAccount(ctx.DB, cur.Proxy)
			if err != nil {
				break
			}
			cur = next
		}
	}
	// hive_evaluator.cpp:214 — refresh governance vote expiration
	// unconditionally before propagating proxy changes. Mutate locally
	// AND persist before NullifyProxiedWitnessVotes runs, so a later
	// re-read of the account doesn't drop this update.
	updateGovernanceVoteExpirationTs(&a, ctx.HeadBlockTime)
	objects.PutAccount(ctx.DB, &a)

	// Mirror C++ hive_evaluator.cpp:210 — account_witness_proxy unconditionally
	// nullifies the propagation chain first (so any prior proxy stops
	// receiving this account's vest), then either sets a new proxy and
	// re-propagates, or clears the field.
	directPower := directGovernanceVotePower(ctx, &a)
	// hived hive_evaluator.cpp — emit proxy_cleared_operation virtual op
	// whenever the account currently HAS a proxy and (a) the new proxy
	// differs or (b) the new proxy is empty. Both code paths leave the
	// old proxy detached, hence "cleared".
	oldProxy := a.Proxy
	if oldProxy != "" && string(oldProxy) != pr.Proxy {
		ctx.EmitProxyCleared(pr.Account, string(oldProxy))
	}
	objects.NullifyProxiedWitnessVotes(ctx.DB, &a, directPower)
	if pr.Proxy != "" {
		// Setting a new proxy: also drop all direct witness votes
		// (clear_witness_votes in C++ — sets witnesses_voted_for=0
		// and removes each row). Pass voterVesting=0 since the
		// preceding Nullify already subtracted the full propagation
		// (direct + proxied_vsf_votes[*]) from each witness.
		objects.ClearWitnessVotesForAccount(ctx.DB, pr.Account, 0)
		// Re-read after Nullify possibly mutated `a` (proxy chain
		// updates can write back through GetAccount). The fresh row
		// already has the persisted GovernanceVoteExpiration update.
		fresh, ferr := objects.GetAccount(ctx.DB, pr.Account)
		if ferr == nil {
			a = fresh
		}
		a.Proxy = pr.Proxy
		a.WitnessesVotedFor = 0
		objects.ApplyProxyAtDepth(ctx.DB, &a, directPower)
	} else {
		a.Proxy = ""
	}
	objects.PutAccount(ctx.DB, &a)
	return nil
}

// directGovernanceVotePower mirrors C++'s account_object::get_direct_governance_vote_power().
// Pre-HF24: vesting_shares; HF24+: vesting_shares - sum_delayed_votes.
func directGovernanceVotePower(ctx *Context, a *objects.Account) int64 {
	v := a.VestingShares
	if ctx.Hardforks.Has(hardfork.HF24) {
		v -= int64(a.SumDelayedVotes)
		if v < 0 {
			v = 0
		}
	}
	return v
}

// --- low-level LE readers --------------------------------------------

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func init() {
	register(op.TagWitnessUpdate, witnessUpdateEvaluator{})
	register(op.TagWitnessSetProperties, witnessSetPropertiesEvaluator{})
	register(op.TagAccountWitnessVote, accountWitnessVoteEvaluator{})
	register(op.TagAccountWitnessProxy, accountWitnessProxyEvaluator{})
}
