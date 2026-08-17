package gossip

import (
	"container/list"
	"sync"

	"gopherd/internal/protocol/block"
)

// seenSet is a concurrent FIFO-LRU bounded set of 20-byte IDs. Used to
// dedup gossip blocks and transactions so we don't apply / echo the
// same item twice.
//
// Insertion ordering is FIFO (oldest first); we evict from the head of
// the list when capacity is exceeded. There's no per-item access bump,
// so it's not a true LRU, but for our purpose ("did we see this block
// in roughly the last 4096 blocks?") FIFO is plenty and avoids list
// surgery on the hot read path.
type seenSet struct {
	mu  sync.Mutex
	cap int

	order *list.List // *list.Element values are block.ID
	idx   map[block.ID]*list.Element
}

func newSeenSet(capacity int) *seenSet {
	if capacity <= 0 {
		capacity = 4096
	}
	return &seenSet{
		cap:   capacity,
		order: list.New(),
		idx:   make(map[block.ID]*list.Element, capacity),
	}
}

// add inserts id into the set. Returns true if id was newly added,
// false if it was already present (caller dedup-rejects).
func (s *seenSet) add(id block.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idx[id]; ok {
		return false
	}
	el := s.order.PushBack(id)
	s.idx[id] = el
	if s.order.Len() > s.cap {
		head := s.order.Front()
		if head != nil {
			s.order.Remove(head)
			delete(s.idx, head.Value.(block.ID))
		}
	}
	return true
}

// contains reports whether id is in the set without touching ordering.
func (s *seenSet) contains(id block.ID) bool {
	s.mu.Lock()
	_, ok := s.idx[id]
	s.mu.Unlock()
	return ok
}
