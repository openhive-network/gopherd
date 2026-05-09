package main

import (
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// Usage: scan_block_ops <block_log> <start> <end> [account_filter]
// Print every op in [start, end] with op type + key fields.
func main() {
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
		_, hdrLen, _ := block.UnmarshalSignedHeader(raw)
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
				fmt.Printf("%d\t%s\n", n, summary)
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func opSummary(o op.Operation) string {
	switch v := o.(type) {
	case op.AccountWitnessVote:
		approve := "-"
		if v.Approve {
			approve = "+"
		}
		return fmt.Sprintf("witness_vote\t%s\t%s%s", v.Account, approve, v.Witness)
	case op.AccountWitnessProxy:
		return fmt.Sprintf("witness_proxy\t%s\tproxy=%q", v.Account, v.Proxy)
	case op.TransferToVesting:
		return fmt.Sprintf("transfer_to_vesting\tfrom=%s\tto=%s\tamt=%d", v.From, v.To, v.Amount.Amount)
	case op.WithdrawVesting:
		return fmt.Sprintf("withdraw_vesting\tacct=%s\tamt=%d", v.Account, v.VestingShares.Amount)
	case op.Transfer:
		return fmt.Sprintf("transfer\tfrom=%s\tto=%s\tamt=%d", v.From, v.To, v.Amount.Amount)
	case op.AccountCreate:
		return fmt.Sprintf("account_create\tcreator=%s\tnew=%s\tfee=%d", v.Creator, v.NewAccountName, v.Fee.Amount)
	case op.Pow:
		return fmt.Sprintf("pow\tworker=%s", v.WorkerAccount)
	case op.Pow2:
		w := ""
		if v.WorkVariantTag == 0 {
			w = v.PowInput.WorkerAccount
		} else if v.WorkVariantTag == 1 {
			w = v.EquihashWork.Input.WorkerAccount
		}
		return fmt.Sprintf("pow2\tworker=%s", w)
	case op.Comment:
		return fmt.Sprintf("comment\tauthor=%s\tparent=%s/%s\tperm=%s", v.Author, v.ParentAuthor, v.ParentPermlink, v.Permlink)
	case op.CommentOptions:
		return fmt.Sprintf("comment_options\tauthor=%s\tperm=%s\tmax_payout=%d\tpercent_hbd=%d", v.Author, v.Permlink, v.MaxAcceptedPayout.Amount, v.PercentHbd)
	case op.Vote:
		return fmt.Sprintf("vote\tvoter=%s\tauthor=%s\tperm=%s\tweight=%d", v.Voter, v.Author, v.Permlink, v.Weight)
	case op.LimitOrderCreate:
		return fmt.Sprintf("limit_order_create\towner=%s\tsell=%d\tmin_recv=%d", v.Owner, v.AmountToSell.Amount, v.MinToReceive.Amount)
	case op.LimitOrderCancel:
		return fmt.Sprintf("limit_order_cancel\towner=%s\torder_id=%d", v.Owner, v.OrderID)
	default:
		return fmt.Sprintf("op_%T", o)
	}
}
