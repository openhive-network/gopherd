package node

import "hived-go/internal/p2p/proto"

// MultiDelegate fans inbound peer events out to a fixed slice of delegates,
// in registration order. Each child delegate is invoked for every event,
// regardless of whether earlier children "consumed" it — message handling
// in this protocol is dispatch-by-tag, so multiple subsystems can safely
// each route their own message types and ignore the rest.
//
// Used at the daemon level to wire sync + gossip + discovery onto the same
// *Node without forcing them into a single concrete implementation.
//
// nil children are skipped, which lets the daemon optionally enable a
// subsystem (or test in isolation) without re-plumbing the wiring.
type MultiDelegate struct {
	children []Delegate
}

// NewMultiDelegate builds a fan-out delegate over the given children.
func NewMultiDelegate(children ...Delegate) *MultiDelegate {
	return &MultiDelegate{children: children}
}

// Add appends another child delegate. Safe to call only before Start —
// the children slice is not protected for concurrent mutation.
func (m *MultiDelegate) Add(d Delegate) {
	if d != nil {
		m.children = append(m.children, d)
	}
}

// OnPeerConnected dispatches to every child.
func (m *MultiDelegate) OnPeerConnected(p *Peer) {
	for _, c := range m.children {
		if c != nil {
			c.OnPeerConnected(p)
		}
	}
}

// OnPeerDisconnected dispatches to every child.
func (m *MultiDelegate) OnPeerDisconnected(p *Peer, cause error) {
	for _, c := range m.children {
		if c != nil {
			c.OnPeerDisconnected(p, cause)
		}
	}
}

// OnMessage dispatches to every child.
func (m *MultiDelegate) OnMessage(p *Peer, t proto.MessageType, body []byte) {
	for _, c := range m.children {
		if c != nil {
			c.OnMessage(p, t, body)
		}
	}
}

// OnBlockReceived dispatches to every child.
func (m *MultiDelegate) OnBlockReceived(p *Peer, body []byte) {
	for _, c := range m.children {
		if c != nil {
			c.OnBlockReceived(p, body)
		}
	}
}

// OnTransactionReceived dispatches to every child.
func (m *MultiDelegate) OnTransactionReceived(p *Peer, body []byte) {
	for _, c := range m.children {
		if c != nil {
			c.OnTransactionReceived(p, body)
		}
	}
}
