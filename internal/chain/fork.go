package chain

import (
	"fmt"
	"gopherd/internal/crypto"

	"gopherd/internal/protocol/block"
)

// PushOutcome reports the net effect of a PushLiveBlock call.
type PushOutcome int

const (
	PushBuffered  PushOutcome = iota // stored in the DAG; head did not move
	PushExtended                     // head advanced by ≥1 block on the current branch
	PushReorged                      // switched to a strictly-longer competing branch
	PushDuplicate                    // block id already known
	PushStale                        // at/below the committed (irreversible) head; ignored
	PushTooDeep                      // a longer branch exists but forks below the committed
	// head — beyond the single-block reversible window; left buffered
)

func (o PushOutcome) String() string {
	switch o {
	case PushBuffered:
		return "buffered"
	case PushExtended:
		return "extended"
	case PushReorged:
		return "reorged"
	case PushDuplicate:
		return "duplicate"
	case PushStale:
		return "stale"
	case PushTooDeep:
		return "too-deep"
	}
	return "?"
}

// PushLiveBlock is the fork-aware entry point for live (tip) blocks. Unlike
// ApplyBlock — which strictly requires head+1 and rejects anything else — it
// records blocks in an id-keyed fork DAG and drives the fork choice:
//   - extends the head with the block (and any already-buffered children), OR
//   - buffers an out-of-order / competing block, OR
//   - on a strictly-longer branch that forks at the COMMITTED head, UNDOes the
//     uncommitted tip and re-applies the winning branch (a depth-1 tip reorg).
//
// Model (1-block commit lag): the node's single tip block is kept UNCOMMITTED
// (reversible); the block below it is the committed head. Reorgs that would need
// to undo a committed block return PushTooDeep and leave the chain untouched
// (those need a multi-block reversible window — future work); tip micro-forks,
// which dominate, are depth-1 and handled here.
//
// MUST be called from a single goroutine (like ApplyBlock). opts is applied to
// every ApplyBlock it drives, with Reversible forced on.
func (d *Database) PushLiveBlock(raw []byte, num uint32, opts ApplyOptions) (PushOutcome, error) {
	d.ensureForkDB()
	// Bound the DAG on every path out of here. Without this the fork database
	// grew without limit: add() retains each block's RAW bytes and prune() had
	// no production caller at all, so a node held every block it ever received
	// (~28.8k/day). That is what made zstd's decompressed buffers top every
	// heap diff -- they were not leaking, they were PINNED here.
	defer d.pruneForkDB()

	sh, hdrLen, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		return 0, fmt.Errorf("push_live_block %d: header: %w", num, err)
	}
	id := block.ConstructID(raw[:hdrLen], num)
	if _, dup := d.fork.get(id); dup {
		return PushDuplicate, nil
	}
	if num <= d.committedNum() {
		// At/below the irreversible head: record for topology, can't act on it.
		d.fork.add(id, sh.Previous, num, raw)
		return PushStale, nil
	}
	d.fork.add(id, sh.Previous, num, raw)

	opts.Reversible = true
	moved := false

	// 1) Extend the head forward as far as buffered blocks allow.
	ext, err := d.drainForward(opts, id)
	if err != nil {
		return 0, err
	}
	moved = moved || ext

	// 2) If a strictly-longer branch now forks at the committed head, reorg to
	//    it, then keep draining on the new branch.
	reorged, err := d.tryReorg(opts)
	if err != nil {
		return 0, err
	}
	if reorged {
		if _, err := d.drainForward(opts, id); err != nil {
			return 0, err
		}
		return PushReorged, nil
	}
	if moved {
		return PushExtended, nil
	}
	// Nothing applied: either buffered (waiting for a parent) or a longer branch
	// forks too deep to switch to with a 1-block window.
	if best := d.fork.longestKnownTip(); best != nil && best.num > d.effHeadNum() {
		if anc, ok := d.fork.commonAncestor(d.effHeadID(), best.id); ok && anc != d.committedID() {
			return PushTooDeep, nil
		}
	}
	return PushBuffered, nil
}

// forkKeepBelowLIB is how much history the DAG keeps BELOW the last
// irreversible block. Nothing at or under the LIB can take part in a reorg, so
// consensus needs none of it; the margin exists purely so a stale block that
// is re-offered still hits the get()/PushDuplicate fast path instead of being
// re-decoded every time a peer replays it. ~3 minutes of chain.
const forkKeepBelowLIB = 64

// pruneForkDB drops DAG nodes below the irreversible horizon. The sweep is
// O(len(byID)), so it is skipped unless the horizon actually advanced.
func (d *Database) pruneForkDB() {
	if d.fork == nil {
		return
	}
	lib := d.committedNum()
	if lib <= forkKeepBelowLIB {
		return // early chain: nothing safe to drop (and avoids uint32 underflow)
	}
	horizon := lib - forkKeepBelowLIB
	if horizon <= d.forkPrunedBelow {
		return
	}
	d.fork.prune(horizon)
	d.forkPrunedBelow = horizon
}

// ForkDBLen reports how many blocks the fork DAG holds. Exported for the
// periodic STATS line: this number growing without bound is precisely the
// failure that went unnoticed, so it is worth watching.
func (d *Database) ForkDBLen() int {
	if d.fork == nil {
		return 0
	}
	return d.fork.len()
}

// ensureForkDB lazily creates the DAG, seeding it with the current committed
// head so the first inbound block can link to it.
func (d *Database) ensureForkDB() {
	if d.fork != nil {
		return
	}
	d.fork = newForkDB()
	// Seed with the current committed head so the first inbound block links to
	// it — even at genesis (head id zero), so a real block whose previous is the
	// zero id is recognised as a linear extension rather than a phantom reorg.
	hid, _ := d.HeadBlockID()
	hn, _ := d.HeadBlock()
	d.fork.add(hid, block.ID{}, hn, nil)
	d.fork.setHead(hid)
}

// effHeadID/effHeadNum are the CURRENT tip (the uncommitted reversible block if
// one is pending, else the committed head) — HeadBlock*/ID read through the
// session so they already reflect the pending block.
func (d *Database) effHeadID() block.ID { id, _ := d.HeadBlockID(); return id }
func (d *Database) effHeadNum() uint32  { n, _ := d.HeadBlock(); return n }

// committedID/committedNum are the last IRREVERSIBLE block — the pending block's
// parent when a reversible block is pending, else the effective head.
func (d *Database) committedID() block.ID {
	if _, _, prev, ok := d.ReversiblePending(); ok {
		return prev
	}
	return d.effHeadID()
}
func (d *Database) committedNum() uint32 {
	if n, _, _, ok := d.ReversiblePending(); ok {
		return n - 1
	}
	return d.effHeadNum()
}

// drainForward applies buffered blocks that extend the current head, one at a
// time (finalizing the prior tip so only the newest stays reversible), until no
// buffered child of the head exists. Returns whether it advanced the head.
func (d *Database) drainForward(opts ApplyOptions, pushed block.ID) (bool, error) {
	moved := false
	for {
		head := d.effHeadID()
		child := d.childOf(head)
		if child == nil {
			return moved, nil
		}
		// The verification hints belong to `pushed` alone; any other block must
		// be verified from its own bytes.
		use := opts
		if child.id != pushed {
			use = optsForOther(opts)
		}
		if err := d.applyReversibleExtend(child, use); err != nil {
			return moved, err
		}
		moved = true
	}
}

// childOf returns a buffered node whose parent is head and whose height is
// head+1 (a linear extension), or nil.
func (d *Database) childOf(head block.ID) *forkNode {
	hn, ok := d.fork.get(head)
	if !ok {
		return nil
	}
	for _, n := range d.fork.byID {
		if n.prev == head && n.num == hn.num+1 && n.raw != nil {
			return n
		}
	}
	return nil
}

// optsForOther returns opts with the PER-BLOCK verification hints removed.
//
// PreVerifiedBlockSigner and PreRecoveredTxKeys are computed for ONE specific
// block on the receive path. Applying a DIFFERENT block with them verifies that
// block's signature against another block's recovered key, which fails at
// random and blamed the wrong witness:
//
//	push_live_block 108915165 failed: apply 108915156:
//	  signing key mismatch for "ocd-witness"
//	  (recovered=<changed on every attempt>)
//
// The recovered key differed each time precisely because a different pushing
// block supplied it. Every buffered (out-of-order) block and every block
// re-applied during a reorg went through this path, so valid blocks were
// rejected at the tip whenever arrival order slipped.
func optsForOther(opts ApplyOptions) ApplyOptions {
	opts.PreVerifiedBlockSigner = crypto.PublicKey{}
	opts.PreRecoveredTxKeys = nil
	return opts
}

// applyReversibleExtend finalizes the current uncommitted tip (if any) and
// applies node reversibly, making it the new tip.
func (d *Database) applyReversibleExtend(node *forkNode, opts ApplyOptions) error {
	if _, _, _, hasPend := d.ReversiblePending(); hasPend {
		if err := d.FinalizeReversible(false); err != nil {
			return fmt.Errorf("push_live_block: finalize tip: %w", err)
		}
	}
	opts.Reversible = true
	if _, err := d.ApplyBlock(node.raw, node.num, opts, nil); err != nil {
		return fmt.Errorf("push_live_block: apply %d: %w", node.num, err)
	}
	d.fork.setHead(node.id)
	return nil
}

// tryReorg switches to the longest known branch when it strictly outgrows the
// current tip AND forks at the committed head (depth-1 reach). Returns whether
// it reorged.
func (d *Database) tryReorg(opts ApplyOptions) (bool, error) {
	best := d.fork.longestKnownTip()
	effID, effNum := d.effHeadID(), d.effHeadNum()
	if best == nil || best.id == effID || best.num <= effNum {
		return false, nil
	}
	committed := d.committedID()
	anc, ok := d.fork.commonAncestor(effID, best.id)
	if !ok || anc != committed {
		return false, nil // not resolvable, or forks below the committed head
	}
	branch, ok := d.fork.branchFrom(committed, best.id)
	if !ok {
		return false, nil // winning branch not fully present yet
	}
	// Undo the uncommitted tip back to the committed common ancestor, then apply
	// the winning branch (finalize all but the last so only the new tip stays
	// reversible).
	if _, _, _, hasPend := d.ReversiblePending(); hasPend {
		d.UndoReversible()
	}
	opts.Reversible = true
	for i, bn := range branch {
		if bn.raw == nil {
			return false, fmt.Errorf("push_live_block: reorg branch %d missing raw", bn.num)
		}
		if _, err := d.ApplyBlock(bn.raw, bn.num, optsForOther(opts), nil); err != nil {
			d.UndoReversible()
			return false, fmt.Errorf("push_live_block: reorg apply %d: %w", bn.num, err)
		}
		if i < len(branch)-1 {
			if err := d.FinalizeReversible(false); err != nil {
				return false, fmt.Errorf("push_live_block: reorg finalize %d: %w", bn.num, err)
			}
		}
	}
	d.fork.setHead(best.id)
	return true, nil
}
