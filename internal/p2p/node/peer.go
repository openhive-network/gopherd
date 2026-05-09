// Package node implements hived's per-peer connection state machine and
// the top-level "set of peers" the daemon owns. It is responsible for:
//
//   - Sending HelloMessage on every new conn (inbound + outbound)
//   - Waiting for ConnectionAccepted / ConnectionRejected
//   - Heartbeating with current_time_request / current_time_reply
//   - Routing inbound business messages to a delegate (agent-gossip)
//
// The transport layer does the byte-level handshake; this layer does
// the application-level handshake.
package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"hived-go/internal/p2p/proto"
	"hived-go/internal/p2p/transport"
)

// PeerState mirrors the lifecycle of a hived peer_connection.
type PeerState int32

const (
	// StateConnecting — TCP/stcp handshake in progress (transport layer).
	StateConnecting PeerState = iota
	// StateHelloSent — we've sent our HelloMessage and are waiting for a
	// ConnectionAccepted or ConnectionRejected reply.
	StateHelloSent
	// StateHelloReceived — peer's HelloMessage arrived; we've not yet
	// sent our ConnectionAccepted (or rejected). Inbound peers transit
	// here briefly.
	StateHelloReceived
	// StateActive — both sides accepted; normal traffic flows.
	StateActive
	// StateClosed — connection torn down (graceful or error).
	StateClosed
)

// String renders a PeerState for log output.
func (s PeerState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateHelloSent:
		return "hello-sent"
	case StateHelloReceived:
		return "hello-received"
	case StateActive:
		return "active"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("state(%d)", s)
	}
}

// Direction tells whether we initiated the connection or accepted it.
type Direction uint8

const (
	DirectionOutbound Direction = iota
	DirectionInbound
)

// Inbound is true if the peer connected to us.
func (d Direction) Inbound() bool { return d == DirectionInbound }

// HeartbeatInterval is how often we send current_time_request_message
// once the peer is active. hived defaults to 30 s.
var HeartbeatInterval = 30 * time.Second

// HeartbeatTimeout is how long without a heartbeat reply we tolerate
// before dropping the peer. hived's GRAPHENE_NET_PEER_DISCONNECT_TIMEOUT
// is 20 s; we allow longer because our reply path goes through Go's
// scheduler with no real-time guarantees.
var HeartbeatTimeout = 90 * time.Second

// HelloTimeout caps how long we wait for ConnectionAccepted before
// declaring the peer dead. Mirrors GRAPHENE_NET_PEER_HANDSHAKE_INACTIVITY_TIMEOUT
// loosely.
var HelloTimeout = 30 * time.Second

// Delegate is invoked by the peer goroutine for application-level events.
//
// All callbacks run inline on the peer's reader goroutine; do non-trivial
// work asynchronously.
type Delegate interface {
	// OnPeerConnected is fired exactly once per peer, the first time it
	// reaches StateActive.
	OnPeerConnected(*Peer)
	// OnPeerDisconnected is fired exactly once per peer, when it
	// transitions to StateClosed.
	OnPeerDisconnected(*Peer, error)
	// OnMessage is fired for every business message after the peer is
	// active (i.e. NOT for hello/connection_accepted/heartbeat messages
	// which the peer state machine consumes itself).
	OnMessage(*Peer, proto.MessageType, []byte)
}

// Peer is one peering session. Cheap; holds no state larger than the
// underlying *transport.Conn plus a few channels.
type Peer struct {
	conn      *transport.Conn
	direction Direction

	state atomic.Int32 // PeerState

	helloPayload []byte // local HelloMessage body (built once at New time)

	mu              sync.Mutex
	lastHeartbeatRX time.Time

	closeOnce sync.Once
	closeErr  atomic.Value // error
	doneCh    chan struct{}
}

// newPeer wraps a fresh transport.Conn. helloPayload is the FC-encoded
// HelloMessage body to send on connect (the node layer builds it).
func newPeer(c *transport.Conn, dir Direction, helloPayload []byte) *Peer {
	p := &Peer{
		conn:         c,
		direction:    dir,
		helloPayload: helloPayload,
		doneCh:       make(chan struct{}),
	}
	p.state.Store(int32(StateConnecting))
	return p
}

// State returns the current state atomically.
func (p *Peer) State() PeerState { return PeerState(p.state.Load()) }

// setState transitions atomically.
func (p *Peer) setState(s PeerState) { p.state.Store(int32(s)) }

// Direction returns inbound / outbound.
func (p *Peer) Direction() Direction { return p.direction }

// Conn exposes the underlying transport for callers that need to inspect
// the remote pubkey or addr.
func (p *Peer) Conn() *transport.Conn { return p.conn }

// Send forwards to the underlying transport. Safe for concurrent use.
func (p *Peer) Send(typeTag proto.MessageType, body []byte) error {
	return p.conn.Send(typeTag, body)
}

// Done returns a channel that is closed when the peer is fully torn down.
func (p *Peer) Done() <-chan struct{} { return p.doneCh }

// CloseErr returns whatever error caused the peer to terminate, or nil
// for graceful close.
func (p *Peer) CloseErr() error {
	if v := p.closeErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// close is internal: it tears the conn down once and stores the cause.
func (p *Peer) close(cause error) {
	p.closeOnce.Do(func() {
		if cause != nil {
			p.closeErr.Store(cause)
		}
		p.setState(StateClosed)
		p.conn.Close()
		close(p.doneCh)
	})
}

// run is the per-peer driver. It performs the application-level handshake,
// then reads in a loop, dispatching messages to the right handler. Returns
// when the peer is closed; the *Node uses Done() to wait for it.
//
// The state machine is:
//
//   1. Send HelloMessage              (state -> HelloSent)
//   2. Loop receiving:
//      - ConnectionAccepted          -> state Active, fire OnPeerConnected,
//                                       start heartbeat goroutine
//      - ConnectionRejected          -> close cleanly with reason error
//      - HelloMessage (inbound peer) -> reply with ConnectionAccepted
//      - CurrentTimeRequest          -> reply with CurrentTimeReply
//      - CurrentTimeReply            -> reset heartbeat deadline
//      - business message            -> if Active, forward to delegate;
//                                       else drop with "premature" warning
//   3. On read error or heartbeat timeout, close.
func (p *Peer) run(ctx context.Context, delegate Delegate) {
	defer p.close(nil)

	// 1. Send HelloMessage.
	if err := p.conn.Send(proto.HelloMessage, p.helloPayload); err != nil {
		p.close(fmt.Errorf("send hello: %w", err))
		return
	}
	p.setState(StateHelloSent)

	// Hello deadline: if we don't reach Active in HelloTimeout, drop.
	helloDeadline := time.Now().Add(HelloTimeout)
	_ = p.conn.SetDeadline(helloDeadline)

	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)

	// 2. Read loop.
	for {
		typeTag, body, err := p.conn.Receive()
		if err != nil {
			// Distinguish ctx cancel vs real error.
			if ctx.Err() != nil {
				p.close(nil)
			} else {
				p.close(fmt.Errorf("receive: %w", err))
			}
			if delegate != nil {
				delegate.OnPeerDisconnected(p, p.CloseErr())
			}
			return
		}
		switch typeTag {
		case proto.HelloMessage:
			// Inbound peer sent its hello after we sent ours. Accept it.
			p.setState(StateHelloReceived)
			if err := p.conn.Send(proto.ConnectionAcceptedMessage, nil); err != nil {
				p.close(fmt.Errorf("send connection_accepted: %w", err))
				if delegate != nil {
					delegate.OnPeerDisconnected(p, p.CloseErr())
				}
				return
			}
			// Don't fire OnPeerConnected yet — we still want to receive
			// THEIR ConnectionAccepted so both sides have agreed.
		case proto.ConnectionAcceptedMessage:
			if p.State() != StateActive {
				p.setState(StateActive)
				_ = p.conn.SetDeadline(time.Time{}) // clear hello deadline
				p.mu.Lock()
				p.lastHeartbeatRX = time.Now()
				p.mu.Unlock()
				if delegate != nil {
					delegate.OnPeerConnected(p)
				}
				go p.heartbeatLoop(ctx, heartbeatStop)
			}
		case proto.ConnectionRejectedMessage:
			// hived's connection_rejected_message body is FC-encoded
			// (user_agent, version, endpoint, reason_string, reason_code)
			// — once agent-p2p-msg lands the proto stub we can decode
			// the reason. For now we record the raw body length in the
			// error message.
			err := fmt.Errorf("peer rejected connection (%d-byte reason)", len(body))
			p.close(err)
			if delegate != nil {
				delegate.OnPeerDisconnected(p, err)
			}
			return
		case proto.CurrentTimeRequestMessage:
			// Reply immediately. Body would normally echo the request's
			// timestamp; agent-p2p-msg will fill in the FC encoding,
			// for now we just bounce the body back.
			if err := p.conn.Send(proto.CurrentTimeReplyMessage, body); err != nil {
				p.close(fmt.Errorf("send current_time_reply: %w", err))
				if delegate != nil {
					delegate.OnPeerDisconnected(p, p.CloseErr())
				}
				return
			}
			p.mu.Lock()
			p.lastHeartbeatRX = time.Now()
			p.mu.Unlock()
		case proto.CurrentTimeReplyMessage:
			p.mu.Lock()
			p.lastHeartbeatRX = time.Now()
			p.mu.Unlock()
		default:
			if p.State() == StateActive {
				if delegate != nil {
					delegate.OnMessage(p, typeTag, body)
				}
			}
			// Pre-Active business messages are silently dropped —
			// hived's node logs a warning here; we mirror that intent
			// but keep it cheap (no allocation in the hot read path).
		}
	}
}

// heartbeatLoop sends periodic current_time_requests and watches for
// missing replies. Stops when stop is closed (peer is going away) or
// when the timeout elapses without a heartbeat reply.
func (p *Peer) heartbeatLoop(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case now := <-t.C:
			p.mu.Lock()
			last := p.lastHeartbeatRX
			p.mu.Unlock()
			if !last.IsZero() && now.Sub(last) > HeartbeatTimeout {
				p.close(errors.New("heartbeat timeout"))
				return
			}
			// FC-encoded current_time_request_message body would carry
			// the request_sent_time. Stub it with 8 zero bytes for now.
			body := []byte{0, 0, 0, 0, 0, 0, 0, 0}
			if err := p.conn.Send(proto.CurrentTimeRequestMessage, body); err != nil {
				p.close(fmt.Errorf("send heartbeat: %w", err))
				return
			}
		}
	}
}
