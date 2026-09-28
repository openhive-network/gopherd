package chain

import (
	"gopherd/internal/chain/evaluator"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
)

// The block's virtual-operation stream.
//
// Virtual ops are not in any transaction — evaluators and per-block hooks
// construct them to explain what the chain DID: who was paid, what expired,
// what was swept. They are the bulk of what an account-history or HAF consumer
// reads, and they are the reason a node can answer "why did my balance change"
// without re-deriving the chain.
//
// This used to be a flat struct with a Kind tag and a union of every field any
// of four ops needed. That does not survive contact with hived's forty-three:
// the struct becomes a bag of optional fields, nothing type-checks, and there
// is no wire encoding at the end of it. The buffer now holds op.Operation —
// the same interface regular operations implement — so each vop is its own
// typed struct and serializes through the identical varint(tag) || body
// framing. Virtual tags continue the operation static_variant's numbering at
// 50, which is what makes the stream readable by anything that already speaks
// hived's encoding.
//
// ORDER IS PART OF THE CONTRACT. hived emits these at specific points, and a
// consumer reconstructing balances by replaying the stream gets a different
// answer if two ops swap. Appending in emission order is the whole mechanism;
// there is no sorting step, and there must not be one.
//
// STILL MISSING: 19 of the 43. The ones defined and wired here are those whose
// emission point already exists in gopherd. The rest need their emission site
// built as well as their payload — comment cashout's author/curation/benefactor
// trio, the hardfork ops, effective_comment_vote, and the escrow pair among
// them. op.Tag names all 43, so the gap is visible from the tag list.

// LastBlockVOps returns the virtual ops emitted during the most recent
// successful ApplyBlock, in emission order. Safe to call concurrently with
// other reads — takes the same RLock as the other read-side accessors.
func (d *Database) LastBlockVOps() []op.Operation {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	if len(d.vops) == 0 {
		return nil
	}
	out := make([]op.Operation, len(d.vops))
	copy(out, d.vops)
	return out
}

// LastBlockVOpsEncoded returns the same stream in wire form: each op as
// varint(tag) || body, concatenated. This is the form a block+vop consumer
// wants, and encoding here rather than at the edge keeps the tag mapping in
// one place.
func (d *Database) LastBlockVOpsEncoded() [][]byte {
	vops := d.LastBlockVOps()
	if len(vops) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(vops))
	for _, v := range vops {
		out = append(out, op.AppendOperation(nil, v))
	}
	return out
}

// pushVOp appends a virtual op to the per-block buffer. Caller must hold the
// write lock (ApplyBlock holds it for the entire block).
func (d *Database) pushVOp(v op.Operation) {
	d.vops = append(d.vops, v)
}

// resetVOps truncates the per-block vop buffer at the top of ApplyBlock.
func (d *Database) resetVOps() {
	d.vops = d.vops[:0]
}

// FindVOp returns the first virtual op in the stream with the given tag, or
// nil. For tests and diagnostics; consumers wanting the whole stream should
// range over LastBlockVOps and keep the order.
func FindVOp(vops []op.Operation, tag op.Tag) op.Operation {
	for _, v := range vops {
		if v.OpTag() == tag {
			return v
		}
	}
	return nil
}

// CountVOps returns how many ops in the stream carry the given tag.
func CountVOps(vops []op.Operation, tag op.Tag) int {
	n := 0
	for _, v := range vops {
		if v.OpTag() == tag {
			n++
		}
	}
	return n
}

// emitVOpClosure returns the function the evaluator package's Context invokes
// when an op wants to push a virtual op. The evaluator package cannot import
// chain, so it hands over its own payload types and this translates them.
func (d *Database) emitVOpClosure() func(any) {
	return func(payload any) {
		switch p := payload.(type) {
		case evaluator.ProposalFeeVOp:
			d.pushVOp(op.ProposalFee{
				Creator:    auth.AccountName(p.Creator),
				Treasury:   auth.AccountName(p.Treasury),
				ProposalID: uint32(p.ProposalID),
				Fee: asset.Asset{
					Amount: p.Fee,
					Symbol: asset.Symbol{Num: asset.HbdSymbol},
				},
			})
		case evaluator.ProxyClearedVOp:
			d.pushVOp(op.ProxyCleared{
				Account: auth.AccountName(p.Account),
				Proxy:   auth.AccountName(p.Proxy),
			})
		}
	}
}
