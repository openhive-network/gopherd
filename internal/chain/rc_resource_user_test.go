package chain

import (
	"testing"
	"unsafe"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
)

// TestResourceUserOfMirrorsHived pins resource_credits::get_resource_user
// (rc/resource_user.cpp): every operation is visited in order until one
// names a payer; active is preferred over posting for custom_json; and the
// operations the old tag switch did not map (account_update2,
// witness_set_properties, custom, custom_binary, witness_block_approve) now
// name their payer. Under HF29 an unmapped payer is a rejected transaction,
// and account_update2 and witness_set_properties are common on mainnet.
func TestResourceUserOfMirrorsHived(t *testing.T) {
	mk := func(ops ...op.Op) *tx.SignedTransaction {
		stx := &tx.SignedTransaction{}
		stx.Operations = ops
		return stx
	}
	cases := []struct {
		name string
		stx  *tx.SignedTransaction
		want string
	}{
		{"account_update2", mk(op.OpFromPtr(op.TagAccountUpdate2, unsafe.Pointer(&op.AccountUpdate2{Account: "alice"}))), "alice"},
		{"witness_set_properties", mk(op.OpFromPtr(op.TagWitnessSetProperties, unsafe.Pointer(&op.WitnessSetProperties{Owner: "gtg"}))), "gtg"},
		{"custom", mk(op.OpFromPtr(op.TagCustom, unsafe.Pointer(&op.Custom{RequiredAuths: []auth.AccountName{"carol"}}))), "carol"},
		{"custom_binary owner only", mk(op.OpFromPtr(op.TagCustomBinary, unsafe.Pointer(&op.CustomBinary{RequiredOwnerAuths: []auth.AccountName{"dave"}}))), "dave"},
		{"witness_block_approve", mk(op.OpFromPtr(op.TagWitnessBlockApprove, unsafe.Pointer(&op.WitnessBlockApprove{Witness: "blocktrades"}))), "blocktrades"},
		{"custom_json active before posting", mk(op.OpFromPtr(op.TagCustomJSON, unsafe.Pointer(&op.CustomJSON{
			RequiredAuths:        []auth.AccountName{"active-acct"},
			RequiredPostingAuths: []auth.AccountName{"posting-acct"},
		}))), "active-acct"},
		{"custom_json posting only", mk(op.OpFromPtr(op.TagCustomJSON, unsafe.Pointer(&op.CustomJSON{
			RequiredPostingAuths: []auth.AccountName{"posting-acct"},
		}))), "posting-acct"},
		{"walks past a payer-less first op", mk(
			op.OpFromPtr(op.TagCustomJSON, unsafe.Pointer(&op.CustomJSON{})),
			op.OpFromPtr(op.TagTransfer, unsafe.Pointer(&op.Transfer{From: "bob", To: "alice"})),
		), "bob"},
		{"recover_account pays as account_to_recover", mk(op.OpFromPtr(op.TagRecoverAccount, unsafe.Pointer(&op.RecoverAccount{AccountToRecover: "victim"}))), "victim"},
		{"no payer at all", mk(op.OpFromPtr(op.TagCustomJSON, unsafe.Pointer(&op.CustomJSON{}))), ""},
	}
	for _, c := range cases {
		if got := ResourceUserOf(c.stx); got != c.want {
			t.Errorf("%s: ResourceUserOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTransactionExpirationHours pins resource_count.cpp:621, the
// round-up-to-whole-hours factor on the per-transaction state charge.
func TestTransactionExpirationHours(t *testing.T) {
	cases := []struct {
		exp, now uint32
		want     int64
	}{
		{1000, 1000, 0},
		{1001, 1000, 1},
		{1000 + 3600, 1000, 1},
		{1000 + 3601, 1000, 2},
		{1000 + 24*3600, 1000, 24},
		{900, 1000, 0},
	}
	for _, c := range cases {
		if got := transactionExpirationHours(c.exp, c.now); got != c.want {
			t.Errorf("hours(exp=%d, now=%d) = %d, want %d", c.exp, c.now, got, c.want)
		}
	}
	_ = crypto.PublicKeySize
}
