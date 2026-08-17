// Package parallel is the scaffolding for tx-level intra-block
// parallelism in ApplyBlock. The goal: within a single block, identify
// transactions whose write sets are disjoint and apply them
// concurrently across goroutines.
//
// Status: scaffolding only. Touch-set extraction per op is implemented
// for the regular op set; the parallel-apply driver is NOT yet wired
// into chain.Database.ApplyBlock — it requires the deeper refactor
// documented in design.md (thread-safe state stores). The package
// exists so the touch-set extraction lives in one place and can be
// validated independently.
package parallel

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
)

// AccountSet is a tiny inline-array-based set of account names. Most
// txs touch ≤ 8 distinct accounts; the inline arrays avoid the
// per-extraction map allocation that a real map-based set would pay.
type AccountSet struct {
	names    [8]auth.AccountName
	n        int
	overflow []auth.AccountName // populated only when names is full
}

// Add inserts name if not already present.
func (s *AccountSet) Add(name auth.AccountName) {
	if name == "" {
		return
	}
	// Linear scan inline first.
	for i := 0; i < s.n; i++ {
		if s.names[i] == name {
			return
		}
	}
	if s.n < len(s.names) {
		s.names[s.n] = name
		s.n++
		return
	}
	// Overflow path: scan overflow + append.
	for _, x := range s.overflow {
		if x == name {
			return
		}
	}
	s.overflow = append(s.overflow, name)
}

// Reset clears the set for reuse.
func (s *AccountSet) Reset() {
	s.n = 0
	s.overflow = s.overflow[:0]
}

// ForEach iterates the accounts in insertion order. Returning false
// stops the iteration.
func (s *AccountSet) ForEach(fn func(name auth.AccountName) bool) {
	for i := 0; i < s.n; i++ {
		if !fn(s.names[i]) {
			return
		}
	}
	for _, x := range s.overflow {
		if !fn(x) {
			return
		}
	}
}

// Len returns the number of distinct accounts.
func (s *AccountSet) Len() int { return s.n + len(s.overflow) }

// Intersects reports whether s and other share at least one account.
func (s *AccountSet) Intersects(other *AccountSet) bool {
	// Walk the smaller side to keep this near-linear in the smaller
	// set's size.
	if s.Len() > other.Len() {
		s, other = other, s
	}
	intersects := false
	s.ForEach(func(name auth.AccountName) bool {
		if other.Contains(name) {
			intersects = true
			return false
		}
		return true
	})
	return intersects
}

// Contains reports membership.
func (s *AccountSet) Contains(name auth.AccountName) bool {
	for i := 0; i < s.n; i++ {
		if s.names[i] == name {
			return true
		}
	}
	for _, x := range s.overflow {
		if x == name {
			return true
		}
	}
	return false
}

// TouchSet is the per-tx footprint used by the conflict-detector.
// Currently only the write side is tracked; reads are not yet
// surfaced because mainnet's hot-path conflicts are all write-vs-write
// (two transfers from the same account, two votes on the same comment,
// two account_update ops on the same account). Reads-only conflicts
// (one tx reads what another writes) are rare and can be added later
// if profiling shows they matter.
type TouchSet struct {
	// Accounts whose persistent state this tx mutates (balance, vesting,
	// auth, proxy, etc.). Two txs whose Accounts sets overlap CANNOT be
	// applied concurrently.
	Accounts AccountSet
	// Witnesses this tx directly mutates (witness_update,
	// witness_set_properties, account_witness_vote, etc.). Note that
	// AdjustWitnessVotesForVoter — triggered by every CreateVesting —
	// also mutates witnesses; downstream code may need to widen the
	// witness set to "all witnesses the voter votes for" if vest-
	// crediting ops parallelize.
	Witnesses AccountSet
	// CommentTouched is set when this tx mutates ANY comment or vote
	// (vote, comment, comment_options, delete_comment). Conflict
	// detection treats this as a global "comments lock" until the
	// underlying comment_id can be cheaply identified at parse time.
	CommentTouched bool
	// FundsTouched is set when this tx mutates DGPO supply totals or
	// the reward funds (transfer to vesting, conversions, withdraw
	// vesting, etc.). Conservatively treated as a global "dgpo lock".
	FundsTouched bool
}

// Reset clears the touch set for reuse.
func (t *TouchSet) Reset() {
	t.Accounts.Reset()
	t.Witnesses.Reset()
	t.CommentTouched = false
	t.FundsTouched = false
}

// ConflictsWith reports whether two TouchSets cannot run in parallel.
func (t *TouchSet) ConflictsWith(o *TouchSet) bool {
	if t.CommentTouched && o.CommentTouched {
		return true
	}
	if t.FundsTouched && o.FundsTouched {
		return true
	}
	if t.Accounts.Intersects(&o.Accounts) {
		return true
	}
	if t.Witnesses.Intersects(&o.Witnesses) {
		return true
	}
	return false
}

// Extract populates t from a single operation. Conservative: when
// unsure whether a field maps to an Account / Witness / Comment /
// Funds touch, opt into the larger set. The detector then errs on
// the side of serializing — never falsely permits a parallel apply.
func Extract(t *TouchSet, o op.Op) {
	switch o.Tag {
	// ---- account-mutating ops ----
	case op.TagTransfer:
		t.Accounts.Add(o.AsTransfer().From)
		t.Accounts.Add(o.AsTransfer().To)
		t.FundsTouched = true // current_supply tracked when treasury hops fire
	case op.TagTransferToVesting:
		t.Accounts.Add(o.AsTransferToVesting().From)
		t.Accounts.Add(o.AsTransferToVesting().To)
		t.FundsTouched = true // total_vesting_fund_hive + total_vesting_shares
	case op.TagWithdrawVesting:
		t.Accounts.Add(o.AsWithdrawVesting().Account)
		t.FundsTouched = true
	case op.TagTransferToSavings:
		t.Accounts.Add(o.AsTransferToSavings().From)
		t.Accounts.Add(o.AsTransferToSavings().To)
		t.FundsTouched = true
	case op.TagTransferFromSavings:
		t.Accounts.Add(o.AsTransferFromSavings().From)
		t.Accounts.Add(o.AsTransferFromSavings().To)
		t.FundsTouched = true
	case op.TagCancelTransferFromSavings:
		t.Accounts.Add(o.AsCancelTransferFromSavings().From)
		t.FundsTouched = true
	case op.TagClaimRewardBalance:
		t.Accounts.Add(o.AsClaimRewardBalance().Account)
		t.FundsTouched = true
	case op.TagAccountCreate:
		t.Accounts.Add(o.AsAccountCreate().Creator)
		t.Accounts.Add(o.AsAccountCreate().NewAccountName)
		t.FundsTouched = true // creator pays the fee
	case op.TagAccountUpdate:
		t.Accounts.Add(o.AsAccountUpdate().Account)
	case op.TagSetWithdrawVestingRoute:
		t.Accounts.Add(o.AsSetWithdrawVestingRoute().FromAccount)
		t.Accounts.Add(o.AsSetWithdrawVestingRoute().ToAccount)
	case op.TagDeclineVotingRights:
		t.Accounts.Add(o.AsDeclineVotingRights().Account)
	case op.TagRequestAccountRecovery:
		t.Accounts.Add(o.AsRequestAccountRecovery().AccountToRecover)
		t.Accounts.Add(o.AsRequestAccountRecovery().RecoveryAccount)
	case op.TagRecoverAccount:
		t.Accounts.Add(o.AsRecoverAccount().AccountToRecover)
	case op.TagChangeRecoveryAccount:
		t.Accounts.Add(o.AsChangeRecoveryAccount().AccountToRecover)
		t.Accounts.Add(o.AsChangeRecoveryAccount().NewRecoveryAccount)
	case op.TagResetAccount:
		t.Accounts.Add(o.AsResetAccount().AccountToReset)
	case op.TagClaimAccount:
		t.Accounts.Add(o.AsClaimAccount().Creator)
		t.FundsTouched = true
	// ---- witness-mutating ops ----
	case op.TagWitnessUpdate:
		t.Witnesses.Add(o.AsWitnessUpdate().Owner)
	case op.TagWitnessSetProperties:
		t.Witnesses.Add(o.AsWitnessSetProperties().Owner)
	case op.TagAccountWitnessVote:
		// Voter's proxy state + the witness's vote count. The HF2 path
		// of applyWitnessVoteDelta calls AdjustProxiedWitnessVotes
		// which mutates EVERY witness the voter currently votes for —
		// not visible from the op fields alone. Mark FundsTouched so
		// AWV serializes with any other op that touches vest /
		// witness-vote propagation state. Loses parallelism for AWV
		// (~rare on mainnet) but keeps correctness sound.
		t.Accounts.Add(o.AsAccountWitnessVote().Account)
		t.Witnesses.Add(o.AsAccountWitnessVote().Witness)
		t.FundsTouched = true
	case op.TagAccountWitnessProxy:
		// Setting/clearing proxy walks the proxy chain and adjusts
		// every witness vested-vote total at each level. The full
		// touch set is determined by current state — mark
		// FundsTouched conservatively.
		t.Accounts.Add(o.AsAccountWitnessProxy().Account)
		t.Witnesses.Add(o.AsAccountWitnessProxy().Proxy)
		t.FundsTouched = true
	case op.TagFeedPublish:
		t.Witnesses.Add(o.AsFeedPublish().Publisher)
	// ---- market ----
	// Limit-order ops share the order book — a create by Alice may
	// match against Bob's existing sell-side order, mutating both
	// accounts' balances. The shared book isn't captured by the
	// (Owner, OrderID) op fields alone, so all market ops are
	// FundsTouched (serialize with each other + with anything else
	// that mutates DGPO / balances).
	case op.TagLimitOrderCreate:
		t.Accounts.Add(o.AsLimitOrderCreate().Owner)
		t.FundsTouched = true
	case op.TagLimitOrderCreate2:
		t.Accounts.Add(o.AsLimitOrderCreate2().Owner)
		t.FundsTouched = true
	case op.TagLimitOrderCancel:
		t.Accounts.Add(o.AsLimitOrderCancel().Owner)
		t.FundsTouched = true
	// ---- escrow ----
	case op.TagEscrowApprove:
		t.Accounts.Add(o.AsEscrowApprove().From)
		t.Accounts.Add(o.AsEscrowApprove().To)
		t.Accounts.Add(o.AsEscrowApprove().Agent)
		t.Accounts.Add(o.AsEscrowApprove().Who)
	case op.TagEscrowDispute:
		t.Accounts.Add(o.AsEscrowDispute().From)
		t.Accounts.Add(o.AsEscrowDispute().To)
		t.Accounts.Add(o.AsEscrowDispute().Agent)
		t.Accounts.Add(o.AsEscrowDispute().Who)
	// ---- conversions ----
	case op.TagConvert:
		t.Accounts.Add(o.AsConvert().Owner)
		t.FundsTouched = true
	// ---- comment/vote ----
	case op.TagVote:
		t.Accounts.Add(o.AsVote().Voter)
		t.CommentTouched = true
	case op.TagComment:
		t.Accounts.Add(o.AsComment().Author)
		t.CommentTouched = true
	case op.TagCommentOptions:
		t.Accounts.Add(o.AsCommentOptions().Author)
		t.CommentTouched = true
	case op.TagDeleteComment:
		t.Accounts.Add(o.AsDeleteComment().Author)
		t.CommentTouched = true
	// ---- recurrent transfer ----
	case op.TagRecurrentTransfer:
		t.Accounts.Add(o.AsRecurrentTransfer().From)
		t.Accounts.Add(o.AsRecurrentTransfer().To)
		t.FundsTouched = true
	// ---- pow (pre-HF13 only) ----
	case op.TagPow:
		t.Witnesses.Add(o.AsPow().WorkerAccount)
		t.FundsTouched = true
	case op.TagPow2:
		// Touched witness extractable from work; conservative widen.
		t.FundsTouched = true
	// ---- custom_json / custom: opaque payloads, conservative widen ----
	case op.TagCustomJSON:
		// Custom_json can reach into arbitrary state via the rc_custom
		// plugin etc. Mark everything to force serial apply.
		t.CommentTouched = true
		t.FundsTouched = true
		for _, name := range o.AsCustomJSON().RequiredPostingAuths {
			t.Accounts.Add(name)
		}
		for _, name := range o.AsCustomJSON().RequiredAuths {
			t.Accounts.Add(name)
		}
	// Unknown / not-yet-classified op kinds: assume the worst.
	default:
		t.CommentTouched = true
		t.FundsTouched = true
	}
}
