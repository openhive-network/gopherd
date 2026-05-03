package op

import (
	"sort"
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// Mirrors the per-op _authorities test pattern from
// libraries/chain/tests/operation_tests.cpp:
//
//   - get_required_owner_authorities → expected
//   - get_required_active_authorities → expected
//   - get_required_posting_authorities → expected
//
// We assemble the union via CollectRequiredAuthorities and bucket per
// level for comparison.

func authoritiesByLevel(t *testing.T, ops []Operation) (owner, active, posting []string) {
	t.Helper()
	for _, r := range CollectRequiredAuthorities(ops) {
		switch r.Level {
		case AuthOwner:
			owner = append(owner, string(r.Account))
		case AuthActive:
			active = append(active, string(r.Account))
		case AuthPosting:
			posting = append(posting, string(r.Account))
		}
	}
	sort.Strings(owner)
	sort.Strings(active)
	sort.Strings(posting)
	return
}

func wantAuths(t *testing.T, ops []Operation, wantOwner, wantActive, wantPosting []string) {
	t.Helper()
	owner, active, posting := authoritiesByLevel(t, ops)
	wantOwner, wantActive, wantPosting = sortStrs(wantOwner), sortStrs(wantActive), sortStrs(wantPosting)
	if !strSliceEq(owner, wantOwner) {
		t.Errorf("owner = %v, want %v", owner, wantOwner)
	}
	if !strSliceEq(active, wantActive) {
		t.Errorf("active = %v, want %v", active, wantActive)
	}
	if !strSliceEq(posting, wantPosting) {
		t.Errorf("posting = %v, want %v", posting, wantPosting)
	}
}

func sortStrs(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func vest(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.VestsSymbol}}
}

// ---- account_create ---------------------------------------------------

// TestAccountCreateAuthorities mirrors operation_tests.cpp::account_create_authorities.
// Active authority of the creator is required; owner/posting are not.
func TestAccountCreateAuthorities(t *testing.T) {
	op := AccountCreate{Creator: "alice", NewAccountName: "bob"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- account_create_with_delegation -----------------------------------

func TestAccountCreateWithDelegationAuthorities(t *testing.T) {
	op := AccountCreateWithDelegation{Creator: "alice", NewAccountName: "bob"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- account_update ---------------------------------------------------

// TestAccountUpdateAuthorities mirrors operation_tests.cpp::account_update_authorities.
// Default (no owner field touched) requires active authority of the
// account. Touching owner promotes it to owner authority.
func TestAccountUpdateAuthorities(t *testing.T) {
	t.Run("active-only", func(t *testing.T) {
		op := AccountUpdate{Account: "alice"}
		wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
	})
}

func TestAccountUpdate2Authorities(t *testing.T) {
	op := AccountUpdate2{Account: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- comment ----------------------------------------------------------

func TestCommentAuthorities(t *testing.T) {
	op := Comment{Author: "alice", Permlink: "p"}
	wantAuths(t, []Operation{op}, nil, nil, []string{"alice"})
}

// ---- vote -------------------------------------------------------------

func TestVoteAuthorities(t *testing.T) {
	op := Vote{Voter: "alice", Author: "bob", Permlink: "p", Weight: 100}
	wantAuths(t, []Operation{op}, nil, nil, []string{"alice"})
}

// ---- transfer ---------------------------------------------------------

func TestTransferAuthorities(t *testing.T) {
	op := Transfer{From: "alice", To: "bob", Amount: hive(10)}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- transfer_to_vesting ---------------------------------------------

func TestTransferToVestingAuthorities(t *testing.T) {
	op := TransferToVesting{From: "alice", To: "bob", Amount: hive(10)}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- withdraw_vesting -------------------------------------------------

func TestWithdrawVestingAuthorities(t *testing.T) {
	op := WithdrawVesting{Account: "alice", VestingShares: vest(1_000)}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- witness_update ---------------------------------------------------

func TestWitnessUpdateAuthorities(t *testing.T) {
	op := WitnessUpdate{Owner: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// witness_set_properties: signed by the witness's signing key directly
// — no account-level authority requirement.
func TestWitnessSetPropertiesAuthorities(t *testing.T) {
	op := WitnessSetProperties{Owner: "alice"}
	wantAuths(t, []Operation{op}, nil, nil, nil)
}

// ---- account_witness_vote / proxy ------------------------------------

func TestAccountWitnessVoteAuthorities(t *testing.T) {
	op := AccountWitnessVote{Account: "alice", Witness: "bob", Approve: true}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestAccountWitnessProxyAuthorities(t *testing.T) {
	op := AccountWitnessProxy{Account: "alice", Proxy: "bob"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// witness_block_approve uses get_required_witness_signatures — no
// account-tier requirement.
func TestAccountWitnessBlockApproveAuthorities(t *testing.T) {
	op := WitnessBlockApprove{Witness: "alice"}
	wantAuths(t, []Operation{op}, nil, nil, nil)
}

// ---- custom / custom_json / custom_binary ----------------------------

// TestCustomAuthorities mirrors custom_authorities. All required_auths
// entries become active requirements; owner/posting empty.
func TestCustomAuthorities(t *testing.T) {
	op := Custom{RequiredAuths: []auth.AccountName{"alice", "bob"}, ID: 1}
	wantAuths(t, []Operation{op}, nil, []string{"alice", "bob"}, nil)
}

// TestCustomJSONAuthorities mirrors custom_json_authorities.
// required_auths → active, required_posting_auths → posting.
func TestCustomJSONAuthorities(t *testing.T) {
	op := CustomJSON{
		RequiredAuths:        []auth.AccountName{"alice"},
		RequiredPostingAuths: []auth.AccountName{"bob"},
		ID:                   "x",
		JSON:                 "{}",
	}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, []string{"bob"})
}

// TestCustomBinaryAuthorities mirrors custom_binary_authorities. Owner
// and posting auths surface; the embedded full-Authority slice does
// not generate account-level requirements (custom_binary is HF-rejected
// post-HF20 and is functionally dead, but we still surface the obvious
// account fields for autodetect routing).
func TestCustomBinaryAuthorities(t *testing.T) {
	op := CustomBinary{
		RequiredOwnerAuths:   []auth.AccountName{"alice"},
		RequiredActiveAuths:  []auth.AccountName{"bob"},
		RequiredPostingAuths: []auth.AccountName{"carol"},
	}
	wantAuths(t, []Operation{op},
		[]string{"alice"},
		[]string{"bob"},
		[]string{"carol"})
}

// ---- feed_publish, convert, collateralized_convert -------------------

func TestFeedPublishAuthorities(t *testing.T) {
	op := FeedPublish{Publisher: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestConvertAuthorities(t *testing.T) {
	op := Convert{Owner: "alice", RequestID: 1, Amount: hbd(10)}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestCollateralizedConvertAuthorities(t *testing.T) {
	op := CollateralizedConvert{Owner: "alice", RequestID: 1, Amount: hive(10)}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- limit_order_* ---------------------------------------------------

func TestLimitOrderCreateAuthorities(t *testing.T) {
	op := LimitOrderCreate{Owner: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestLimitOrderCreate2Authorities(t *testing.T) {
	op := LimitOrderCreate2{Owner: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestLimitOrderCancelAuthorities(t *testing.T) {
	op := LimitOrderCancel{Owner: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- escrow ----------------------------------------------------------

func TestEscrowTransferAuthorities(t *testing.T) {
	op := EscrowTransfer{From: "alice", To: "bob", Agent: "carol"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// EscrowApprove — only `who` (the approver) is required as active.
func TestEscrowApproveAuthorities(t *testing.T) {
	op := EscrowApprove{Who: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestEscrowDisputeAuthorities(t *testing.T) {
	op := EscrowDispute{Who: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestEscrowReleaseAuthorities(t *testing.T) {
	op := EscrowRelease{Who: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- transfer_to_savings / from / cancel_from ------------------------

func TestTransferToSavingsAuthorities(t *testing.T) {
	op := TransferToSavings{From: "alice", To: "bob"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestTransferFromSavingsAuthorities(t *testing.T) {
	op := TransferFromSavings{From: "alice", To: "bob"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestCancelTransferFromSavingsAuthorities(t *testing.T) {
	op := CancelTransferFromSavings{From: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- decline_voting_rights -------------------------------------------

// Owner authority required (irreversibility safeguard).
func TestDeclineVotingRightsAuthorities(t *testing.T) {
	op := DeclineVotingRights{Account: "alice", Decline: true}
	wantAuths(t, []Operation{op}, []string{"alice"}, nil, nil)
}

// ---- claim_reward_balance --------------------------------------------

func TestClaimRewardBalanceAuthorities(t *testing.T) {
	op := ClaimRewardBalance{Account: "alice"}
	wantAuths(t, []Operation{op}, nil, nil, []string{"alice"})
}

// ---- delegate_vesting_shares -----------------------------------------

func TestDelegateVestingSharesAuthorities(t *testing.T) {
	op := DelegateVestingShares{Delegator: "alice", Delegatee: "bob"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- claim_account / create_claimed_account --------------------------

func TestClaimAccountAuthorities(t *testing.T) {
	op := ClaimAccount{Creator: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestCreateClaimedAccountAuthorities(t *testing.T) {
	op := CreateClaimedAccount{Creator: "alice", NewAccountName: "bob"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

// ---- pow / pow2 ------------------------------------------------------

// Pow returns the work account's active authority (the work itself
// implicitly proves the signing key, but the autodetect API still
// surfaces the account so the right key gets routed).
func TestPowAuthorities(t *testing.T) {
	op := Pow{WorkerAccount: "alice"}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}

func TestPow2AuthoritiesVariant0(t *testing.T) {
	op := Pow2{WorkVariantTag: 0, PowInput: Pow2Input{WorkerAccount: "alice"}}
	wantAuths(t, []Operation{op}, nil, []string{"alice"}, nil)
}
