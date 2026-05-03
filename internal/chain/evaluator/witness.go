package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
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
// The redistribution prevents every pre-HF25 vote from expiring on the
// same day after HF25 activation.
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

func (witnessUpdateEvaluator) Apply(ctx *Context, o op.Operation) error {
	wu, ok := o.(op.WitnessUpdate)
	if !ok {
		return fmt.Errorf("witnessUpdateEvaluator: unexpected op type %T", o)
	}
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

	w := objects.Witness{
		Owner:              wu.Owner,
		Created:            ctx.HeadBlockTime,
		URL:                wu.URL,
		BlockSigningKey:    wu.BlockSigningKey,
		AccountCreationFee: wu.Props.AccountCreationFee.Amount,
		MaximumBlockSize:   wu.Props.MaximumBlockSize,
		HBDInterestRate:    wu.Props.HBDInterestRate,
	}
	if exists {
		// Preserve cumulative counters that aren't in the op.
		prev, err := objects.GetWitness(ctx.DB, wu.Owner)
		if err == nil {
			w.Created = prev.Created
			w.TotalVotesVests = prev.TotalVotesVests
			w.TotalMissedBlocks = prev.TotalMissedBlocks
			w.HardforkVersionVote = prev.HardforkVersionVote
			w.HardforkTimeVote = prev.HardforkTimeVote
			w.RunningVersion = prev.RunningVersion
			w.VirtualLastUpdate = prev.VirtualLastUpdate
			w.VirtualPosition = prev.VirtualPosition
			w.VirtualScheduledTime = prev.VirtualScheduledTime
			w.PowWorker = prev.PowWorker
		}
	} else {
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
// a freshly-created witness. Mirrors witness_schedule.cpp:35-38:
//
//	new.virtual_last_update    = current_virtual_time
//	new.virtual_scheduled_time = current_virtual_time +
//	                             VIRTUAL_SCHEDULE_LAP_LENGTH2 / (votes + 1)
//
// The addition is plain uint128 arithmetic — when current_virtual_time
// is non-zero it intentionally overflows past 2^128 - 1, wrapping the
// VST to a small value (current_virtual_time - 1 for votes=0) so the
// new witness appears at the FRONT of the by_schedule_time index and
// is picked as the next timeshare slot.
func seedVirtualScheduledTime(ctx *Context, w *objects.Witness) {
	const uint128MaxLowMask uint64 = ^uint64(0)
	// Load current_virtual_time from the witness schedule (zero if no
	// schedule object yet; the genesis seeder populates one).
	var cvtLo, cvtHi uint64
	if ws, err := objects.GetWitnessSchedule(ctx.DB); err == nil {
		hi, lo := decodeUint128BE(ws.CurrentVirtualTime)
		cvtHi, cvtLo = hi, lo
	}
	// VST = current_virtual_time + LAP_LENGTH2  (votes+1=1 for fresh
	// witness with no votes). LAP_LENGTH2 = 2^128 - 1, i.e. all-ones.
	// Sum = (cvtHi:cvtLo) + (^0:^0) = (cvtHi-1+overflow:cvtLo-1+overflow).
	newLo := cvtLo - 1
	carryFromLo := uint64(0)
	if cvtLo == 0 {
		carryFromLo = ^uint64(0) // borrow into hi
	}
	_ = uint128MaxLowMask
	newHi := cvtHi - 1 - carryFromLo
	w.VirtualScheduledTime = encodeUint128BE(newHi, newLo)
	w.VirtualPosition = [16]byte{}
	w.VirtualLastUpdate = ctx.witnessScheduleVirtualTimeBytes()
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

func (witnessSetPropertiesEvaluator) Apply(ctx *Context, o op.Operation) error {
	wsp, ok := o.(op.WitnessSetProperties)
	if !ok {
		return fmt.Errorf("witnessSetPropertiesEvaluator: unexpected op type %T", o)
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

func (accountWitnessVoteEvaluator) Apply(ctx *Context, o op.Operation) error {
	wv, ok := o.(op.AccountWitnessVote)
	if !ok {
		return fmt.Errorf("accountWitnessVoteEvaluator: unexpected op type %T", o)
	}

	voter, err := objects.GetAccount(ctx.DB, wv.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, wv.Account)
		}
		return err
	}
	if !voter.CanVote() {
		return fmt.Errorf("account_witness_vote: %q has declined voting rights", wv.Account)
	}
	if voter.Proxy != "" {
		return ErrCantVoteWithProxy
	}

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
		// hive_evaluator.cpp:269 — UNCONDITIONAL refresh of the
		// governance vote expiration. Pre-HF22 the field exists but
		// is unread (so the write is a harmless idempotent set).
		updateGovernanceVoteExpirationTs(&voter, ctx.HeadBlockTime)
	} else {
		if !had {
			return fmt.Errorf("%w: %q -> %q", ErrNotVotedForWitness, wv.Account, wv.Witness)
		}
		objects.DeleteWitnessVote(ctx.DB, wv.Account, wv.Witness)
		if voter.WitnessesVotedFor > 0 {
			voter.WitnessesVotedFor--
		}
		applyWitnessVoteDelta(ctx, &voter, &w, -effectiveVests)
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
	if !ctx.Hardforks.Has(hardfork.HF2) || ctx.Hardforks.Has(hardfork.HF3) {
		objects.AdjustWitnessVote(ctx.DB, w, delta, ctx.Hardforks)
		return
	}
	objects.AdjustProxiedWitnessVotes(ctx.DB, voter, delta)
	if fresh, ferr := objects.GetWitness(ctx.DB, w.Owner); ferr == nil {
		*w = fresh
	}
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
// We currently elide proxied_vsf_votes (the recursion-up-the-chain
// re-adjustment) since we don't model the per-account
// proxied_vsf_votes[5] array. The clear_witness_votes side-effect IS
// modeled, since not modeling it caused stale witness_vote rows that
// rejected later re-votes ("already votes for this witness").
type accountWitnessProxyEvaluator struct{}

func (accountWitnessProxyEvaluator) Apply(ctx *Context, o op.Operation) error {
	pr, ok := o.(op.AccountWitnessProxy)
	if !ok {
		return fmt.Errorf("accountWitnessProxyEvaluator: unexpected op type %T", o)
	}
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
	// hive_evaluator.cpp:214 — refresh governance vote expiration
	// unconditionally before propagating proxy changes.
	updateGovernanceVoteExpirationTs(&a, ctx.HeadBlockTime)

	// Mirror C++ hive_evaluator.cpp:210 — account_witness_proxy unconditionally
	// nullifies the propagation chain first (so any prior proxy stops
	// receiving this account's vest), then either sets a new proxy and
	// re-propagates, or clears the field.
	directPower := directGovernanceVotePower(ctx, &a)
	objects.NullifyProxiedWitnessVotes(ctx.DB, &a, directPower)
	if pr.Proxy != "" {
		// Setting a new proxy: also drop all direct witness votes
		// (clear_witness_votes in C++ — sets witnesses_voted_for=0
		// and removes each row). Pass voterVesting=0 since the
		// preceding Nullify already subtracted the full propagation
		// (direct + proxied_vsf_votes[*]) from each witness.
		objects.ClearWitnessVotesForAccount(ctx.DB, pr.Account, 0)
		a.WitnessesVotedFor = 0
		a.Proxy = pr.Proxy
		// Re-read after Nullify possibly mutated `a` (proxy chain
		// updates can write back through GetAccount).
		fresh, ferr := objects.GetAccount(ctx.DB, pr.Account)
		if ferr == nil {
			fresh.Proxy = pr.Proxy
			fresh.WitnessesVotedFor = 0
			a = fresh
		}
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
