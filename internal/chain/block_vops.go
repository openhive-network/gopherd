package chain

import (
	"gopherd/internal/chain/evaluator"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
)

// BlockVOp captures the small set of virtual operations the Go chain
// currently records during ApplyBlock so tests can assert against them
// (e.g. the recurrent-transfer parity test that inspects pair_id on the
// fill_recurrent_transfer / failed_recurrent_transfer vops).
//
// Hived's full virtual-op pipeline (push_virtual_operation +
// post_push_virtual_operation + account_history_plugin) is not yet
// ported. This struct is intentionally lightweight — a tagged union
// over the few vops the test suite needs to observe. Buffer is cleared
// at the top of every ApplyBlock and exposed via LastBlockVOps().
//
// MISSING, and worth knowing before chasing an upstream change that touches
// one: every other virtual operation. author_reward, curation_reward,
// comment_reward, producer_reward, fill_vesting_withdraw, interest,
// fill_convert_request, expired_account_notification,
// declined_voting_rights, effective_comment_vote, hardfork — none are
// emitted. So a hived commit that only alters a virtual operation's PAYLOAD
// is a no-op here: hive e3b8a39d5, for instance, changed
// author_reward_operation's curators_vesting_payout from a single conversion
// at the post-payout price to the exact sum actually paid, and there is
// nothing in gopherd for it to change.
//
// This is not a small gap. It is what a HAF-style block+vop stream needs, and
// closing it means a real push_virtual_operation with hived's exact emission
// ORDER, not just the payloads.
type BlockVOpKind uint8

const (
	BlockVOpFillRecurrentTransfer BlockVOpKind = iota + 1
	BlockVOpFailedRecurrentTransfer
	BlockVOpProposalFee
	BlockVOpProxyCleared
)

// BlockVOp is the union record. Only the fields relevant to the kind
// are populated. For *RecurrentTransfer vops:
//
//	From, To, Memo, Amount, RemainingExecutions, PairID are always set.
//	ConsecutiveFailures + Deleted are set on Failed*.
//
// For ProposalFee:
//
//	Creator, Treasury, ProposalID, Fee.
//
// For ProxyCleared:
//
//	Account, Proxy (the old proxy that was cleared).
//
// Mirrors hived's:
//
//	fill_recurrent_transfer_operation(from, to, amount, memo, remaining_executions)
//	failed_recurrent_transfer_operation(from, to, amount, consecutive_failures, memo, remaining_executions, deleted)
//	proposal_fee_operation(creator, treasury, proposal_id, fee)
//	proxy_cleared_operation(account, proxy)
type BlockVOp struct {
	Kind                BlockVOpKind
	From                auth.AccountName
	To                  auth.AccountName
	Amount              asset.Asset
	Memo                string
	RemainingExecutions uint16
	ConsecutiveFailures uint8
	Deleted             bool
	PairID              uint8

	// ProposalFee
	Creator    auth.AccountName
	Treasury   auth.AccountName
	ProposalID int64
	Fee        int64

	// ProxyCleared
	Account auth.AccountName
	Proxy   auth.AccountName
}

// LastBlockVOps returns a copy of the virtual ops emitted during the
// most recent successful ApplyBlock. Safe to call concurrently with
// other reads — takes the same RLock as the other read-side accessors.
func (d *Database) LastBlockVOps() []BlockVOp {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	if len(d.vops) == 0 {
		return nil
	}
	out := make([]BlockVOp, len(d.vops))
	copy(out, d.vops)
	return out
}

// pushVOp appends a virtual op to the per-block buffer. Caller must
// hold the write lock (ApplyBlock holds it for the entire block).
func (d *Database) pushVOp(v BlockVOp) {
	d.vops = append(d.vops, v)
}

// resetVOps truncates the per-block vop buffer at the top of ApplyBlock.
func (d *Database) resetVOps() {
	d.vops = d.vops[:0]
}

// emitVOpClosure returns the function the evaluator package's Context
// invokes when an op wants to push a virtual op. It type-asserts the
// payload (currently evaluator.ProposalFeeVOp / evaluator.ProxyClearedVOp)
// into the chain.BlockVOp union and appends.
func (d *Database) emitVOpClosure() func(any) {
	return func(payload any) {
		switch p := payload.(type) {
		case evaluator.ProposalFeeVOp:
			d.pushVOp(BlockVOp{
				Kind:       BlockVOpProposalFee,
				Creator:    auth.AccountName(p.Creator),
				Treasury:   auth.AccountName(p.Treasury),
				ProposalID: p.ProposalID,
				Fee:        p.Fee,
			})
		case evaluator.ProxyClearedVOp:
			d.pushVOp(BlockVOp{
				Kind:    BlockVOpProxyCleared,
				Account: auth.AccountName(p.Account),
				Proxy:   auth.AccountName(p.Proxy),
			})
		}
	}
}
