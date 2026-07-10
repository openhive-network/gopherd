package sync

import "hived-go/internal/protocol/block"

// forkNode is one block in the fork DAG: its id, its parent (prev), height, and
// the raw signed_block bytes needed to (re)apply it during a reorg.
type forkNode struct {
	id   block.ID
	prev block.ID
	num  uint32
	raw  []byte
}

// forkDB is an in-memory DAG of recent block headers keyed by block id, used to
// detect and resolve tip forks. It holds the small reversible window near the
// head (blocks below the prune horizon are dropped). NOT safe for concurrent
// use — the applyLoop is the single owner.
//
// It is pure bookkeeping: it never applies blocks or touches chain state. The
// reorg DRIVER (applyLoop) reads longestBranch()/branchFrom() to decide when to
// undo + re-apply, then feeds the raw bytes back through chain.ApplyBlock.
type forkDB struct {
	byID map[block.ID]*forkNode
	// head is the id of the node's currently-applied best block.
	head block.ID
}

func newForkDB() *forkDB {
	return &forkDB{byID: make(map[block.ID]*forkNode, 128)}
}

// add records a block (id, prev, num, raw). Idempotent: a duplicate id keeps the
// first raw seen. Returns the stored node.
func (f *forkDB) add(id, prev block.ID, num uint32, raw []byte) *forkNode {
	if n, ok := f.byID[id]; ok {
		return n
	}
	n := &forkNode{id: id, prev: prev, num: num, raw: raw}
	f.byID[id] = n
	return n
}

func (f *forkDB) get(id block.ID) (*forkNode, bool) { n, ok := f.byID[id]; return n, ok }

// setHead marks id as the node's applied head. id should already be add()ed.
func (f *forkDB) setHead(id block.ID) { f.head = id }

// headNode returns the current head node (nil if head unknown to the DAG).
func (f *forkDB) headNode() *forkNode { return f.byID[f.head] }

// commonAncestor walks both chains back by prev-pointer until they meet, and
// returns the shared ancestor id. Requires every intermediate block on both
// chains (from a and b down to the ancestor) to be present in the DAG; returns
// ok=false if either chain hits an unknown parent before meeting. O(depth).
func (f *forkDB) commonAncestor(a, b block.ID) (block.ID, bool) {
	// Collect a's ancestor set (including a itself) keyed by id.
	seen := make(map[block.ID]struct{}, 64)
	for cur := a; ; {
		seen[cur] = struct{}{}
		n, ok := f.byID[cur]
		if !ok {
			break // a's chain root reached (or gap); still usable as a boundary
		}
		if n.num == 0 {
			break
		}
		cur = n.prev
		if _, dup := seen[cur]; dup {
			break // cycle guard (should never happen with real ids)
		}
	}
	// Walk b upward until we hit something in a's set.
	for cur := b; ; {
		if _, ok := seen[cur]; ok {
			return cur, true
		}
		n, ok := f.byID[cur]
		if !ok || n.num == 0 {
			return block.ID{}, false
		}
		cur = n.prev
	}
}

// branchFrom returns the chain of nodes from ancestor(exclusive) up to tip
// (inclusive), in ASCENDING apply order. Returns ok=false if tip is not
// connected back to ancestor through the DAG (missing intermediate block).
func (f *forkDB) branchFrom(ancestor, tip block.ID) ([]*forkNode, bool) {
	var rev []*forkNode
	for cur := tip; cur != ancestor; {
		n, ok := f.byID[cur]
		if !ok {
			return nil, false
		}
		rev = append(rev, n)
		if n.num == 0 {
			return nil, false // reached genesis without hitting ancestor
		}
		cur = n.prev
	}
	// Reverse into ascending apply order.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, true
}

// longestKnownTip returns the DAG node with the highest block num (ties broken
// by keeping the incumbent head, i.e. only a STRICTLY higher num wins). This is
// the candidate best head; the driver reorgs to it when it differs from head.
func (f *forkDB) longestKnownTip() *forkNode {
	best := f.byID[f.head]
	for _, n := range f.byID {
		switch {
		case best == nil:
			best = n
		case n.num > best.num:
			best = n
		}
	}
	return best
}

// prune drops every node strictly below minNum. Called as the last-irreversible
// block advances so the DAG stays bounded to the reversible window.
func (f *forkDB) prune(minNum uint32) {
	for id, n := range f.byID {
		if n.num < minNum {
			delete(f.byID, id)
		}
	}
}

func (f *forkDB) len() int { return len(f.byID) }
