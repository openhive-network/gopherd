//go:build testnet

package main

import (
	"fmt"
	"math/rand"
	"time"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
)

// generator produces zero or more operations for a bot. Returning nil means
// "preconditions not met right now" (no open order to cancel, no comment to
// vote on) and is counted as a skip, not a failure — a bot that has not posted
// yet legitimately cannot delete a post.
type generator struct {
	name   string
	weight int
	gen    func(b *bot, w *world, r *rand.Rand) []op.Operation
}

// pendingCommit lets a generator defer recording state until the chain ACCEPTS
// the transaction. Bots used to record a permlink the moment they built the
// comment, so a post refused for posting too fast still became a vote target —
// and every vote on it failed !comment_is_required. That is the generator
// lying to itself, not a chain bug, and it accounted for most vote failures.
type pendingCommit struct {
	b      *bot
	commit func()
}

func acct(s string) auth.AccountName { return auth.AccountName(s) }

func nowSec() uint32 { return uint32(time.Now().Unix()) }

// generators covers the operation surface a testnet can realistically exercise.
//
// Deliberately excluded, with reasons:
//
//	pow / pow2                mining, dead since HF16; the evaluator rejects them
//	witness_block_approve     a fast-confirm message, never valid inside a block
//	custom_binary             rejected from HF20 onward
//	recover_account           needs a request + the old key, a multi-step dance
//	reset_account             the reset flow was never enabled on mainnet
//	decline_voting_rights     one-way and irreversible: it would permanently
//	                          remove a bot from governance, shrinking coverage
//	                          for the rest of the run
//	set_reset_account         hived asserts `false && ...` — hard-disabled, the
//	                          reset flow was never turned on
//	account_create_with_delegation
//	                          asserts !has_hardfork(HF20); dead from HF20, and a
//	                          testnet starts at HF28
//
// The last two were found by RUNNING the generator: both looked plausible from
// the operation list and only the chain says they cannot succeed.
var generators = []generator{
	// ---- value transfer ----
	{"transfer", 60, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		o := w.otherBot(b, r)
		if o == nil {
			return nil
		}
		amt := hiveSat(int64(1 + r.Intn(5000)))
		if r.Intn(2) == 0 {
			amt = hbdSat(int64(1 + r.Intn(2000)))
		}
		return []op.Operation{op.Transfer{From: acct(b.name), To: acct(o.name), Amount: amt,
			Memo: fmt.Sprintf("bot %d", r.Intn(1000))}}
	}},
	{"transfer_to_vesting", 12, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		to := b.name
		if o := w.otherBot(b, r); o != nil && r.Intn(4) == 0 {
			to = o.name
		}
		return []op.Operation{op.TransferToVesting{From: acct(b.name), To: acct(to),
			Amount: hiveSat(int64(1000 + r.Intn(50000)))}}
	}},
	{"transfer_to_savings", 12, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		amt := hiveSat(int64(100 + r.Intn(5000)))
		if r.Intn(2) == 0 {
			amt = hbdSat(int64(100 + r.Intn(2000)))
		}
		return []op.Operation{op.TransferToSavings{From: acct(b.name), To: acct(b.name), Amount: amt, Memo: "savings"}}
	}},
	{"transfer_from_savings", 10, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		id := b.nextSeq()
		b.pending = func() { b.savingsReqs = append(b.savingsReqs, id) }
		return []op.Operation{op.TransferFromSavings{From: acct(b.name), RequestID: id,
			To: acct(b.name), Amount: hiveSat(int64(10 + r.Intn(500))), Memo: "out"}}
	}},
	{"cancel_transfer_from_savings", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.savingsReqs) == 0 {
			return nil
		}
		i := r.Intn(len(b.savingsReqs))
		id := b.savingsReqs[i]
		b.savingsReqs = append(b.savingsReqs[:i], b.savingsReqs[i+1:]...)
		return []op.Operation{op.CancelTransferFromSavings{From: acct(b.name), RequestID: id}}
	}},
	{"withdraw_vesting", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.WithdrawVesting{Account: acct(b.name), VestingShares: vestSat(int64(1_000_000 + r.Intn(9_000_000)))}}
	}},
	{"set_withdraw_vesting_route", 5, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		o := w.otherBot(b, r)
		if o == nil {
			return nil
		}
		return []op.Operation{op.SetWithdrawVestingRoute{FromAccount: acct(b.name), ToAccount: acct(o.name),
			Percent: uint16(r.Intn(10000)), AutoVest: r.Intn(2) == 0}}
	}},
	{"delegate_vesting_shares", 8, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		o := w.otherBot(b, r)
		if o == nil {
			return nil
		}
		return []op.Operation{op.DelegateVestingShares{Delegator: acct(b.name), Delegatee: acct(o.name),
			VestingShares: vestSat(int64(r.Intn(5_000_000)))}}
	}},
	{"claim_reward_balance", 8, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		// hived asserts at least one amount is non-zero, so claiming zeros is
		// always refused. Ask what is actually pending and skip when nothing is.
		h, d, v := w.pendingRewards(b.name)
		if h == 0 && d == 0 && v == 0 {
			return nil
		}
		return []op.Operation{op.ClaimRewardBalance{Account: acct(b.name),
			RewardHive: hiveSat(h), RewardHbd: hbdSat(d), RewardVests: vestSat(v)}}
	}},
	{"recurrent_transfer", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		o := w.otherBot(b, r)
		if o == nil {
			return nil
		}
		return []op.Operation{op.RecurrentTransfer{From: acct(b.name), To: acct(o.name),
			Amount: hiveSat(int64(10 + r.Intn(100))), Memo: "rec",
			Recurrence: uint16(24 + r.Intn(48)), Executions: uint16(2 + r.Intn(5)), Extensions: []byte{0x00}}}
	}},
	{"convert", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.Convert{Owner: acct(b.name), RequestID: b.nextSeq(),
			Amount: hbdSat(int64(10 + r.Intn(500)))}}
	}},
	{"collateralized_convert", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.CollateralizedConvert{Owner: acct(b.name), RequestID: b.nextSeq(),
			Amount: hiveSat(int64(100 + r.Intn(2000)))}}
	}},

	// ---- market ----
	{"limit_order_create", 14, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		id := b.nextSeq()
		b.pending = func() { b.orders = append(b.orders, id) }
		return []op.Operation{op.LimitOrderCreate{Owner: acct(b.name), OrderID: id,
			AmountToSell: hiveSat(int64(100 + r.Intn(3000))),
			MinToReceive: hbdSat(int64(100 + r.Intn(3000))),
			FillOrKill:   false, Expiration: nowSec() + uint32(600+r.Intn(3600))}}
	}},
	{"limit_order_create2", 10, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		id := b.nextSeq()
		b.pending = func() { b.orders = append(b.orders, id) }
		return []op.Operation{op.LimitOrderCreate2{Owner: acct(b.name), OrderID: id,
			AmountToSell: hbdSat(int64(100 + r.Intn(2000))), FillOrKill: false,
			ExchangeRate: asset.Price{Base: hbdSat(1000), Quote: hiveSat(int64(500 + r.Intn(2000)))},
			Expiration:   nowSec() + uint32(600+r.Intn(3600))}}
	}},
	{"limit_order_cancel", 10, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.orders) == 0 {
			return nil
		}
		i := r.Intn(len(b.orders))
		id := b.orders[i]
		b.orders = append(b.orders[:i], b.orders[i+1:]...)
		return []op.Operation{op.LimitOrderCancel{Owner: acct(b.name), OrderID: id}}
	}},

	// ---- social ----
	{"comment", 40, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		permlink := fmt.Sprintf("%s-post-%d", b.name, b.nextSeq())
		parentAuthor, parentPermlink := "", fmt.Sprintf("cat%d", r.Intn(5))
		// Half the time reply to somebody, which exercises the comment tree,
		// depth limits and reply-interval rules rather than just root posts.
		if o := w.otherBot(b, r); o != nil && r.Intn(2) == 0 {
			o.mu.Lock()
			if len(o.comments) > 0 {
				parentAuthor = o.name
				parentPermlink = o.comments[r.Intn(len(o.comments))]
			}
			o.mu.Unlock()
		}
		b.pending = func() {
			b.comments = append(b.comments, permlink)
			if len(b.comments) > 40 {
				b.comments = b.comments[len(b.comments)-40:]
			}
		}
		return []op.Operation{op.Comment{
			ParentAuthor: acct(parentAuthor), ParentPermlink: parentPermlink,
			Author: acct(b.name), Permlink: permlink,
			Title: "t", Body: fmt.Sprintf("body %d", r.Intn(100000)),
			JSONMetadata: `{"app":"gopherd-bots"}`}}
	}},
	{"vote", 45, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		o := w.otherBot(b, r)
		if o == nil {
			return nil
		}
		o.mu.Lock()
		var pl string
		if len(o.comments) > 0 {
			pl = o.comments[r.Intn(len(o.comments))]
		}
		o.mu.Unlock()
		if pl == "" {
			return nil
		}
		wgt := int16(1 + r.Intn(10000))
		if r.Intn(5) == 0 {
			wgt = -wgt // downvotes exercise a separate mana pool
		}
		return []op.Operation{op.Vote{Voter: acct(b.name), Author: acct(o.name), Permlink: pl, Weight: wgt}}
	}},
	{"delete_comment", 5, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.comments) == 0 {
			return nil
		}
		i := r.Intn(len(b.comments))
		pl := b.comments[i]
		b.comments = append(b.comments[:i], b.comments[i+1:]...)
		return []op.Operation{op.DeleteComment{Author: acct(b.name), Permlink: pl}}
	}},
	{"comment_options", 10, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.comments) == 0 {
			return nil
		}
		pl := b.comments[r.Intn(len(b.comments))]
		o := w.otherBot(b, r)
		var bene []op.CommentBeneficiary
		if o != nil && r.Intn(2) == 0 {
			bene = []op.CommentBeneficiary{{Account: acct(o.name), Weight: uint16(1 + r.Intn(5000))}}
		}
		return []op.Operation{op.CommentOptions{Author: acct(b.name), Permlink: pl,
			MaxAcceptedPayout: hbdSat(int64(r.Intn(1_000_000))), PercentHbd: uint16(r.Intn(10001)),
			AllowVotes: true, AllowCurationRewards: true, Beneficiaries: bene}}
	}},
	{"custom_json", 25, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.CustomJSON{
			RequiredPostingAuths: []auth.AccountName{acct(b.name)},
			ID:                   "gopherd_bots",
			JSON:                 fmt.Sprintf(`{"n":%d,"kind":"noise"}`, r.Intn(100000))}}
	}},
	{"custom", 5, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.Custom{RequiredAuths: []auth.AccountName{acct(b.name)},
			ID: uint16(r.Intn(100)), Data: []byte{byte(r.Intn(256)), byte(r.Intn(256))}}}
	}},

	// ---- account management ----
	{"account_update", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.AccountUpdate{Account: acct(b.name), MemoKey: b.pub,
			JSONMetadata: fmt.Sprintf(`{"v":%d}`, r.Intn(1000))}}
	}},
	{"account_update2", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.AccountUpdate2{Account: acct(b.name),
			JSONMetadata:        fmt.Sprintf(`{"v2":%d}`, r.Intn(1000)),
			PostingJSONMetadata: `{"profile":{"name":"bot"}}`, Extensions: []byte{0x00}}}
	}},
	{"claim_account", 5, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		// Same trap as the comment permlinks: crediting the ticket before the
		// chain accepts the claim made create_claimed_account spend one that was
		// never granted, failing pending_claimed_accounts > 0.
		b.pending = func() { b.tickets++ }
		return []op.Operation{op.ClaimAccount{Creator: acct(b.name), Fee: hiveSat(0), Extensions: []byte{0x00}}}
	}},
	{"create_claimed_account", 5, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if b.tickets <= 0 {
			return nil
		}
		name := fmt.Sprintf("%s-sub%d", b.name, b.nextSeq())
		b.pending = func() {
			b.tickets--
			b.subAccounts = append(b.subAccounts, name)
		}
		a := authOf(b.pub)
		return []op.Operation{op.CreateClaimedAccount{Creator: acct(b.name), NewAccountName: acct(name),
			Owner: a, Active: a, Posting: a, MemoKey: b.pub, JSONMetadata: "", Extensions: []byte{0x00}}}
	}},
	{"account_create", 4, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		name := fmt.Sprintf("%s-ac%d", b.name, b.nextSeq())
		b.pending = func() { b.subAccounts = append(b.subAccounts, name) }
		a := authOf(b.pub)
		return []op.Operation{op.AccountCreate{Fee: hiveSat(0), Creator: acct(b.name), NewAccountName: acct(name),
			Owner: a, Active: a, Posting: a, MemoKey: b.pub}}
	}},
	{"change_recovery_account", 3, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		o := w.otherBot(b, r)
		if o == nil {
			return nil
		}
		return []op.Operation{op.ChangeRecoveryAccount{AccountToRecover: acct(b.name),
			NewRecoveryAccount: acct(o.name), Extensions: []byte{0x00}}}
	}},
	{"request_account_recovery", 3, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.subAccounts) == 0 {
			return nil
		}
		target := b.subAccounts[r.Intn(len(b.subAccounts))]
		return []op.Operation{op.RequestAccountRecovery{RecoveryAccount: acct(b.name),
			AccountToRecover: acct(target), NewOwnerAuthority: authOf(b.pub), Extensions: []byte{0x00}}}
	}},

	// ---- governance ----
	{"account_witness_vote", 8, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(w.witnesses) == 0 {
			return nil
		}
		return []op.Operation{op.AccountWitnessVote{Account: acct(b.name),
			Witness: acct(w.witnesses[r.Intn(len(w.witnesses))]), Approve: r.Intn(4) != 0}}
	}},
	{"account_witness_proxy", 3, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		o := w.otherBot(b, r)
		if o == nil {
			return nil
		}
		proxy := o.name
		if r.Intn(3) == 0 {
			proxy = "" // clearing the proxy is its own code path
		}
		return []op.Operation{op.AccountWitnessProxy{Account: acct(b.name), Proxy: acct(proxy)}}
	}},
	{"witness_update", 4, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		b.isWitness = true
		return []op.Operation{op.WitnessUpdate{Owner: acct(b.name),
			URL: "https://gopherd.test/" + b.name, BlockSigningKey: b.pub,
			Props: op.LegacyChainProperties{
				AccountCreationFee: asset.LegacyHiveAsset{Amount: 0, Sym: asset.LegacyHiveSymBytes()},
				MaximumBlockSize:   uint32(65536 + r.Intn(60000)), HBDInterestRate: uint16(r.Intn(2000))},
			Fee: hiveSat(0)}}
	}},
	{"feed_publish", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if !b.isWitness {
			return nil
		}
		return []op.Operation{op.FeedPublish{Publisher: acct(b.name),
			ExchangeRate: asset.Price{Base: hbdSat(int64(900 + r.Intn(200))), Quote: hiveSat(1000)}}}
	}},
	{"create_proposal", 4, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.comments) == 0 {
			return nil
		}
		start := nowSec() + 60
		return []op.Operation{op.CreateProposal{Creator: acct(b.name), Receiver: acct(b.name),
			StartDate: start, EndDate: start + uint32(86400*(1+r.Intn(10))),
			DailyPay: hbdSat(int64(1000 + r.Intn(10000))), Subject: "bot proposal",
			Permlink: b.comments[r.Intn(len(b.comments))], Extensions: []byte{0x00}}}
	}},
	{"update_proposal_votes", 5, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.UpdateProposalVotes{Voter: acct(b.name),
			ProposalIDs: []int64{int64(r.Intn(20))}, Approve: r.Intn(2) == 0, Extensions: []byte{0x00}}}
	}},
	{"update_proposal", 3, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.comments) == 0 {
			return nil
		}
		return []op.Operation{op.UpdateProposal{ProposalID: int64(r.Intn(20)), Creator: acct(b.name),
			DailyPay: hbdSat(int64(500 + r.Intn(5000))), Subject: "updated",
			Permlink: b.comments[r.Intn(len(b.comments))], Extensions: []byte{0x00}}}
	}},
	{"remove_proposal", 2, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		return []op.Operation{op.RemoveProposal{ProposalOwner: acct(b.name),
			ProposalIDs: []int64{int64(r.Intn(20))}, Extensions: []byte{0x00}}}
	}},

	// ---- escrow ----
	{"escrow_transfer", 6, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		to := w.otherBot(b, r)
		agent := w.otherBot(b, r)
		if to == nil || agent == nil || to.name == agent.name {
			return nil
		}
		id := b.nextSeq()
		ref := escrowRef{from: b.name, to: to.name, agent: agent.name, id: id}
		b.pending = func() { b.escrows = append(b.escrows, ref) }
		return []op.Operation{op.EscrowTransfer{From: acct(b.name), To: acct(to.name), Agent: acct(agent.name),
			EscrowID: id, HbdAmount: hbdSat(int64(r.Intn(500))), HiveAmount: hiveSat(int64(100 + r.Intn(1000))),
			Fee:                  hiveSat(int64(1 + r.Intn(50))),
			RatificationDeadline: nowSec() + 3600, EscrowExpiration: nowSec() + 7200, JSONMeta: "{}"}}
	}},
	{"escrow_approve", 5, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.escrows) == 0 {
			return nil
		}
		e := b.escrows[r.Intn(len(b.escrows))]
		return []op.Operation{op.EscrowApprove{From: acct(e.from), To: acct(e.to), Agent: acct(e.agent),
			Who: acct(b.name), EscrowID: e.id, Approve: true}}
	}},
	{"escrow_dispute", 3, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		// Requires to_approved && agent_approved; without that the chain refuses
		// it every time and the row says nothing.
		if len(b.escrows) == 0 {
			return nil
		}
		e := b.escrows[r.Intn(len(b.escrows))]
		return []op.Operation{op.EscrowDispute{From: acct(e.from), To: acct(e.to), Agent: acct(e.agent),
			Who: acct(b.name), EscrowID: e.id}}
	}},
	{"escrow_release", 3, func(b *bot, w *world, r *rand.Rand) []op.Operation {
		if len(b.escrows) == 0 {
			return nil
		}
		e := b.escrows[r.Intn(len(b.escrows))]
		return []op.Operation{op.EscrowRelease{From: acct(e.from), To: acct(e.to), Agent: acct(e.agent),
			Who: acct(b.name), Receiver: acct(e.to), EscrowID: e.id,
			HbdAmount: hbdSat(0), HiveAmount: hiveSat(int64(1 + r.Intn(100)))}}
	}},
}
