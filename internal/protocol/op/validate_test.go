package op

import (
	"strings"
	"testing"

	"hived-go/internal/protocol/asset"
)

func hive(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.HiveSymbol}}
}

func hbd(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.HbdSymbol}}
}

// TestTransferValidate ports operation_tests.cpp::transfer_validate.
func TestTransferValidate(t *testing.T) {
	good := Transfer{From: "alice", To: "bob", Amount: hive(100), Memo: "Memo"}
	if err := good.Validate(); err != nil {
		t.Fatalf("good Transfer: %v", err)
	}

	cases := []struct {
		name string
		op   Transfer
	}{
		{"invalid from", Transfer{From: "alice-", To: "bob", Amount: hive(100)}},
		{"invalid to", Transfer{From: "alice", To: "bob-", Amount: hive(100)}},
		{"memo too long", Transfer{From: "alice", To: "bob", Amount: hive(100),
			Memo: strings.Repeat("x", MaxMemoSize+1)}},
		{"negative amount", Transfer{From: "alice", To: "bob", Amount: hive(-100)}},
		{"zero amount", Transfer{From: "alice", To: "bob", Amount: hive(0)}},
		{"unsupported symbol", Transfer{From: "alice", To: "bob",
			Amount: asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.VestsSymbol}}}},
	}
	for _, c := range cases {
		if err := c.op.Validate(); err == nil {
			t.Errorf("Transfer Validate: expected error for %q, got nil", c.name)
		}
	}
}

// TestVoteValidate ports operation_tests.cpp::vote_validate.
func TestVoteValidate(t *testing.T) {
	good := Vote{Voter: "alice", Author: "bob", Permlink: "p", Weight: 5000}
	if err := good.Validate(); err != nil {
		t.Fatalf("good Vote: %v", err)
	}
	// C++ vote_operation::validate calls validate_permlink, which permits an
	// EMPTY permlink (it checks only size < 256 and UTF-8). Rejecting it made
	// us refuse transactions hived accepts.
	for _, ok := range []Vote{
		{Voter: "alice", Author: "bob", Permlink: "", Weight: 5000},
		{Voter: "alice", Author: "bob", Permlink: strings.Repeat("a", 255), Weight: 5000},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("Vote Validate: C++ accepts this, we rejected: %v", err)
		}
	}
	bad := []struct {
		name string
		op   Vote
	}{
		{"invalid voter", Vote{Voter: "alice-", Author: "bob", Permlink: "p", Weight: 5000}},
		{"invalid author", Vote{Voter: "alice", Author: "bob-", Permlink: "p", Weight: 5000}},
		{"permlink at limit (C++ requires < 256)", Vote{Voter: "alice", Author: "bob", Permlink: strings.Repeat("a", 256), Weight: 5000}},
		{"permlink not utf8", Vote{Voter: "alice", Author: "bob", Permlink: "\xff\xfe", Weight: 5000}},
		{"weight too high", Vote{Voter: "alice", Author: "bob", Permlink: "p", Weight: 10001}},
		{"weight too low", Vote{Voter: "alice", Author: "bob", Permlink: "p", Weight: -10001}},
	}
	for _, c := range bad {
		if err := c.op.Validate(); err == nil {
			t.Errorf("Vote Validate: expected error for %q", c.name)
		}
	}
}

// TestCommentValidate ports operation_tests.cpp::comment_validate.
func TestCommentValidate(t *testing.T) {
	good := Comment{Author: "alice", Permlink: "p", Body: "hi"}
	if err := good.Validate(); err != nil {
		t.Fatalf("good Comment: %v", err)
	}
	// C++ validate_permlink has NO non-empty check — only vote/comment-style
	// ops; the DHF ops add validate_string_is_not_empty explicitly. We used to
	// reject an empty permlink here, which refuses transactions the network
	// accepts. At mempool admission that is invisible: the queue looks healthy
	// and our blocks are simply short.
	for _, ok := range []Comment{
		{Author: "alice", Permlink: "", Body: "x"},
		{Author: "alice", Permlink: "p", ParentPermlink: "", Body: "x"},
		{Author: "alice", Permlink: strings.Repeat("a", 255), Body: "x"},
		{Author: "alice", Permlink: "p", Title: strings.Repeat("a", 255), Body: "x"},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("Comment Validate: C++ accepts this, we rejected: %v", err)
		}
	}
	bad := []struct {
		name string
		op   Comment
	}{
		{"invalid author", Comment{Author: "alice-", Permlink: "p", Body: "x"}},
		{"permlink at limit (C++ requires < 256)", Comment{Author: "alice", Permlink: strings.Repeat("a", 256), Body: "x"}},
		{"permlink too long", Comment{Author: "alice", Permlink: strings.Repeat("a", 257), Body: "x"}},
		{"permlink not utf8", Comment{Author: "alice", Permlink: "\xff\xfe", Body: "x"}},
		{"empty body", Comment{Author: "alice", Permlink: "p", Body: ""}},
		{"body not utf8", Comment{Author: "alice", Permlink: "p", Body: "\xff\xfe"}},
		{"title at limit (C++ requires <= 255)", Comment{Author: "alice", Permlink: "p", Title: strings.Repeat("a", 256), Body: "x"}},
		{"invalid parent", Comment{Author: "alice", Permlink: "p", ParentAuthor: "bob-", Body: "x"}},
	}
	for _, c := range bad {
		if err := c.op.Validate(); err == nil {
			t.Errorf("Comment Validate: expected error for %q", c.name)
		}
	}
}

// TestAccountWitnessVoteValidate ports operation_tests.cpp::
// account_witness_vote_validate.
func TestAccountWitnessVoteValidate(t *testing.T) {
	good := AccountWitnessVote{Account: "alice", Witness: "bob", Approve: true}
	if err := good.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}
	bad := []AccountWitnessVote{
		{Account: "alice-", Witness: "bob"},
		{Account: "alice", Witness: "bob-"},
		{Account: "", Witness: "bob"},
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}

// TestAccountWitnessProxyValidate ports operation_tests.cpp::
// account_witness_proxy_validate. Empty proxy is valid (means clear).
func TestAccountWitnessProxyValidate(t *testing.T) {
	good := []AccountWitnessProxy{
		{Account: "alice", Proxy: "bob"},
		{Account: "alice", Proxy: ""}, // clear proxy
	}
	for _, g := range good {
		if err := g.Validate(); err != nil {
			t.Errorf("good %+v: %v", g, err)
		}
	}
	bad := []AccountWitnessProxy{
		{Account: "alice-", Proxy: "bob"},
		{Account: "alice", Proxy: "bob-"},
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}

// TestTransferToVestingValidate ports transfer_to_vesting_validate.
func TestTransferToVestingValidate(t *testing.T) {
	good := []TransferToVesting{
		{From: "alice", To: "bob", Amount: hive(100)},
		{From: "alice", To: "", Amount: hive(100)}, // empty to = same as from
	}
	for _, g := range good {
		if err := g.Validate(); err != nil {
			t.Errorf("good %+v: %v", g, err)
		}
	}
	bad := []TransferToVesting{
		{From: "alice-", To: "bob", Amount: hive(100)},
		{From: "alice", To: "bob", Amount: hive(0)},
		{From: "alice", To: "bob", Amount: hive(-1)},
		{From: "alice", To: "bob", Amount: hbd(100)}, // wrong symbol
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}

// TestLimitOrderCreateValidate ports limit_order_create_validate.
func TestLimitOrderCreateValidate(t *testing.T) {
	good := LimitOrderCreate{
		Owner:        "alice",
		AmountToSell: hive(100),
		MinToReceive: hbd(100),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}
	bad := []LimitOrderCreate{
		{Owner: "alice-", AmountToSell: hive(100), MinToReceive: hbd(100)},
		{Owner: "alice", AmountToSell: hive(0), MinToReceive: hbd(100)},
		{Owner: "alice", AmountToSell: hive(100), MinToReceive: hbd(0)},
		{Owner: "alice", AmountToSell: hive(100), MinToReceive: hive(100)}, // same asset
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}

// TestAccountCreateValidate ports account_create_validate.
func TestAccountCreateValidate(t *testing.T) {
	good := AccountCreate{Creator: "alice", NewAccountName: "bob", Fee: hive(0)}
	if err := good.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}
	bad := []AccountCreate{
		{Creator: "alice-", NewAccountName: "bob", Fee: hive(0)},
		{Creator: "alice", NewAccountName: "bob-", Fee: hive(0)},
		{Creator: "alice", NewAccountName: "bob", Fee: hive(-1)},
		{Creator: "alice", NewAccountName: "bob", Fee: hbd(0)}, // wrong symbol
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}

// TestDeclineVotingRightsValidate verifies the simple validator.
func TestDeclineVotingRightsValidate(t *testing.T) {
	good := DeclineVotingRights{Account: "alice", Decline: true}
	if err := good.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}
	if err := (DeclineVotingRights{Account: "alice-", Decline: true}).Validate(); err == nil {
		t.Error("expected error for invalid account")
	}
}
