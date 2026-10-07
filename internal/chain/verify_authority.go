package chain

import (
	"fmt"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// AuthorityLevel is a coarse categorization of which authority a given
// operation requires. Mirrors hived's get_required_*_authorities() per-op
// methods.
type AuthorityLevel int

const (
	AuthorityActive AuthorityLevel = iota
	AuthorityOwner
	AuthorityPosting
	// AuthorityWitness is hived's SEPARATE witness-signature category
	// (get_required_witness_signatures, base.hpp:19). It is NOT an account
	// authority: it is satisfied only by the witness's own signing_key from
	// the witness object, checked in transaction_util.cpp:169.
	//
	// Leaving it unimplemented did not fail safe. Our authorities table
	// correctly returns no ACCOUNT-tier requirement for witness_block_approve,
	// so VerifyAuthority passed such a transaction with ANY signature at all,
	// and the mempool filled with them while real blocks carried zero.
	AuthorityWitness
)

// RequiredAuthorities walks the operations in `signedTx` and returns
// the union of accounts whose authority must sign, grouped by level.
// Mirrors transaction::get_required_authorities (libraries/protocol/
// transaction.cpp:122).
func RequiredAuthorities(signedTx *tx.SignedTransaction) map[AuthorityLevel]map[string]struct{} {
	out := map[AuthorityLevel]map[string]struct{}{
		AuthorityActive:  make(map[string]struct{}),
		AuthorityOwner:   make(map[string]struct{}),
		AuthorityPosting: make(map[string]struct{}),
		AuthorityWitness: make(map[string]struct{}),
	}
	add := func(level AuthorityLevel, name string) {
		if name != "" {
			out[level][name] = struct{}{}
		}
	}
	for _, o := range signedTx.Operations {
		switch o.Tag {
		// Posting-level operations (vote, comment, etc.) — mirrors
		// the per-op get_required_posting_authorities in C++
		// libraries/protocol/hive_operations.cpp.
		// witness_block_approve is authorised ONLY by the witness's signing
		// key, via hived's separate get_required_witness_signatures category
		// (hive_operations.hpp:1047 inserts `witness`).
		case op.TagWitnessBlockApprove:
			add(AuthorityWitness, string(o.AsWitnessBlockApprove().Witness))

		case op.TagVote:
			add(AuthorityPosting, string(o.AsVote().Voter))
		case op.TagComment:
			add(AuthorityPosting, string(o.AsComment().Author))
		case op.TagCommentOptions:
			add(AuthorityPosting, string(o.AsCommentOptions().Author))
		case op.TagDeleteComment:
			add(AuthorityPosting, string(o.AsDeleteComment().Author))
		case op.TagClaimRewardBalance:
			// hive_operations.hpp:claim_reward_balance is posting-level
			// (added via add_authorities(posting)).
			add(AuthorityPosting, string(o.AsClaimRewardBalance().Account))

		// custom_json carries BOTH required_auths (active) AND
		// required_posting_auths (posting). Each is its own list.
		// hive_operations.cpp:get_required_active_authorities /
		// get_required_posting_authorities for custom_json_operation.
		case op.TagCustomJSON:
			cj := o.AsCustomJSON()
			for _, a := range cj.RequiredPostingAuths {
				add(AuthorityPosting, string(a))
			}
			for _, a := range cj.RequiredAuths {
				add(AuthorityActive, string(a))
			}

		// custom (binary, ID-only) — active-only (required_auths).
		case op.TagCustom:
			for _, a := range o.AsCustom().RequiredAuths {
				add(AuthorityActive, string(a))
			}

		// custom_binary — multi-level (owner/active/posting/account_auths).
		case op.TagCustomBinary:
			cb := o.AsCustomBinary()
			for _, a := range cb.RequiredOwnerAuths {
				add(AuthorityOwner, string(a))
			}
			for _, a := range cb.RequiredActiveAuths {
				add(AuthorityActive, string(a))
			}
			for _, a := range cb.RequiredPostingAuths {
				add(AuthorityPosting, string(a))
			}

		// account_update / account_update2: owner-level when the owner
		// authority itself is being touched (hive_operations.cpp:account_
		// update_operation::get_required_authorities), else active.
		case op.TagAccountUpdate:
			au := o.AsAccountUpdate()
			if au.Owner != nil {
				add(AuthorityOwner, string(au.Account))
			} else {
				add(AuthorityActive, string(au.Account))
			}
		case op.TagAccountUpdate2:
			// Mirror hived account_update2_operation exactly
			// (hive_operations.hpp:88-95):
			//   owner   iff owner set
			//   active  iff !owner && (active||posting||memo_key||json_metadata)
			//   posting iff none of the above (only posting_json_metadata /
			//           extensions) — a posting key alone signs it.
			// depressed-dev's block 107,981,294 tx updates only
			// posting_json_metadata → posting authority; demanding active
			// (the old conservative default) rejected the valid posting
			// signature and stalled live P2P sync.
			au := o.AsAccountUpdate2()
			switch {
			case au.Owner != nil:
				add(AuthorityOwner, string(au.Account))
			case au.Active != nil || au.Posting != nil || au.MemoKey != nil || len(au.JSONMetadata) > 0:
				add(AuthorityActive, string(au.Account))
			default:
				add(AuthorityPosting, string(au.Account))
			}

		// Owner-only ops where the required authority is a STATE LOOKUP
		// on the affected account.
		case op.TagChangeRecoveryAccount, op.TagDeclineVotingRights:
			add(AuthorityOwner, PayerOf(o))

		// recover_account and witness_set_properties declare NO account-tier
		// authority. They are authorised through hived's fourth category,
		// `other` (vector<authority> get_required_authorities): embedded
		// authorities that the tx signatures must satisfy directly. Those
		// are collected by RequiredOtherAuthorities, not here. Listing them
		// explicitly keeps them out of the `default` active-of-payer arm.
		case op.TagRecoverAccount, op.TagWitnessSetProperties:

		default:
			// Everything else (transfer, transfer_to_vesting,
			// witness_update, limit_order, claim_*, escrow_*, etc.)
			// requires active.
			add(AuthorityActive, PayerOf(o))
		}
	}
	return out
}

// RequiredOtherAuthorities collects hived's `other` category: authorities
// embedded in the operation itself rather than looked up on an account.
// Mirrors the per-op get_required_authorities( vector<authority>& )
// overloads in hive_operations.hpp:
//
//   - recover_account (hpp:851-855): new_owner_authority AND
//     recent_owner_authority. Both are public data (the open request and
//     owner_authority_history), so without this check an UNSIGNED
//     recover_account for any account with an open request passed
//     verification and only the evaluator's equality checks stood between
//     it and the chain.
//   - witness_set_properties (hpp:475-489): authority(1, props["key"], 1),
//     i.e. the tx must be signed by the witness's current signing key. The
//     evaluator compares props["key"] to the stored key, but that is public
//     too; the signature is the proof.
//   - pow2 with new_owner_key (hpp:751-757): authority(1, new_owner_key, 1).
//     Dead on mainnet since HF17, kept for completeness.
//
// hived verifies these at transaction_util.cpp:124-127 with the ACTIVE
// fetcher for any account_auths they contain. A malformed `key` blob makes
// fc::raw::unpack throw in hived, so it is an error here too.
func RequiredOtherAuthorities(signedTx *tx.SignedTransaction) ([]auth.Authority, error) {
	var out []auth.Authority
	keyAuthority := func(k crypto.PublicKey) auth.Authority {
		return auth.Authority{
			WeightThreshold: 1,
			KeyAuths:        []auth.KeyAuth{{Key: k, Weight: 1}},
		}
	}
	for _, o := range signedTx.Operations {
		switch o.Tag {
		case op.TagRecoverAccount:
			ra := o.AsRecoverAccount()
			out = append(out, ra.NewOwnerAuthority, ra.RecentOwnerAuthority)
		case op.TagWitnessSetProperties:
			wsp := o.AsWitnessSetProperties()
			for _, p := range wsp.Props {
				if p.Key != "key" {
					continue
				}
				if len(p.Value) < crypto.PublicKeySize {
					return nil, fmt.Errorf("verify_authority: witness_set_properties 'key' prop is %d bytes, need %d", len(p.Value), crypto.PublicKeySize)
				}
				var k crypto.PublicKey
				copy(k[:], p.Value[:crypto.PublicKeySize])
				out = append(out, keyAuthority(k))
				break
			}
		case op.TagPow2:
			if k := o.AsPow2().NewOwnerKey; k != nil {
				out = append(out, keyAuthority(*k))
			}
		}
	}
	return out, nil
}

// VerifyAuthority is the entry point for live-mode signature
// verification. It:
//
//  1. Recovers signing public keys from each tx.Signatures via secp256k1.
//  2. Looks up each required authority via the supplied state session.
//  3. Confirms the recovered keys cover each required authority's threshold
//     (with up to HIVE_MAX_SIG_CHECK_DEPTH levels of account_auths recursion).
//
// Returns nil on accept, an error otherwise. SkipSig short-circuits to
// nil to mirror hived's skip_transaction_signatures behavior. `chainID`
// is the chain identifier that gets prepended to the unsigned tx bytes
// in the sig digest; pass network.ChainID for HF24+ and network.OldChainID
// (zero) pre-HF24.
func VerifyAuthority(s *state.Session, signedTx *tx.SignedTransaction, unsignedBytes []byte, chainID network.Hash32, skipSig bool) error {
	if skipSig {
		return nil
	}
	keys, err := RecoverTxSignerKeys(signedTx, unsignedBytes, chainID)
	if err != nil {
		return err
	}
	return VerifyAuthorityWithKeys(s, signedTx, keys)
}

// ErrDuplicateSig is returned when two signatures recover the same
// public key. Mirrors hived's tx_duplicate_sig exception
// (libraries/protocol/transaction.cpp:108, FC_ASSERT in
// get_signature_keys).
var ErrDuplicateSig = fmt.Errorf("verify_authority: duplicate signature included")

// ErrIrrelevantSig is returned when a tx carries one or more signatures
// that were not needed to satisfy any required authority. Mirrors
// hived's tx_irrelevant_sig exception (libraries/protocol/
// transaction_util.cpp:51, FC_ASSERT on !remove_unused_signatures()).
var ErrIrrelevantSig = fmt.Errorf("verify_authority: unnecessary signature(s) detected (irrelevant)")

// RecoverTxSignerKeys recovers the public keys behind each tx.Signatures
// entry. Pure CPU work — no state access — so callers may run it in
// parallel across goroutines (the apply pipeline fans this out before
// handing the job to the single-writer applyLoop).
//
// chainID is the prefix mixed into the sig digest (zero pre-HF24,
// network.ChainID HF24+).
//
// Returns ErrDuplicateSig if two distinct signatures recover the same
// public key — mirrors hived's `flat_set::insert(...).second` assertion
// in signed_transaction::get_signature_keys.
func RecoverTxSignerKeys(signedTx *tx.SignedTransaction, unsignedBytes []byte, chainID network.Hash32) ([]crypto.PublicKey, error) {
	digest := crypto.SHA256Of2(chainID[:], unsignedBytes)
	keys := make([]crypto.PublicKey, 0, len(signedTx.Signatures))
	seen := make(map[crypto.PublicKey]struct{}, len(signedTx.Signatures))
	for i, sig := range signedTx.Signatures {
		var compact crypto.CompactSignature
		copy(compact[:], sig[:])
		if !compact.IsCanonical() {
			return nil, fmt.Errorf("verify_authority: signature %d non-canonical", i)
		}
		k, err := crypto.RecoverPublicKey(digest, compact)
		if err != nil {
			return nil, fmt.Errorf("verify_authority: recover signature %d: %w", i, err)
		}
		if _, dup := seen[k]; dup {
			return nil, ErrDuplicateSig
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	return keys, nil
}

// VerifyAuthorityWithKeys is VerifyAuthority's second half: given the
// pre-recovered set of signer pubkeys, walk the required authorities
// against state and confirm each is covered. Uses no goroutines; the
// state.Session must be owned by the caller.
func VerifyAuthorityWithKeys(s *state.Session, signedTx *tx.SignedTransaction, keys []crypto.PublicKey) error {
	required := RequiredAuthorities(signedTx)
	other, err := RequiredOtherAuthorities(signedTx)
	if err != nil {
		return err
	}
	// Hived's verify_authority does NOT enforce "len(sigs)>0" globally;
	// each required authority is checked individually. Operations like
	// pow_operation declare zero required authorities ("there is no need
	// to verify authority, the proof of work is sufficient", per
	// hive_operations.hpp:707) and legitimately ship as 0-sig txs in
	// early mainnet (every tx in block 1092..N until pow ends). Reject
	// only when a required authority cannot be satisfied below.
	if len(required[AuthorityOwner]) == 0 &&
		len(required[AuthorityActive]) == 0 &&
		len(required[AuthorityPosting]) == 0 &&
		len(required[AuthorityWitness]) == 0 &&
		len(other) == 0 {
		// No required authorities — nothing to satisfy. (Hived would
		// also reject "unused signatures" pre-HF28, but in practice
		// pow txs never carry sigs and we don't currently track
		// per-key consumption; skip that strictness for now.)
		return nil
	}

	// Mirror C++ transaction_util.cpp:69-73 — pre-HF28 reject any tx
	// that combines posting-required ops with active- or owner-required
	// ops (the "mixed_with_posting" verify_authority_problem). HF28's
	// HIVE_HARDFORK_1_28_ALLOW_STRICT_AND_MIXED_AUTHORITIES lifts the
	// restriction (database.cpp:2646 passes the flag into
	// verify_authority), so the guard is pre-HF28 only.
	// HF28 gates TWO independent relaxations, and C++ passes both at the same
	// call site (database.cpp:2646-2647):
	//   HIVE_HARDFORK_1_28_ALLOW_STRICT_AND_MIXED_AUTHORITIES -> mixing posting
	//     with active/owner in one tx
	//   HIVE_HARDFORK_1_28_ALLOW_REDUNDANT_SIGNATURES          -> the
	//     remove_unused_signatures / tx_irrelevant_sig assert below
	hf28 := false
	if dgpoRef, ok := objects.GetDGPORef(s); ok {
		hf28 = hardfork.SetForBlock(dgpoRef.HeadBlockNumber).Has(hardfork.HF28)
	}
	// transaction_util.cpp:99-104 lists all four non-posting categories in
	// the mixed check: active, owner, witness and other.
	if len(required[AuthorityPosting]) > 0 &&
		(len(required[AuthorityActive]) > 0 || len(required[AuthorityOwner]) > 0 ||
			len(required[AuthorityWitness]) > 0 || len(other) > 0) {
		if !hf28 {
			return fmt.Errorf("verify_authority: tx mixes posting-required ops with active/owner-required ops")
		}
	}

	fetcher := &sessionAuthFetcher{s: s}
	state := newSignState(keys, fetcher)

	// Walk required authorities by level. Each must be satisfied.
	//
	// Role upgrade — a higher authority standing in for a lower one (owner
	// for active; active or owner for posting) — is PRE-HF28 ONLY. With
	// HIVE_HARDFORK_1_28_ALLOW_STRICT_AND_MIXED_AUTHORITIES hived calls
	// s.check_authority(id) alone (transaction_util.cpp:84-86 posting,
	// :131-134 active); the get_active/get_owner fallbacks at :88-100 and
	// :137-150 are the `else` branch. On mainnet today a vote signed with
	// the active key, or a transfer signed with the owner key, is REJECTED
	// by hived (tx_missing_posting_auth / tx_missing_active_auth). Keeping
	// the fallbacks unconditionally let gopherd admit, relay, and put into
	// its own blocks transactions every hived node refuses.
	allowRoleUpgrade := !hf28
	for _, name := range sortedSet(required[AuthorityOwner]) {
		if !state.checkAuthority(AuthorityOwner, name, 0) {
			return fmt.Errorf("verify_authority: missing owner authority for %q", name)
		}
	}
	for _, name := range sortedSet(required[AuthorityActive]) {
		if !state.checkAuthority(AuthorityActive, name, 0) {
			if !allowRoleUpgrade || !state.checkAuthority(AuthorityOwner, name, 0) {
				return fmt.Errorf("verify_authority: missing active authority for %q", name)
			}
		}
	}
	for _, name := range sortedSet(required[AuthorityPosting]) {
		if !state.checkAuthority(AuthorityPosting, name, 0) {
			if !allowRoleUpgrade ||
				(!state.checkAuthority(AuthorityActive, name, 0) &&
					!state.checkAuthority(AuthorityOwner, name, 0)) {
				return fmt.Errorf("verify_authority: missing posting authority for %q", name)
			}
		}
	}
	// `other` category — transaction_util.cpp:124-127: each embedded
	// authority is checked as-is, with account_auths resolved at the ACTIVE
	// level (change_current_authority(get_active) precedes the loop). No
	// role upgrade applies: there is no account to upgrade through.
	for i := range other {
		if !state.satisfies(&other[i], AuthorityActive, 0) {
			return fmt.Errorf("verify_authority: missing signature for embedded authority %d (recover_account / witness_set_properties)", i)
		}
	}
	// Witness-signature category — C++ transaction_util.cpp:169. Satisfied
	// ONLY by the witness's own signing_key from the witness object; no
	// account-authority substitution applies, and owner/active cannot stand in
	// for it. A missing witness object is its own failure
	// (missing_witness_key), not a pass.
	for _, name := range sortedSet(required[AuthorityWitness]) {
		w, werr := objects.GetWitness(s, auth.AccountName(name))
		if werr != nil {
			return fmt.Errorf("verify_authority: no witness object for %q (missing witness key): %w", name, werr)
		}
		if !state.signedByKey(w.BlockSigningKey) {
			return fmt.Errorf("verify_authority: missing witness signature for %q", name)
		}
	}
	// Mirrors hived's `!s.remove_unused_signatures()` final assert in
	// libraries/protocol/transaction_util.cpp:111/185 — every signature
	// supplied must have been consumed by at least one satisfied
	// authority. Extras are surfaced as tx_irrelevant_sig.
	//
	// HF28 lifts this: C++ guards BOTH asserts with `if( !allow_redundant_
	// signatures )`, and database.cpp:2647 passes
	// has_hardfork( HIVE_HARDFORK_1_28_ALLOW_REDUNDANT_SIGNATURES ). Enforcing
	// it post-HF28 rejects valid mainnet blocks — hit live at block
	// 108,860,724 tx 2, which wedged the node at the tip. Catch-up never sees
	// this because historical blocks skip signature verification entirely, so
	// it only manifests once strict verify engages near the head.
	if !hf28 {
		for _, k := range keys {
			if !state.usedKeys[k] {
				return ErrIrrelevantSig
			}
		}
	}
	return nil
}

// sessionAuthFetcher resolves authority lookups against a state.Session
// via the AccountAuthority table.
type sessionAuthFetcher struct{ s *state.Session }

func (f *sessionAuthFetcher) OwnerAuthority(name auth.AccountName) (*auth.Authority, error) {
	a, err := objects.GetAccountAuthority(f.s, name)
	if err != nil {
		return nil, err
	}
	o := a.Owner
	return &o, nil
}
func (f *sessionAuthFetcher) ActiveAuthority(name auth.AccountName) (*auth.Authority, error) {
	a, err := objects.GetAccountAuthority(f.s, name)
	if err != nil {
		return nil, err
	}
	o := a.Active
	return &o, nil
}
func (f *sessionAuthFetcher) PostingAuthority(name auth.AccountName) (*auth.Authority, error) {
	a, err := objects.GetAccountAuthority(f.s, name)
	if err != nil {
		return nil, err
	}
	o := a.Posting
	return &o, nil
}

// sortedSet flattens a string-set into a sorted slice. Sorting makes
// errors deterministic for callers and avoids iteration-order-dependent
// behavior in the recursion's `visited` map.
func sortedSet(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Local insertion sort — small N, avoids importing sort just for this.
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}

// OpExcludedFromBlocks reports whether an operation may NEVER appear in a
// block, regardless of how well it is signed.
//
// hived enforces this with a hard assert in the evaluator rather than a
// validation rule (hive_evaluator.cpp:615, witness_block_approve):
//
//	HIVE_CHAIN_UNREACHABLE_CODE_ASSERT( false &&
//	  "This operation may not be included in a block" );
//
// witness_block_approve is a FAST-CONFIRMATION message, routed through
// database::process_fast_confirm_transaction and never through
// push_transaction. It is ~92% of gossiped traffic and appears in ZERO real
// blocks. Admitting it to the block-building mempool is what put 579 of them
// into block 108912883 and got that block rejected by the network.
//
// Kept as a named predicate so the admission path and its test check the SAME
// rule — a test that re-states the rule locally passes even when the
// production check is gone.
func OpExcludedFromBlocks(tag op.Tag) bool {
	return tag == op.TagWitnessBlockApprove
}
