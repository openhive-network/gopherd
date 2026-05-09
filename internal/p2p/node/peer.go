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
	"os"
	"sync"
	"sync/atomic"
	"time"

	"hived-go/internal/p2p/proto"
	"hived-go/internal/p2p/transport"
)

// TraceEnabled toggles a per-peer wire-trace dump on stderr. Driven by the
// GHIVE_P2P_TRACE env var ("1" / "true") at process start; useful when
// diff'ing handshake sequences against a real hived peer.
var TraceEnabled = func() bool {
	v := os.Getenv("GHIVE_P2P_TRACE")
	return v == "1" || v == "true" || v == "TRUE"
}()

// tracef writes a one-line peer-scoped trace message to stderr when
// TraceEnabled. Cheap no-op when disabled (just an interface check).
func (p *Peer) tracef(format string, args ...interface{}) {
	if !TraceEnabled {
		return
	}
	pre := fmt.Sprintf("p2p-trace [%s/%s] ", p.Conn().RemoteAddr(), p.direction)
	fmt.Fprintf(os.Stderr, pre+format+"\n", args...)
}

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
//
// Higher-level subsystems (sync, gossip, discovery) plug in by composing
// Delegate implementations: each wraps the next and forwards calls it
// doesn't claim. See internal/p2p/sync.NewDelegate for the catch-up
// catalogue, and internal/p2p/node.MultiDelegate for the fan-out helper.
type Delegate interface {
	// OnPeerConnected is fired exactly once per peer, the first time it
	// reaches StateActive.
	OnPeerConnected(*Peer)
	// OnPeerDisconnected is fired exactly once per peer, when it
	// transitions to StateClosed.
	OnPeerDisconnected(*Peer, error)
	// OnMessage is fired for every business message after the peer is
	// active (i.e. NOT for hello/connection_accepted/heartbeat messages
	// which the peer state machine consumes itself, and NOT for the
	// gossip messages BlockMessage / TrxMessage which we route through
	// dedicated callbacks below).
	OnMessage(*Peer, proto.MessageType, []byte)
	// OnBlockReceived is fired when a peer hands us a BlockMessage body
	// (the FC-binary signed_block + trailing 20-byte block_id). The
	// callee owns the slice; the peer goroutine does no further work
	// with it. The callee should NOT block — apply work belongs on a
	// dedicated drain goroutine.
	OnBlockReceived(*Peer, []byte)
	// OnTransactionReceived is fired when a peer hands us a TrxMessage
	// body (the FC-binary signed_transaction). Same ownership rules
	// as OnBlockReceived.
	OnTransactionReceived(*Peer, []byte)
	// OnAddressesReceived is fired when a remote sends us an
	// address_message (typically in response to our address_request after
	// the connection is active). The Node uses this to feed its peer
	// database.
	OnAddressesReceived(*Peer, []proto.AddressInfo)
	// BuildAddressReply is called when a remote sends us an
	// address_request_message and we need to assemble an address_message
	// reply. The implementation should return the set of currently-known
	// peer endpoints (typically active peers + the local listener).
	BuildAddressReply(*Peer) []proto.AddressInfo
}

// Peer is one peering session. Cheap; holds no state larger than the
// underlying *transport.Conn plus a few channels.
type Peer struct {
	conn      *transport.Conn
	direction Direction

	state atomic.Int32 // PeerState

	helloPayload       []byte                                  // local HelloMessage body (built per-peer because the signed shared-secret depends on the per-conn ECDHE)
	legacyHelloPayload []byte                                  // optional IPv4-only legacy_hello_message body
	buildHello         func(c *transport.Conn) ([]byte, error) // optional override (testing)
	buildLegacyHello   func(c *transport.Conn) ([]byte, error) // optional legacy_hello builder

	mu              sync.Mutex
	lastHeartbeatRX time.Time

	closeOnce sync.Once
	closeErr  atomic.Value // error
	doneCh    chan struct{}
}

// newPeer wraps a fresh transport.Conn. helloBuilder is called once at
// run-start to produce the FC-encoded HelloMessage body — must be lazy
// because the signed_shared_secret field depends on the per-connection
// stcp ECDHE result, which only becomes known AFTER performHandshake.
//
// legacyHelloBuilder, if non-nil, is invoked the same way to produce the
// IPv4-only legacy_hello_message body (graphene::net::legacy_hello_message,
// 5006). When set, peer.run sends the legacy body immediately after the new
// HelloMessage on every outbound dial — mirroring hived's send_hello_message
// which always emits both forms so older peers that don't recognize 5018
// can still complete the handshake on 5006.
func newPeer(c *transport.Conn, dir Direction, helloBuilder, legacyHelloBuilder func(c *transport.Conn) ([]byte, error)) *Peer {
	p := &Peer{
		conn:             c,
		direction:        dir,
		buildHello:       helloBuilder,
		buildLegacyHello: legacyHelloBuilder,
		doneCh:           make(chan struct{}),
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
	p.tracef("--> SEND %s len=%d", typeTag, len(body))
	err := p.conn.Send(typeTag, body)
	if err != nil {
		p.tracef("    SEND %s err=%v", typeTag, err)
	}
	return err
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

	// 1. Build + send HelloMessage. The body is built lazily here
	// because signed_shared_secret depends on the per-conn ECDHE
	// shared secret + our ephemeral key, both only available after
	// performHandshake completes inside transport.Dial/Listen.
	hello, err := p.buildHello(p.conn)
	if err != nil {
		p.close(fmt.Errorf("build hello: %w", err))
		return
	}
	p.helloPayload = hello
	if err := p.Send(proto.HelloMessageType, p.helloPayload); err != nil {
		p.close(fmt.Errorf("send hello: %w", err))
		return
	}
	// Mirror hived's send_hello_message: emit the IPv4-only legacy hello
	// (5006) right after the new hello (5018). Mainnet seeds running older
	// hived versions only understand 5006 and will silently EOF if they
	// don't see it; newer peers see both, accept the new one, and ignore
	// the legacy duplicate.
	if p.buildLegacyHello != nil {
		legacy, lerr := p.buildLegacyHello(p.conn)
		if lerr == nil && len(legacy) > 0 {
			p.legacyHelloPayload = legacy
			if err := p.Send(proto.LegacyHelloMessageType, p.legacyHelloPayload); err != nil {
				p.close(fmt.Errorf("send legacy_hello: %w", err))
				return
			}
		} else if lerr != nil {
			// Non-fatal: the new-hello path may still complete on a
			// recent peer.
			p.tracef("legacy hello build skipped: %v", lerr)
		}
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
				p.tracef("<-- RECV err=ctx-cancelled state=%s", p.State())
				p.close(nil)
			} else {
				p.tracef("<-- RECV err=%v state=%s", err, p.State())
				p.close(fmt.Errorf("receive: %w", err))
			}
			if delegate != nil {
				delegate.OnPeerDisconnected(p, p.CloseErr())
			}
			return
		}
		p.tracef("<-- RECV %s len=%d state=%s", typeTag, len(body), p.State())
		switch typeTag {
		case proto.HelloMessageType, proto.LegacyHelloMessageType:
			// Inbound peer sent its hello after we sent ours. Accept it.
			// Hived sends both 5018 and 5006 on every dial; the second
			// one arrives slightly after the first. Treat them
			// identically — once we've answered with ConnectionAccepted
			// once, the second one is a no-op (we stay in Active).
			if p.State() == StateHelloSent || p.State() == StateConnecting {
				p.setState(StateHelloReceived)
				if err := p.Send(proto.ConnectionAcceptedMessageType, nil); err != nil {
					p.close(fmt.Errorf("send connection_accepted: %w", err))
					if delegate != nil {
						delegate.OnPeerDisconnected(p, p.CloseErr())
					}
					return
				}
			}
			// Don't fire OnPeerConnected yet — we still want to receive
			// THEIR ConnectionAccepted so both sides have agreed.
		case proto.ConnectionAcceptedMessageType:
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
				// Mirror hived: every freshly-active peer sends an
				// address_request to learn the remote's view of the
				// network. We send ours immediately. Failure here
				// drops the peer — discovery is essential.
				if err := p.conn.Send(proto.AddressRequestMessageType, nil); err != nil {
					p.close(fmt.Errorf("send address_request: %w", err))
					if delegate != nil {
						delegate.OnPeerDisconnected(p, p.CloseErr())
					}
					return
				}
			}
		case proto.AddressRequestMessageType:
			// Remote wants to know who we know about. Build the reply
			// from our peer DB via the delegate hook and send it
			// straight back. Per hived: address_request is allowed
			// pre-Active too, but we only honor it once active so a
			// half-open peer can't pull our address book.
			if p.State() != StateActive || delegate == nil {
				continue
			}
			infos := delegate.BuildAddressReply(p)
			reply := &proto.AddressMessage{Addresses: infos}
			body, err := reply.Marshal()
			if err != nil {
				p.close(fmt.Errorf("marshal address_message: %w", err))
				delegate.OnPeerDisconnected(p, p.CloseErr())
				return
			}
			if err := p.conn.Send(proto.AddressMessageType, body); err != nil {
				p.close(fmt.Errorf("send address_message: %w", err))
				delegate.OnPeerDisconnected(p, p.CloseErr())
				return
			}
		case proto.LegacyAddressMessageType:
			// Legacy address_message (5010) — vector of legacy_address_info
			// (IPv4-only endpoint). We don't currently maintain a separate
			// peerdb for legacy peers; treat this as an inbound discovery
			// signal but skip parsing for now (the new 5020 path covers
			// modern peers). A future cleanup can teach the peerdb to
			// accept legacy addresses too.
			_ = body
		case proto.AddressMessageType:
			if delegate == nil {
				continue
			}
			var msg proto.AddressMessage
			if err := msg.Unmarshal(body); err != nil {
				// A malformed address_message is non-fatal: ignore and
				// keep the peer. Mirrors hived's lenient behavior here.
				continue
			}
			delegate.OnAddressesReceived(p, msg.Addresses)
		case proto.LegacyConnectionRejectedMessageType:
			// Older peers send the IPv4-only legacy_connection_rejected
			// (5008). Body fields match the new connection_rejected modulo
			// the endpoint encoding; for diagnostics we just record the
			// raw size — older peers usually wrap the same human-readable
			// reason string we'd see from a 5019.
			err := fmt.Errorf("peer rejected connection (legacy, %d-byte reason)", len(body))
			p.tracef("LEGACY-REJECTED: %v", err)
			p.close(err)
			if delegate != nil {
				delegate.OnPeerDisconnected(p, err)
			}
			return
		case proto.ConnectionRejectedMessageType:
			// Decode the reason for inclusion in the error so the operator
			// can see why hived rejected the handshake.
			var rej proto.ConnectionRejectedMessage
			var err error
			if dErr := rej.Unmarshal(body); dErr == nil {
				err = fmt.Errorf("peer rejected connection: code=%d reason=%q", rej.ReasonCode, rej.ReasonString)
			} else {
				err = fmt.Errorf("peer rejected connection (%d-byte reason; decode err=%v)", len(body), dErr)
			}
			p.tracef("REJECTED: %v", err)
			p.close(err)
			if delegate != nil {
				delegate.OnPeerDisconnected(p, err)
			}
			return
		case proto.CurrentTimeRequestMessageType:
			// Decode the request to recover request_sent_time, then build
			// a proper 3-timestamp current_time_reply_message body. Hived
			// closes the connection if the reply is the wrong size (8 vs
			// 24 bytes) — the request and reply are NOT the same shape,
			// despite the names being symmetrical.
			now := uint64(time.Now().UnixMicro())
			var req proto.CurrentTimeRequestMessage
			if err := req.Unmarshal(body); err != nil {
				// Fall back to zero so we still answer; hived's own
				// clock_offset math handles bogus values.
				req.RequestSentTime = 0
			}
			reply := proto.CurrentTimeReplyMessage{
				RequestSentTime:      req.RequestSentTime,
				RequestReceivedTime:  now,
				ReplyTransmittedTime: now,
			}
			rb, err := reply.Marshal()
			if err != nil {
				p.close(fmt.Errorf("marshal current_time_reply: %w", err))
				if delegate != nil {
					delegate.OnPeerDisconnected(p, p.CloseErr())
				}
				return
			}
			if err := p.Send(proto.CurrentTimeReplyMessageType, rb); err != nil {
				p.close(fmt.Errorf("send current_time_reply: %w", err))
				if delegate != nil {
					delegate.OnPeerDisconnected(p, p.CloseErr())
				}
				return
			}
			p.mu.Lock()
			p.lastHeartbeatRX = time.Now()
			p.mu.Unlock()
		case proto.CurrentTimeReplyMessageType:
			p.mu.Lock()
			p.lastHeartbeatRX = time.Now()
			p.mu.Unlock()
		case proto.BlockMessageType:
			if p.State() == StateActive && delegate != nil {
				delegate.OnBlockReceived(p, body)
			}
		case proto.TrxMessageType:
			if p.State() == StateActive && delegate != nil {
				delegate.OnTransactionReceived(p, body)
			}
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
			// FC-encoded current_time_request_message body carries the
			// request_sent_time as a uint64 LE microseconds-since-epoch.
			req := proto.CurrentTimeRequestMessage{
				RequestSentTime: uint64(time.Now().UnixMicro()),
			}
			rb, err := req.Marshal()
			if err != nil {
				p.close(fmt.Errorf("marshal heartbeat: %w", err))
				return
			}
			if err := p.Send(proto.CurrentTimeRequestMessageType, rb); err != nil {
				p.close(fmt.Errorf("send heartbeat: %w", err))
				return
			}
		}
	}
}
