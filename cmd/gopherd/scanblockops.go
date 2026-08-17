package main

import (
	"fmt"
	"os"
	"strconv"

	"gopherd/internal/blocklog"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
)

// Usage: scan_block_ops <block_log> <start> <end> [account_filter]
// Print every op in [start, end] with op type + key fields.
func cmdScanOps(args []string) int {
	os.Args = append([]string{"gopherd-scan-ops"}, args...)

	r, err := blocklog.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer r.Close()
	start, _ := strconv.Atoi(os.Args[2])
	end, _ := strconv.Atoi(os.Args[3])
	filter := ""
	if len(os.Args) > 4 {
		filter = os.Args[4]
	}
	for n := uint32(start); n <= uint32(end); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		hdr, hdrLen, _ := block.UnmarshalSignedHeader(raw)
		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rd.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				continue
			}
			for _, o := range txRes.SignedTx.Operations {
				summary := opSummary(o)
				if filter != "" && !contains(summary, filter) {
					continue
				}
				fmt.Printf("%d\t%d\t%s\n", n, hdr.Timestamp, summary)
			}
		}
	}
	return 0
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func opSummary(o op.Op) string {
	switch o.Tag {
	case op.TagAccountWitnessVote:
		approve := "-"
		if o.AsAccountWitnessVote().Approve {
			approve = "+"
		}
		return fmt.Sprintf("witness_vote\t%s\t%s%s", o.AsAccountWitnessVote().Account, approve, o.AsAccountWitnessVote().Witness)
	case op.TagAccountWitnessProxy:
		return fmt.Sprintf("witness_proxy\t%s\tproxy=%q", o.AsAccountWitnessProxy().Account, o.AsAccountWitnessProxy().Proxy)
	case op.TagTransferToVesting:
		return fmt.Sprintf("transfer_to_vesting\tfrom=%s\tto=%s\tamt=%d", o.AsTransferToVesting().From, o.AsTransferToVesting().To, o.AsTransferToVesting().Amount.Amount)
	case op.TagWithdrawVesting:
		return fmt.Sprintf("withdraw_vesting\tacct=%s\tamt=%d", o.AsWithdrawVesting().Account, o.AsWithdrawVesting().VestingShares.Amount)
	case op.TagTransfer:
		return fmt.Sprintf("transfer\tfrom=%s\tto=%s\tamt=%d", o.AsTransfer().From, o.AsTransfer().To, o.AsTransfer().Amount.Amount)
	case op.TagAccountCreate:
		return fmt.Sprintf("account_create\tcreator=%s\tnew=%s\tfee=%d", o.AsAccountCreate().Creator, o.AsAccountCreate().NewAccountName, o.AsAccountCreate().Fee.Amount)
	case op.TagPow:
		return fmt.Sprintf("pow\tworker=%s", o.AsPow().WorkerAccount)
	case op.TagPow2:
		w := ""
		if o.AsPow2().WorkVariantTag == 0 {
			w = o.AsPow2().PowInput.WorkerAccount
		} else if o.AsPow2().WorkVariantTag == 1 {
			w = o.AsPow2().EquihashWork.Input.WorkerAccount
		}
		return fmt.Sprintf("pow2\tworker=%s", w)
	case op.TagComment:
		return fmt.Sprintf("comment\tauthor=%s\tparent=%s/%s\tperm=%s", o.AsComment().Author, o.AsComment().ParentAuthor, o.AsComment().ParentPermlink, o.AsComment().Permlink)
	case op.TagCommentOptions:
		return fmt.Sprintf("comment_options\tauthor=%s\tperm=%s\tmax_payout=%d\tpercent_hbd=%d", o.AsCommentOptions().Author, o.AsCommentOptions().Permlink, o.AsCommentOptions().MaxAcceptedPayout.Amount, o.AsCommentOptions().PercentHbd)
	case op.TagSetWithdrawVestingRoute:
		r := o.AsSetWithdrawVestingRoute()
		return fmt.Sprintf("set_withdraw_vesting_route\tfrom=%s\tto=%s\tpercent=%d\tauto_vest=%v", r.FromAccount, r.ToAccount, r.Percent, r.AutoVest)
	case op.TagDelegateVestingShares:
		d := o.AsDelegateVestingShares()
		return fmt.Sprintf("delegate_vesting_shares\tdelegator=%s\tdelegatee=%s\tvests=%d", d.Delegator, d.Delegatee, d.VestingShares.Amount)
	case op.TagClaimAccount:
		c := o.AsClaimAccount()
		return fmt.Sprintf("claim_account\tcreator=%s\tfee=%d", c.Creator, c.Fee.Amount)
	case op.TagCreateClaimedAccount:
		c := o.AsCreateClaimedAccount()
		return fmt.Sprintf("create_claimed_account\tcreator=%s\tnew=%s", c.Creator, c.NewAccountName)
	case op.TagWitnessSetProperties:
		return fmt.Sprintf("witness_set_properties\towner=%s", o.AsWitnessSetProperties().Owner)
	case op.TagClaimRewardBalance:
		c := o.AsClaimRewardBalance()
		return fmt.Sprintf("claim_reward_balance\tacct=%s\thive=%d\thbd=%d\tvests=%d", c.Account, c.RewardHive.Amount, c.RewardHbd.Amount, c.RewardVests.Amount)
	case op.TagDeleteComment:
		return fmt.Sprintf("delete_comment\tauthor=%s\tperm=%s", o.AsDeleteComment().Author, o.AsDeleteComment().Permlink)
	case op.TagVote:
		return fmt.Sprintf("vote\tvoter=%s\tauthor=%s\tperm=%s\tweight=%d", o.AsVote().Voter, o.AsVote().Author, o.AsVote().Permlink, o.AsVote().Weight)
	case op.TagLimitOrderCreate:
		return fmt.Sprintf("limit_order_create\towner=%s\tsell=%d\tmin_recv=%d", o.AsLimitOrderCreate().Owner, o.AsLimitOrderCreate().AmountToSell.Amount, o.AsLimitOrderCreate().MinToReceive.Amount)
	case op.TagLimitOrderCancel:
		return fmt.Sprintf("limit_order_cancel\towner=%s\torder_id=%d", o.AsLimitOrderCancel().Owner, o.AsLimitOrderCancel().OrderID)
	default:
		return fmt.Sprintf("op_%T", o)
	}
}
