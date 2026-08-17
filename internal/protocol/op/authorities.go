// Authority requirement resolution. Each operation declares which
// account_name + authority level (owner / active / posting) it requires
// to authorize the op. Mirrors the C++ get_required_*_authorities
// methods on each operation_struct (libraries/protocol/include/hive/
// protocol/hive_operations.hpp).
//
// Used during transaction signing and during signature verification:
// the union across all ops in the tx is the set of authorities whose
// keys must validate at least one of the tx's signatures.

package op

import "gopherd/internal/protocol/auth"

// AuthorityLevel mirrors hive::protocol::authority::classification.
// Note that posting authorities can NOT mix with active or owner in a
// single transaction (pre-HF28); this is enforced at signing time, not
// here.
type AuthorityLevel int

const (
	AuthOwner AuthorityLevel = iota
	AuthActive
	AuthPosting
)

func (l AuthorityLevel) String() string {
	switch l {
	case AuthOwner:
		return "owner"
	case AuthActive:
		return "active"
	case AuthPosting:
		return "posting"
	default:
		return "?"
	}
}

// AuthorityRequirement is one (account, level) pair declared by an
// operation as required for validation.
type AuthorityRequirement struct {
	Account auth.AccountName
	Level   AuthorityLevel
}

// AuthorityRequirer is implemented by ops that contribute to the
// required-authorities set.
type AuthorityRequirer interface {
	// RequiredAuthorities appends each (account, level) pair this op
	// requires. Existing entries in `dst` are preserved; the caller
	// dedupes if needed.
	RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement
}

// CollectRequiredAuthorities walks an operation list and returns the
// union of all required (account, level) pairs. Ops that don't
// implement AuthorityRequirer are ignored — they're either deprecated
// or have no auth requirement (e.g. virtual ops).
func CollectRequiredAuthorities(ops []Operation) []AuthorityRequirement {
	var out []AuthorityRequirement
	for _, op := range ops {
		if r, ok := op.(AuthorityRequirer); ok {
			out = r.RequiredAuthorities(out)
		}
	}
	return dedupAuthorityRequirements(out)
}

func dedupAuthorityRequirements(in []AuthorityRequirement) []AuthorityRequirement {
	seen := make(map[AuthorityRequirement]struct{}, len(in))
	out := in[:0]
	for _, r := range in {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}

// --- Per-op declarations -----------------------------------------------

func (o Transfer) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.From, Level: AuthActive})
}

func (o TransferToVesting) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.From, Level: AuthActive})
}

func (o AccountCreate) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Creator, Level: AuthActive})
}

func (o AccountUpdate) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	// Owner-or-active depending on whether owner is being updated;
	// hived's get_required_active_authorities adds active when posting/
	// memo/json-metadata are touched, owner via get_required_owner_authorities
	// when owner is touched. We conservatively require active and let
	// the signing stack add owner if needed. (If owner is being changed,
	// the C++ implementation requires owner authority.)
	return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthActive})
}

func (o Vote) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Voter, Level: AuthPosting})
}

func (o Comment) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Author, Level: AuthPosting})
}

func (o CommentOptions) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Author, Level: AuthPosting})
}

func (o DeleteComment) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Author, Level: AuthPosting})
}

func (o AccountWitnessVote) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthActive})
}

func (o AccountWitnessProxy) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthActive})
}

func (o WitnessUpdate) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Owner, Level: AuthActive})
}

func (o WitnessSetProperties) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	// witness_set_properties is special: the "key" prop self-attests as
	// the witness's signing key. Signature comes from the prop, not
	// from any account_authority. Returns no requirements — the signing
	// stack handles it via the prop's key.
	return dst
}

func (o DeclineVotingRights) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	// Owner authority required (irreversibility).
	return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthOwner})
}

func (o LimitOrderCreate) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Owner, Level: AuthActive})
}

func (o LimitOrderCreate2) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Owner, Level: AuthActive})
}

func (o LimitOrderCancel) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Owner, Level: AuthActive})
}

func (o Convert) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Owner, Level: AuthActive})
}

func (o CollateralizedConvert) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Owner, Level: AuthActive})
}

func (o FeedPublish) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Publisher, Level: AuthActive})
}

func (o ClaimRewardBalance) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthPosting})
}

func (o RecurrentTransfer) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.From, Level: AuthActive})
}

func (o WithdrawVesting) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthActive})
}

func (o ChangeRecoveryAccount) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.AccountToRecover, Level: AuthOwner})
}

func (o RequestAccountRecovery) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.RecoveryAccount, Level: AuthActive})
}

func (o RecoverAccount) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	// Both new owner and recent owner authorities must sign — get_required_authorities
	// in C++ uses get_required_authorities (not _active/_owner/_posting) and adds the
	// authority structs verbatim. We approximate as owner-of-account-to-recover.
	return append(dst, AuthorityRequirement{Account: o.AccountToRecover, Level: AuthOwner})
}

func (o TransferToSavings) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.From, Level: AuthActive})
}

func (o TransferFromSavings) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.From, Level: AuthActive})
}

func (o CancelTransferFromSavings) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.From, Level: AuthActive})
}

func (o EscrowTransfer) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.From, Level: AuthActive})
}

func (o EscrowApprove) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Who, Level: AuthActive})
}

func (o EscrowDispute) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Who, Level: AuthActive})
}

func (o EscrowRelease) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Who, Level: AuthActive})
}

func (o DelegateVestingShares) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Delegator, Level: AuthActive})
}

func (o ClaimAccount) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Creator, Level: AuthActive})
}

func (o CreateClaimedAccount) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Creator, Level: AuthActive})
}

func (o CreateProposal) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Creator, Level: AuthActive})
}

func (o UpdateProposal) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Creator, Level: AuthActive})
}

func (o UpdateProposalVotes) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Voter, Level: AuthActive})
}

func (o RemoveProposal) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.ProposalOwner, Level: AuthActive})
}

// Custom — every required_auths entry contributes an active requirement
// (custom_operation::get_required_active_authorities).
func (o Custom) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	for _, a := range o.RequiredAuths {
		dst = append(dst, AuthorityRequirement{Account: a, Level: AuthActive})
	}
	return dst
}

// CustomJSON — required_auths → active, required_posting_auths → posting.
// Mirrors custom_json_operation::get_required_active_authorities +
// get_required_posting_authorities.
func (o CustomJSON) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	for _, a := range o.RequiredAuths {
		dst = append(dst, AuthorityRequirement{Account: a, Level: AuthActive})
	}
	for _, a := range o.RequiredPostingAuths {
		dst = append(dst, AuthorityRequirement{Account: a, Level: AuthPosting})
	}
	return dst
}

// CustomBinary — required_owner_auths → owner, required_active_auths →
// active, required_posting_auths → posting. The required_auths field
// (a slice of full Authority structs) is rejected by hived's validate
// post-HF20 (custom_binary is effectively dead), so we don't surface
// those as account-level requirements.
func (o CustomBinary) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	for _, a := range o.RequiredOwnerAuths {
		dst = append(dst, AuthorityRequirement{Account: a, Level: AuthOwner})
	}
	for _, a := range o.RequiredActiveAuths {
		dst = append(dst, AuthorityRequirement{Account: a, Level: AuthActive})
	}
	for _, a := range o.RequiredPostingAuths {
		dst = append(dst, AuthorityRequirement{Account: a, Level: AuthPosting})
	}
	return dst
}

// WitnessBlockApprove uses get_required_witness_signatures (signed by
// the witness's signing key directly), not the standard owner/active/
// posting flow. RequiredAuthorities returns nothing for the
// account-tier walker; the live-sync sigverify path handles witness
// signatures separately.
func (o WitnessBlockApprove) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return dst
}

// SetWithdrawVestingRoute is signed with the from_account's active key.
func (o SetWithdrawVestingRoute) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.FromAccount, Level: AuthActive})
}

// AccountUpdate2 — exact hived rule (hive_operations.hpp:88-95): owner
// iff owner set; active iff active/posting/memo_key/json_metadata set;
// posting iff ONLY posting_json_metadata/extensions (a posting key signs).
func (o AccountUpdate2) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	switch {
	case o.Owner != nil:
		return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthOwner})
	case o.Active != nil || o.Posting != nil || o.MemoKey != nil || len(o.JSONMetadata) > 0:
		return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthActive})
	default:
		return append(dst, AuthorityRequirement{Account: o.Account, Level: AuthPosting})
	}
}

// AccountCreateWithDelegation — same as account_create.
func (o AccountCreateWithDelegation) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.Creator, Level: AuthActive})
}

// Pow — get_required_active_authorities returns the work account; key
// is recovered from the proof-of-work directly. We surface the work
// account as an active requirement so the autodetect helper sees it.
func (o Pow) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	return append(dst, AuthorityRequirement{Account: o.WorkerAccount, Level: AuthActive})
}

// Pow2 — worker account is encoded inside PowInput (variant 0) or
// EquihashWork.Input (variant 1).
func (o Pow2) RequiredAuthorities(dst []AuthorityRequirement) []AuthorityRequirement {
	worker := o.PowInput.WorkerAccount
	if o.WorkVariantTag == 1 {
		worker = o.EquihashWork.Input.WorkerAccount
	}
	if worker != "" {
		return append(dst, AuthorityRequirement{Account: worker, Level: AuthActive})
	}
	return dst
}
