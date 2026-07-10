package chain

import (
	"fmt"

	"hived-go/internal/protocol/block"
)

// PushOutcome reports what PushLiveBlock did with a block.
type PushOutcome int

const (
	PushExtended  PushOutcome = iota // linear apply on top of the current head
	PushBuffered                     // a fork block stored; current head still best
	PushReorged                      // switched to a strictly-longer competing branch
	PushDuplicate                    // block id already known
	PushStale                        // at/below the committed (irreversible) head; ignored
	PushTooDeep                      // reorg would need to undo a committed block (beyond the
	// single-block reversible window); left buffered, head unchanged
)

func (o PushOutcome) String() string {
	switch o {
	case PushExtended:
		return "extended"
	case PushBuffered:
		return "buffered"
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
// tracks recent blocks in a fork DAG and, on a strictly-longer competing branch
// that forks at the committed head, UNDOes the node's uncommitted tip block and
// re-applies the winning branch (a depth-1 reorg, the common tip micro-fork).
//
// Model (1-block commit lag): the node's single tip block is kept UNCOMMITTED
// (reversible); the block below it is the committed head. A block that extends
// the tip finalizes the old tip and applies the new one reversibly; a competing
// branch that outgrows the tip is switched to by undoing the uncommitted tip.
//
// Reorgs deeper than the committed head (which would require undoing an already-
// committed block) return PushTooDeep and leave the chain untouched — those need
// a multi-block reversible window (future work). Micro-forks at the tip, which
// dominate in practice, are depth-1 and handled here.
//
// MUST be called from a single goroutine (like ApplyBlock). opts is applied to
// every ApplyBlock this drives, with Reversible forced on.
func (d *Database) PushLiveBlock(raw []byte, num uint32, opts ApplyOptions) (PushOutcome, error) {
	if d.fork == nil {
		d.fork = newForkDB()
		if hid, err := d.HeadBlockID(); err == nil && hid != (block.ID{}) {
			hn, _ := d.HeadBlock()
			d.fork.add(hid, block.ID{}, hn, nil)
			d.fork.setHead(hid)
		}
	}

	sh, hdrLen, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		return 0, fmt.Errorf("push_live_block %d: header: %w", num, err)
	}
	id := block.ConstructID(raw[:hdrLen], num)
	prev := sh.Previous

	if _, dup := d.fork.get(id); dup {
		return PushDuplicate, nil
	}

	// HeadBlock/HeadBlockID read THROUGH the session overlay, so they reflect
	// the uncommitted pending (tip) block, not the committed head. The
	// EFFECTIVE head is therefore what they return; the COMMITTED head is the
	// pending block's parent (pendPrev) when a reversible block is pending.
	effHeadNum, _ := d.HeadBlock()
	effHeadID, _ := d.HeadBlockID()
	pendNum, _, pendPrev, hasPend := d.ReversiblePending()
	committedID, committedNum := effHeadID, effHeadNum
	if hasPend {
		committedID, committedNum = pendPrev, pendNum-1
	}

	// At or below the committed (irreversible) head — a depth-1 window cannot
	// undo committed blocks, so record it for topology and ignore.
	if num <= committedNum {
		d.fork.add(id, prev, num, raw)
		return PushStale, nil
	}

	opts.Reversible = true

	// Case A — linear extension of the current (possibly uncommitted) head.
	if prev == effHeadID {
		if hasPend {
			// The old tip is now confirmed (a child builds on it): commit it.
			if err := d.FinalizeReversible(false); err != nil {
				return 0, fmt.Errorf("push_live_block %d: finalize prev tip: %w", num, err)
			}
		}
		if _, err := d.ApplyBlock(raw, num, opts, nil); err != nil {
			return 0, err
		}
		d.fork.add(id, prev, num, raw)
		d.fork.setHead(id)
		return PushExtended, nil
	}

	// Case B — a fork block. Buffer it; reorg only if a strictly-longer branch
	// now exists AND it forks at the committed head (depth-1 reach).
	d.fork.add(id, prev, num, raw)
	best := d.fork.longestKnownTip()
	if best == nil || best.id == effHeadID || best.num <= effHeadNum {
		return PushBuffered, nil
	}
	anc, ok := d.fork.commonAncestor(effHeadID, best.id)
	if !ok {
		return PushBuffered, nil // ancestor not resolvable yet (missing blocks)
	}
	if anc != committedID {
		// Fork point is above the committed head is fine; below it we can't undo.
		return PushTooDeep, nil
	}
	branch, ok := d.fork.branchFrom(committedID, best.id)
	if !ok {
		return PushBuffered, nil // winning branch not fully present yet
	}

	// Switch: undo our uncommitted tip back to the committed common ancestor,
	// then apply the winning branch. Finalize all but the last so only the new
	// tip stays reversible (preserving the depth-1 invariant).
	if hasPend {
		d.UndoReversible()
	}
	for i, bn := range branch {
		if bn.raw == nil {
			return 0, fmt.Errorf("push_live_block: reorg branch block %d missing raw", bn.num)
		}
		if _, err := d.ApplyBlock(bn.raw, bn.num, opts, nil); err != nil {
			d.UndoReversible() // drop the partial; committed head stays the ancestor
			return 0, fmt.Errorf("push_live_block: reorg apply %d: %w", bn.num, err)
		}
		if i < len(branch)-1 {
			if err := d.FinalizeReversible(false); err != nil {
				return 0, fmt.Errorf("push_live_block: reorg finalize %d: %w", bn.num, err)
			}
		}
	}
	d.fork.setHead(best.id)
	return PushReorged, nil
}
