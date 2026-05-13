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

// scanaccount scans the block_log for every operation that touches
// the given account name. Useful for tracing where a balance came
// from. Reports virtual ops (cashout etc.) only via real-op effects
// — author cashouts show up as the parent comment ops.
//
// usage: scanaccount <block_log> <account> <start_block> <end_block>

func main() {
	if len(os.Args) < 5 {
		fmt.Println("usage: scanaccount <block_log> <account> <start_block> <end_block>")
		os.Exit(2)
	}
	r, _ := blocklog.Open(os.Args[1])
	defer r.Close()
	acct := os.Args[2]
	startBlock, _ := strconv.Atoi(os.Args[3])
	endBlock, _ := strconv.Atoi(os.Args[4])
	for n := uint32(startBlock); n <= uint32(endBlock); n++ {
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
				switch o := o.Operation().(type) {
				case op.Transfer:
					if string(o.From) == acct || string(o.To) == acct {
						fmt.Printf("block %d TRANSFER from=%s to=%s amount=%d sym=%d\n",
							n, o.From, o.To, o.Amount.Amount, o.Amount.Symbol.Num)
					}
				case op.TransferToVesting:
					to := o.To
					if to == "" {
						to = string(o.From)
					}
					if string(o.From) == acct || to == acct {
						fmt.Printf("block %d TRANSFER_TO_VESTING from=%s to=%s amount=%d\n",
							n, o.From, to, o.Amount.Amount)
					}
				case op.WithdrawVesting:
					if string(o.Account) == acct {
						fmt.Printf("block %d WITHDRAW_VESTING acct=%s amount=%d\n",
							n, o.Account, o.VestingShares.Amount)
					}
				case op.Comment:
					if string(o.Author) == acct {
						fmt.Printf("block %d COMMENT author=%s permlink=%s parent=%s/%s\n",
							n, o.Author, o.Permlink, o.ParentAuthor, o.ParentPermlink)
					}
				case op.CommentOptions:
					if string(o.Author) == acct {
						fmt.Printf("block %d COMMENT_OPTIONS author=%s permlink=%s percent_hbd=%d max_payout=%d\n",
							n, o.Author, o.Permlink, o.PercentHbd, o.MaxAcceptedPayout.Amount)
					}
				case op.Vote:
					if string(o.Voter) == acct || string(o.Author) == acct {
						fmt.Printf("block %d VOTE voter=%s author=%s permlink=%s weight=%d\n",
							n, o.Voter, o.Author, o.Permlink, o.Weight)
					}
				case op.AccountCreate:
					if string(o.Creator) == acct || string(o.NewAccountName) == acct {
						fmt.Printf("block %d ACCOUNT_CREATE creator=%s new=%s fee=%d\n",
							n, o.Creator, o.NewAccountName, o.Fee.Amount)
					}
				case op.Convert:
					if string(o.Owner) == acct {
						fmt.Printf("block %d CONVERT owner=%s amount=%d sym=%d\n",
							n, o.Owner, o.Amount.Amount, o.Amount.Symbol.Num)
					}
				case op.LimitOrderCreate:
					if string(o.Owner) == acct {
						fmt.Printf("block %d LIMIT_ORDER_CREATE owner=%s id=%d sell=%d sym=%d min_to_receive=%d exp=%d\n",
							n, o.Owner, o.OrderID, o.AmountToSell.Amount, o.AmountToSell.Symbol.Num, o.MinToReceive.Amount, o.Expiration)
					}
				case op.LimitOrderCreate2:
					if string(o.Owner) == acct {
						fmt.Printf("block %d LIMIT_ORDER_CREATE2 owner=%s id=%d sell=%d sym=%d exchange_rate=%d/%d exp=%d\n",
							n, o.Owner, o.OrderID, o.AmountToSell.Amount, o.AmountToSell.Symbol.Num, o.ExchangeRate.Base.Amount, o.ExchangeRate.Quote.Amount, o.Expiration)
					}
				case op.LimitOrderCancel:
					if string(o.Owner) == acct {
						fmt.Printf("block %d LIMIT_ORDER_CANCEL owner=%s id=%d\n",
							n, o.Owner, o.OrderID)
					}
				case op.TransferFromSavings:
					if string(o.From) == acct || string(o.To) == acct {
						fmt.Printf("block %d TRANSFER_FROM_SAVINGS from=%s to=%s amount=%d sym=%d req=%d\n",
							n, o.From, o.To, o.Amount.Amount, o.Amount.Symbol.Num, o.RequestID)
					}
				case op.TransferToSavings:
					if string(o.From) == acct || string(o.To) == acct {
						fmt.Printf("block %d TRANSFER_TO_SAVINGS from=%s to=%s amount=%d sym=%d\n",
							n, o.From, o.To, o.Amount.Amount, o.Amount.Symbol.Num)
					}
				case op.CancelTransferFromSavings:
					if string(o.From) == acct {
						fmt.Printf("block %d CANCEL_TRANSFER_FROM_SAVINGS from=%s req=%d\n",
							n, o.From, o.RequestID)
					}
				case op.SetWithdrawVestingRoute:
					if string(o.FromAccount) == acct || string(o.ToAccount) == acct {
						fmt.Printf("block %d SET_WITHDRAW_VESTING_ROUTE from=%s to=%s pct=%d auto_vest=%t\n",
							n, o.FromAccount, o.ToAccount, o.Percent, o.AutoVest)
					}
				}
			}
		}
	}
}
