// Package objects defines the state-object types stored in the chainbase
// replacement, with their FC binary codecs and TableID assignments.
//
// Note on scope: hived's account_object has 60+ fields covering balances,
// vesting, recovery, RC, voting power, witness votes, post counts, etc.
// This Go port introduces fields incrementally as evaluators need them.
// On-disk state format is implementation-internal (Hive consensus does
// not commit a state-root) so we are free to choose a leaner schema.
package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// AccountTable is the TableID for account_object state.
const AccountTable state.TableID = 1

// Account is the minimal-viable account state object. Fields are added as
// evaluators need them; the FC codec preserves field order so future
// schema extensions are append-only.
type Account struct {
	Name auth.AccountName
	// Created is the time_point_sec at which this account was created.
	Created uint32
	// Balance is the liquid HIVE balance, in satoshis (1e-3 HIVE).
	Balance int64
	// HBDBalance is the liquid HBD balance, in satoshis (1e-3 HBD).
	HBDBalance int64
	// VestingShares is the total VESTS held (1e-6 VESTS).
	VestingShares int64
	// Pending reward balances (claim_reward_balance moves these into the
	// liquid Balance/HBDBalance/VestingShares fields).
	RewardHive  int64
	RewardHBD   int64
	RewardVests int64
	// MemoKey is the 33-byte compressed secp256k1 memo key.
	MemoKey [33]byte
	// JSONMetadata is the account's published metadata blob.
	JSONMetadata string
	// Proxy is the account_witness_proxy target. Empty string ("" or
	// HIVE_PROXY_TO_SELF_ACCOUNT in C++) means no proxy.
	Proxy auth.AccountName
	// WitnessesVotedFor counts how many witnesses this account has voted
	// for directly. Capped at HIVE_MAX_ACCOUNT_WITNESS_VOTES (30).
	WitnessesVotedFor uint16
	// PendingClaimedAccounts is the count of paid-for-but-not-yet-used
	// account claims (HF20+). claim_account increments;
	// create_claimed_account decrements.
	PendingClaimedAccounts uint16
	// SavingsBalance / SavingsHBDBalance hold funds in the time-locked
	// savings account.
	SavingsBalance    int64
	SavingsHBDBalance int64
	// DelegatedVestingShares are VESTS this account has delegated out
	// to others; ReceivedVestingShares are VESTS delegated in.
	DelegatedVestingShares int64
	ReceivedVestingShares  int64
	// Vesting power-down schedule (withdraw_vesting). Pays
	// HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS apart for
	// HIVE_VESTING_WITHDRAW_INTERVALS iterations.
	VestingWithdrawRate   int64
	NextVestingWithdrawal uint32
	ToWithdraw            int64
	Withdrawn             int64
	// LastPost is the timestamp of the most recent comment_operation
	// (root or reply) by this account; LastRootPost tracks the most
	// recent root post specifically. Used by the comment evaluator to
	// enforce HIVE_MIN_ROOT_COMMENT_INTERVAL (5 min) and
	// HIVE_MIN_REPLY_INTERVAL (3 sec) — see hived
	// hive_evaluator_social.cpp:204.
	LastPost     uint32
	LastRootPost uint32
	// HBDSeconds tracks integral( hbd_balance dt ) since
	// HBDSecondsLastUpdate; on every adjust_balance() it accrues, and on
	// every >= HIVE_HBD_INTEREST_COMPOUND_INTERVAL_SEC tick it is paid
	// out as HBD interest. Stored as a 16-byte big-endian uint128 since
	// hbd_balance × seconds can exceed int64 for large balances over a
	// year. Mirrors account_object::hbd_seconds in
	// libraries/chain/include/hive/chain/account_object.hpp.
	HBDSeconds              [16]byte
	HBDSecondsLastUpdate    uint32
	HBDLastInterestPayment  uint32
	SavingsHBDSeconds       [16]byte
	SavingsHBDSecondsLastUpdate   uint32
	SavingsHBDLastInterestPayment uint32
	// voting_manabar (libraries/chain/util/manabar.hpp +
	// libraries/chain/include/hive/chain/account_object.hpp): each vote
	// depletes current_mana, regenerating linearly back to the cap of
	// HIVE_100_PERCENT (10000 pre-HF20) over
	// HIVE_VOTING_MANA_REGENERATION_SECONDS (5 days). Post-HF20 the cap
	// becomes the account's effective vesting weight; this Go field
	// stores both regimes.
	VotingManaCurrent    int64
	VotingManaLastUpdate uint32
	// downvote_manabar (HF20+) — separate 5-day-regen pool for downvotes.
	// Mirrors account_object::downvote_manabar. Pre-HF21 the cap is
	// max_voting_mana / 4; HF21+ scales with dgpo.downvote_pool_percent.
	// When a downvote runs the pool dry, the remainder pulls from
	// voting_manabar. Pre-HF20 the field is unused (zero).
	DownvoteManaCurrent    int64
	DownvoteManaLastUpdate uint32
	// CanVote — set false by decline_voting_rights effective-date hook.
	// Default true (the FC-decoded zero value would be false; we shift
	// the sense to "VoteRevoked" to keep the zero value harmless).
	VoteRevoked bool
	// GovernanceVoteExpiration — HF22+ governance votes expire after a
	// year of voter inactivity. We mirror the field; the per-block
	// remove_expired_governance_votes hook is not modeled.
	GovernanceVoteExpiration uint32
	// DelayedVotes / SumDelayedVotes — HF24+ vesting-share maturation
	// queue. Newly-acquired VESTS (from transfer_to_vesting) sit in
	// DelayedVotes for HIVE_DELAYED_VOTING_TOTAL_INTERVAL_SECONDS
	// (30 days) before counting toward witness vote weight. Buckets are
	// HIVE_DELAYED_VOTING_INTERVAL_SECONDS (1 day) wide; up to 30
	// buckets at any time.
	//
	// SumDelayedVotes is the redundant sum of all bucket values, kept
	// in sync by add/erase to avoid having to walk the slice every
	// effective-vote-weight read.
	//
	// Mirrors libraries/chain/include/hive/chain/util/
	// delayed_voting_processor.hpp + account_object.hpp.
	DelayedVotes    []DelayedVoteEntry
	SumDelayedVotes uint64
	// Resource Credits manabar (HF20+). Mirrors hived's
	// account_object::rc_manabar + last_max_rc fields.
	//
	//	max_rc      = effective_vesting_shares (post HF22+ adjusted by
	//	              direct RC delegations in/out)
	//	regen_time  = HIVE_RC_REGEN_TIME = 5 days
	//	current_mana regenerates linearly to max_mana over regen_time.
	//
	// LastMaxRC is the max_rc value at the most recent
	// regenerate_rc_mana call — used as a sanity check that every op
	// that mutates vesting also bumps the manabar.
	RCManaCurrent    int64
	RCManaLastUpdate uint32
	LastMaxRC        int64
	// LastOwnerUpdate / PreviousOwnerUpdate (post-HF, but cheap to
	// always track). Mirrors account_authority_object::last_owner_update
	// and previous_owner_update. Set whenever account_update changes
	// the owner authority. Used by request_account_recovery to gate
	// the HIVE_OWNER_UPDATE_LIMIT (60 minutes between owner changes).
	LastOwnerUpdate     uint32
	PreviousOwnerUpdate uint32
	// DelegatedRCIn / DelegatedRCOut — HF22+ direct RC delegations.
	// MaximumRC = effective_vesting + DelegatedRCIn - DelegatedRCOut.
	// Mirrors account_object's tracking fields used by
	// rc_direct_delegation_object aggregations.
	DelegatedRCIn  int64
	DelegatedRCOut int64
	// ProxiedVSFVotes — HIVE_MAX_PROXY_RECURSION_DEPTH+1 = 5 slots of
	// vesting weight proxied THROUGH this account at depth i. When an
	// account A proxies to B, A's direct vest goes into B.ProxiedVSFVotes[0],
	// A.ProxiedVSFVotes[i] cascades into B.ProxiedVSFVotes[i+1], etc.
	// Used by adjust_proxied_witness_votes and nullify_proxied_witness_votes
	// (database.cpp:1077-1138). Pre-HF24 the direct portion (delta[0]) is
	// vesting_shares; HF24+ it's vesting_shares - sum_delayed_votes.
	ProxiedVSFVotes [5]int64
	// API-tracking fields (no consensus impact). Mirror hived's
	// account_object equivalents so chain-history APIs can be served
	// without reconstructing them from operation history.
	//
	//   LastVoteTime              — timestamp of most recent vote_operation
	//   PostCount                 — number of comments authored
	//   PostBandwidth             — recent post bandwidth (used pre-HF20 for
	//                               post-rate limiting; cosmetic post-HF20)
	//   Mined                     — true if account was created via pow op
	//   LastAccountUpdate         — timestamp of most recent account_update*
	//   BlockLastAccountRecovery  — block number of last successful recovery
	LastVoteTime             uint32
	PostCount                uint32
	PostBandwidth            uint32
	Mined                    bool
	LastAccountUpdate        uint32
	BlockLastAccountRecovery uint32
	// RewardVestingHive mirrors hived's account_object::reward_vesting_hive
	// (vest_rewards_as_hive). HF17+ comment payouts to the reward pool
	// credit (RewardVests, RewardVestingHive) in lockstep; on
	// claim_reward_balance, the matching HIVE-backing share moves from
	// dgpo.pending_rewarded_vesting_* into dgpo.total_vesting_* so the
	// vesting share price stays correct.
	RewardVestingHive int64
}

// CanVote returns whether the account is allowed to cast votes
// (negation of the on-disk VoteRevoked flag, which has zero value =
// false = "still allowed", matching new accounts which can vote).
func (a *Account) CanVote() bool { return !a.VoteRevoked }

// AppendAccount serializes a to dst using FC binary form. The field
// order chosen here is internal to the state schema — it does not need
// to match hived's account_object FC_REFLECT — but it MUST be preserved
// across releases of the schema.
func AppendAccount(dst []byte, a *Account) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(a.Name)))
	dst = append(dst, a.Name...)
	dst = appendU32LE(dst, a.Created)
	dst = appendI64LE(dst, a.Balance)
	dst = appendI64LE(dst, a.HBDBalance)
	dst = appendI64LE(dst, a.VestingShares)
	dst = appendI64LE(dst, a.RewardHive)
	dst = appendI64LE(dst, a.RewardHBD)
	dst = appendI64LE(dst, a.RewardVests)
	dst = append(dst, a.MemoKey[:]...)
	dst = fcraw.AppendVarUint(dst, uint32(len(a.JSONMetadata)))
	dst = append(dst, a.JSONMetadata...)
	dst = fcraw.AppendVarUint(dst, uint32(len(a.Proxy)))
	dst = append(dst, a.Proxy...)
	dst = append(dst, byte(a.WitnessesVotedFor), byte(a.WitnessesVotedFor>>8))
	dst = append(dst, byte(a.PendingClaimedAccounts), byte(a.PendingClaimedAccounts>>8))
	dst = appendI64LE(dst, a.SavingsBalance)
	dst = appendI64LE(dst, a.SavingsHBDBalance)
	dst = appendI64LE(dst, a.DelegatedVestingShares)
	dst = appendI64LE(dst, a.ReceivedVestingShares)
	dst = appendI64LE(dst, a.VestingWithdrawRate)
	dst = appendU32LE(dst, a.NextVestingWithdrawal)
	dst = appendI64LE(dst, a.ToWithdraw)
	dst = appendI64LE(dst, a.Withdrawn)
	dst = appendU32LE(dst, a.LastPost)
	dst = appendU32LE(dst, a.LastRootPost)
	dst = append(dst, a.HBDSeconds[:]...)
	dst = appendU32LE(dst, a.HBDSecondsLastUpdate)
	dst = appendU32LE(dst, a.HBDLastInterestPayment)
	dst = append(dst, a.SavingsHBDSeconds[:]...)
	dst = appendU32LE(dst, a.SavingsHBDSecondsLastUpdate)
	dst = appendU32LE(dst, a.SavingsHBDLastInterestPayment)
	dst = appendI64LE(dst, a.VotingManaCurrent)
	dst = appendU32LE(dst, a.VotingManaLastUpdate)
	if a.VoteRevoked {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	dst = appendU32LE(dst, a.GovernanceVoteExpiration)
	dst = fcraw.AppendVarUint(dst, uint32(len(a.DelayedVotes)))
	for _, e := range a.DelayedVotes {
		dst = appendU32LE(dst, e.Time)
		dst = appendU64LE(dst, e.Val)
	}
	dst = appendU64LE(dst, a.SumDelayedVotes)
	dst = appendI64LE(dst, a.RCManaCurrent)
	dst = appendU32LE(dst, a.RCManaLastUpdate)
	dst = appendI64LE(dst, a.LastMaxRC)
	dst = appendU32LE(dst, a.LastOwnerUpdate)
	dst = appendU32LE(dst, a.PreviousOwnerUpdate)
	dst = appendI64LE(dst, a.DelegatedRCIn)
	dst = appendI64LE(dst, a.DelegatedRCOut)
	for _, v := range a.ProxiedVSFVotes {
		dst = appendI64LE(dst, v)
	}
	dst = appendI64LE(dst, a.DownvoteManaCurrent)
	dst = appendU32LE(dst, a.DownvoteManaLastUpdate)
	dst = appendU32LE(dst, a.LastVoteTime)
	dst = appendU32LE(dst, a.PostCount)
	dst = appendU32LE(dst, a.PostBandwidth)
	if a.Mined {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	dst = appendU32LE(dst, a.LastAccountUpdate)
	dst = appendU32LE(dst, a.BlockLastAccountRecovery)
	dst = appendI64LE(dst, a.RewardVestingHive)
	return dst
}

// ReadAccount decodes an account from src.
func ReadAccount(src []byte) (Account, error) {
	r := fcraw.NewReader(src)
	var a Account
	name, err := r.ReadString()
	if err != nil {
		return a, fmt.Errorf("account.name: %w", err)
	}
	a.Name = name
	if a.Created, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.created: %w", err)
	}
	if a.Balance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.balance: %w", err)
	}
	if a.HBDBalance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.hbd_balance: %w", err)
	}
	if a.VestingShares, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.vesting_shares: %w", err)
	}
	if a.RewardHive, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.reward_hive: %w", err)
	}
	if a.RewardHBD, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.reward_hbd: %w", err)
	}
	if a.RewardVests, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.reward_vests: %w", err)
	}
	if err := r.ReadInto(a.MemoKey[:]); err != nil {
		return a, fmt.Errorf("account.memo_key: %w", err)
	}
	if a.JSONMetadata, err = r.ReadString(); err != nil {
		return a, fmt.Errorf("account.json_metadata: %w", err)
	}
	if a.Proxy, err = r.ReadString(); err != nil {
		return a, fmt.Errorf("account.proxy: %w", err)
	}
	if a.WitnessesVotedFor, err = r.ReadUint16(); err != nil {
		return a, fmt.Errorf("account.witnesses_voted_for: %w", err)
	}
	if a.PendingClaimedAccounts, err = r.ReadUint16(); err != nil {
		return a, fmt.Errorf("account.pending_claimed_accounts: %w", err)
	}
	if a.SavingsBalance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.savings_balance: %w", err)
	}
	if a.SavingsHBDBalance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.savings_hbd_balance: %w", err)
	}
	if a.DelegatedVestingShares, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.delegated_vesting_shares: %w", err)
	}
	if a.ReceivedVestingShares, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.received_vesting_shares: %w", err)
	}
	if a.VestingWithdrawRate, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.vesting_withdraw_rate: %w", err)
	}
	if a.NextVestingWithdrawal, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.next_vesting_withdrawal: %w", err)
	}
	if a.ToWithdraw, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.to_withdraw: %w", err)
	}
	if a.Withdrawn, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.withdrawn: %w", err)
	}
	// LastPost / LastRootPost are a later schema addition. Treat EOF as
	// zero so the on-disk format stays append-only across releases.
	if r.AtEnd() {
		return a, nil
	}
	if a.LastPost, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.last_post: %w", err)
	}
	if a.LastRootPost, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.last_root_post: %w", err)
	}
	// HBD interest tracking is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	if err := r.ReadInto(a.HBDSeconds[:]); err != nil {
		return a, fmt.Errorf("account.hbd_seconds: %w", err)
	}
	if a.HBDSecondsLastUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.hbd_seconds_last_update: %w", err)
	}
	if a.HBDLastInterestPayment, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.hbd_last_interest_payment: %w", err)
	}
	if err := r.ReadInto(a.SavingsHBDSeconds[:]); err != nil {
		return a, fmt.Errorf("account.savings_hbd_seconds: %w", err)
	}
	if a.SavingsHBDSecondsLastUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.savings_hbd_seconds_last_update: %w", err)
	}
	if a.SavingsHBDLastInterestPayment, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.savings_hbd_last_interest_payment: %w", err)
	}
	// voting_manabar fields are a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	if a.VotingManaCurrent, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.voting_mana_current: %w", err)
	}
	if a.VotingManaLastUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.voting_mana_last_update: %w", err)
	}
	// VoteRevoked + GovernanceVoteExpiration are a later schema addition;
	// tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	if a.VoteRevoked, err = r.ReadBool(); err != nil {
		return a, fmt.Errorf("account.vote_revoked: %w", err)
	}
	if a.GovernanceVoteExpiration, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.governance_vote_expiration: %w", err)
	}
	// DelayedVotes / SumDelayedVotes are a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return a, fmt.Errorf("account.delayed_votes.count: %w", err)
	}
	if count > 0 {
		a.DelayedVotes = make([]DelayedVoteEntry, count)
		for i := uint32(0); i < count; i++ {
			if a.DelayedVotes[i].Time, err = r.ReadUint32(); err != nil {
				return a, fmt.Errorf("account.delayed_votes[%d].time: %w", i, err)
			}
			if a.DelayedVotes[i].Val, err = r.ReadUint64(); err != nil {
				return a, fmt.Errorf("account.delayed_votes[%d].val: %w", i, err)
			}
		}
	}
	if a.SumDelayedVotes, err = r.ReadUint64(); err != nil {
		return a, fmt.Errorf("account.sum_delayed_votes: %w", err)
	}
	// RC manabar fields are a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	if a.RCManaCurrent, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.rc_mana_current: %w", err)
	}
	if a.RCManaLastUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.rc_mana_last_update: %w", err)
	}
	if a.LastMaxRC, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.last_max_rc: %w", err)
	}
	// LastOwnerUpdate / PreviousOwnerUpdate / DelegatedRC* are a
	// later schema addition; tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	if a.LastOwnerUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.last_owner_update: %w", err)
	}
	if a.PreviousOwnerUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.previous_owner_update: %w", err)
	}
	if a.DelegatedRCIn, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.delegated_rc_in: %w", err)
	}
	if a.DelegatedRCOut, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.delegated_rc_out: %w", err)
	}
	// ProxiedVSFVotes is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	for i := 0; i < 5; i++ {
		if a.ProxiedVSFVotes[i], err = r.ReadInt64(); err != nil {
			return a, fmt.Errorf("account.proxied_vsf_votes[%d]: %w", i, err)
		}
	}
	// downvote_manabar + API-tracking fields are a later schema addition;
	// tolerate EOF.
	if r.AtEnd() {
		return a, nil
	}
	if a.DownvoteManaCurrent, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.downvote_mana_current: %w", err)
	}
	if a.DownvoteManaLastUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.downvote_mana_last_update: %w", err)
	}
	if a.LastVoteTime, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.last_vote_time: %w", err)
	}
	if a.PostCount, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.post_count: %w", err)
	}
	if a.PostBandwidth, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.post_bandwidth: %w", err)
	}
	mb, err := r.ReadByte()
	if err != nil {
		return a, fmt.Errorf("account.mined: %w", err)
	}
	a.Mined = mb != 0
	if a.LastAccountUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.last_account_update: %w", err)
	}
	if a.BlockLastAccountRecovery, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.block_last_account_recovery: %w", err)
	}
	// RewardVestingHive is a later schema addition; tolerate EOF for legacy rows.
	if r.AtEnd() {
		return a, nil
	}
	if a.RewardVestingHive, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.reward_vesting_hive: %w", err)
	}
	if !r.AtEnd() {
		return a, errors.New("account: trailing bytes after decode (schema mismatch?)")
	}
	return a, nil
}

// AccountKey returns the primary-index key bytes for an account named name.
// Account names are NUL-padded to 16 bytes (AccountNameKeyLen) so byte-wise
// prefix iteration produces alphabetic order.
func AccountKey(name auth.AccountName) []byte {
	return state.AppendAccountName(nil, name)
}

// PutAccount stores a in the session's typed AccountStore.
func PutAccount(s *state.Session, a *Account) {
	EnsureAccountStore(s).Put(a)
}

// GetAccount fetches the account named name. Returns ErrNotFound if absent.
func GetAccount(s *state.Session, name auth.AccountName) (Account, error) {
	a, ok := EnsureAccountStore(s).Get(name)
	if !ok {
		return Account{}, state.ErrNotFound
	}
	return a, nil
}

// GetAccountRef returns a *read-only* pointer to the account named
// `name` (or nil if absent). Aliases internal storage — callers must
// not mutate the returned struct and must not retain the pointer
// past the next Delete-on-this-name (which is currently impossible
// for accounts in mainnet, but the rule keeps future safety). Skips
// the ~hundred-byte Account value-copy that GetAccount pays. Use on
// read-only paths only.
func GetAccountRef(s *state.Session, name auth.AccountName) (*Account, bool) {
	return EnsureAccountStore(s).GetRef(name)
}

// MutateAccount applies an in-place modification to the account slot
// for `name` via AccountStore.Mutate. Returns false if the row
// doesn't exist. Saves the Account value-copy on read + write that
// the Get+modify+Put pattern pays.
func MutateAccount(s *state.Session, name auth.AccountName, fn func(*Account)) bool {
	return EnsureAccountStore(s).Mutate(name, fn)
}

// HasAccount reports whether an account named name exists.
func HasAccount(s *state.Session, name auth.AccountName) (bool, error) {
	return EnsureAccountStore(s).Has(name), nil
}

// DeleteAccount removes an account row.
func DeleteAccount(s *state.Session, name auth.AccountName) {
	EnsureAccountStore(s).Delete(name)
}

// RangeAccounts iterates every account in name-ascending order.
func RangeAccounts(s *state.Session, fn func(a *Account) (bool, error)) error {
	return EnsureAccountStore(s).RangePrimary(fn)
}
