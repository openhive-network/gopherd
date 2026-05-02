package evaluator

import (
	"errors"
	"fmt"

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
		// New witness: seed VirtualScheduledTime to LAP_LENGTH2 /
		// (votes+1) so the by_schedule_time index has them. Mirrors
		// hived's witness_object construction at create<witness_object>
		// where the multi_index ctor populates schedule fields based
		// on votes (=0 for a fresh witness, so VST = LAP_LENGTH2).
		seedVirtualScheduledTime(&w)
	}
	objects.PutWitness(ctx.DB, &w)
	return nil
}

// seedVirtualScheduledTime initializes the virtual scheduling fields on
// a freshly-created witness. Mirrors witness_schedule.cpp:35-38.
func seedVirtualScheduledTime(w *objects.Witness) {
	// We don't import math/big here directly to avoid pulling
	// uint128 helpers — the chain.uint128* helpers live in the chain
	// package. Instead we hand-roll the simple case: votes is 0 (or
	// some small value) for a brand-new witness, so VST starts at
	// LAP_LENGTH2 / (votes+1). For votes=0, that's LAP_LENGTH2 = 2^128-1.
	// For non-zero votes (existing witnesses being initialized for the
	// first time post-schema-bump), use the math/big helper at the
	// scheduler.
	w.VirtualScheduledTime = [16]byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}
	w.VirtualPosition = [16]byte{}
	w.VirtualLastUpdate = [16]byte{}
}

// witnessSetPropertiesEvaluator implements HF20+ witness_set_properties.
//
// hived parses the flat_map<string, vector<char>> against ~14 known keys
// (account_creation_fee, maximum_block_size, hbd_interest_rate, key,
// new_signing_key, url, account_subsidy_budget, etc.). For ghive's
// minimum-viable implementation we touch the three keys that map to
// existing Witness fields; unknown keys are accepted silently so the
// pipeline doesn't reject otherwise-valid blocks.
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

	// hived: the "key" prop MUST match the witness's current signing
	// key — it's a self-attestation that this op came from the holder
	// of that key (libraries/chain/hive_evaluator.cpp:130). The op has
	// no signature meta, so this `key` is the only proof.
	for _, p := range wsp.Props {
		if p.Key == "key" {
			if len(p.Value) >= 33 {
				var supplied [33]byte
				copy(supplied[:], p.Value[:33])
				if supplied != w.BlockSigningKey {
					return fmt.Errorf("witness_set_properties: 'key' does not match witness signing key for %q", wsp.Owner)
				}
			}
		}
	}

	for _, p := range wsp.Props {
		switch p.Key {
		case "account_creation_fee":
			// 8 bytes amount + 4 or 8 bytes symbol — for legacy_hive_asset.
			// We're lenient here since the symbol form varies.
			if len(p.Value) >= 8 {
				w.AccountCreationFee = int64(le64(p.Value[:8]))
			}
		case "maximum_block_size":
			if len(p.Value) >= 4 {
				w.MaximumBlockSize = le32(p.Value[:4])
			}
		case "hbd_interest_rate":
			if len(p.Value) >= 2 {
				w.HBDInterestRate = uint16(p.Value[0]) | uint16(p.Value[1])<<8
			}
		case "key", "new_signing_key":
			if len(p.Value) >= 33 {
				copy(w.BlockSigningKey[:], p.Value[:33])
			}
		case "url":
			// vector<char> = varint length + bytes; for url the value is the
			// payload bytes verbatim.
			w.URL = string(p.Value)
		}
		// Other keys (account_subsidy_budget, etc.) are silently accepted
		// until corresponding Witness fields land.
	}
	objects.PutWitness(ctx.DB, &w)
	return nil
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

	// Effective vote weight: pre-HF24, the full VestingShares applies
	// immediately. Post-HF24, votes still in the DelayedVotes queue
	// don't count toward the witness's total — only matured votes do.
	// Mirrors hived's account_object::get_governance_vote_power().
	effectiveVests := voter.VestingShares
	if ctx.Hardforks.Has(hardfork.HF24) {
		effectiveVests -= int64(voter.SumDelayedVotes)
		if effectiveVests < 0 {
			effectiveVests = 0
		}
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
		w.TotalVotesVests += delta
		if w.TotalVotesVests < 0 {
			w.TotalVotesVests = 0
		}
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
		// and removes each row).
		objects.ClearWitnessVotesForAccount(ctx.DB, pr.Account, a.VestingShares)
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
