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
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/op"
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
func Extract(t *TouchSet, o op.Operation) {
	switch v := o.(type) {
	// ---- account-mutating ops ----
	case op.Transfer:
		t.Accounts.Add(v.From)
		t.Accounts.Add(v.To)
		t.FundsTouched = true // current_supply tracked when treasury hops fire
	case op.TransferToVesting:
		t.Accounts.Add(v.From)
		t.Accounts.Add(v.To)
		t.FundsTouched = true // total_vesting_fund_hive + total_vesting_shares
	case op.WithdrawVesting:
		t.Accounts.Add(v.Account)
		t.FundsTouched = true
	case op.TransferToSavings:
		t.Accounts.Add(v.From)
		t.Accounts.Add(v.To)
		t.FundsTouched = true
	case op.TransferFromSavings:
		t.Accounts.Add(v.From)
		t.Accounts.Add(v.To)
		t.FundsTouched = true
	case op.CancelTransferFromSavings:
		t.Accounts.Add(v.From)
		t.FundsTouched = true
	case op.ClaimRewardBalance:
		t.Accounts.Add(v.Account)
		t.FundsTouched = true
	case op.AccountCreate:
		t.Accounts.Add(v.Creator)
		t.Accounts.Add(v.NewAccountName)
		t.FundsTouched = true // creator pays the fee
	case op.AccountUpdate:
		t.Accounts.Add(v.Account)
	case op.SetWithdrawVestingRoute:
		t.Accounts.Add(v.FromAccount)
		t.Accounts.Add(v.ToAccount)
	case op.DeclineVotingRights:
		t.Accounts.Add(v.Account)
	case op.RequestAccountRecovery:
		t.Accounts.Add(v.AccountToRecover)
		t.Accounts.Add(v.RecoveryAccount)
	case op.RecoverAccount:
		t.Accounts.Add(v.AccountToRecover)
	case op.ChangeRecoveryAccount:
		t.Accounts.Add(v.AccountToRecover)
		t.Accounts.Add(v.NewRecoveryAccount)
	case op.ResetAccount:
		t.Accounts.Add(v.AccountToReset)
	case op.ClaimAccount:
		t.Accounts.Add(v.Creator)
		t.FundsTouched = true
	// ---- witness-mutating ops ----
	case op.WitnessUpdate:
		t.Witnesses.Add(v.Owner)
	case op.WitnessSetProperties:
		t.Witnesses.Add(v.Owner)
	case op.AccountWitnessVote:
		// Voter's proxy state + the witness's vote count. The HF2 path
		// of applyWitnessVoteDelta calls AdjustProxiedWitnessVotes
		// which mutates EVERY witness the voter currently votes for —
		// not visible from the op fields alone. Mark FundsTouched so
		// AWV serializes with any other op that touches vest /
		// witness-vote propagation state. Loses parallelism for AWV
		// (~rare on mainnet) but keeps correctness sound.
		t.Accounts.Add(v.Account)
		t.Witnesses.Add(v.Witness)
		t.FundsTouched = true
	case op.AccountWitnessProxy:
		// Setting/clearing proxy walks the proxy chain and adjusts
		// every witness vested-vote total at each level. The full
		// touch set is determined by current state — mark
		// FundsTouched conservatively.
		t.Accounts.Add(v.Account)
		t.Witnesses.Add(v.Proxy)
		t.FundsTouched = true
	case op.FeedPublish:
		t.Witnesses.Add(v.Publisher)
	// ---- market ----
	// Limit-order ops share the order book — a create by Alice may
	// match against Bob's existing sell-side order, mutating both
	// accounts' balances. The shared book isn't captured by the
	// (Owner, OrderID) op fields alone, so all market ops are
	// FundsTouched (serialize with each other + with anything else
	// that mutates DGPO / balances).
	case op.LimitOrderCreate:
		t.Accounts.Add(v.Owner)
		t.FundsTouched = true
	case op.LimitOrderCreate2:
		t.Accounts.Add(v.Owner)
		t.FundsTouched = true
	case op.LimitOrderCancel:
		t.Accounts.Add(v.Owner)
		t.FundsTouched = true
	// ---- escrow ----
	case op.EscrowApprove:
		t.Accounts.Add(v.From)
		t.Accounts.Add(v.To)
		t.Accounts.Add(v.Agent)
		t.Accounts.Add(v.Who)
	case op.EscrowDispute:
		t.Accounts.Add(v.From)
		t.Accounts.Add(v.To)
		t.Accounts.Add(v.Agent)
		t.Accounts.Add(v.Who)
	// ---- conversions ----
	case op.Convert:
		t.Accounts.Add(v.Owner)
		t.FundsTouched = true
	// ---- comment/vote ----
	case op.Vote:
		t.Accounts.Add(v.Voter)
		t.CommentTouched = true
	case op.Comment:
		t.Accounts.Add(v.Author)
		t.CommentTouched = true
	case op.CommentOptions:
		t.Accounts.Add(v.Author)
		t.CommentTouched = true
	case op.DeleteComment:
		t.Accounts.Add(v.Author)
		t.CommentTouched = true
	// ---- recurrent transfer ----
	case op.RecurrentTransfer:
		t.Accounts.Add(v.From)
		t.Accounts.Add(v.To)
		t.FundsTouched = true
	// ---- pow (pre-HF13 only) ----
	case op.Pow:
		t.Witnesses.Add(v.WorkerAccount)
		t.FundsTouched = true
	case op.Pow2:
		// Touched witness extractable from work; conservative widen.
		t.FundsTouched = true
	// ---- custom_json / custom: opaque payloads, conservative widen ----
	case op.CustomJSON:
		// Custom_json can reach into arbitrary state via the rc_custom
		// plugin etc. Mark everything to force serial apply.
		t.CommentTouched = true
		t.FundsTouched = true
		for _, name := range v.RequiredPostingAuths {
			t.Accounts.Add(name)
		}
		for _, name := range v.RequiredAuths {
			t.Accounts.Add(name)
		}
	// Unknown / not-yet-classified op kinds: assume the worst.
	default:
		t.CommentTouched = true
		t.FundsTouched = true
	}
}
