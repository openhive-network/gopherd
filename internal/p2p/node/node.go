package node

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"hived-go/internal/chain"
	"hived-go/internal/p2p/proto"
	"hived-go/internal/p2p/transport"
	"hived-go/internal/protocol/network"
)

// ChainView is the slice of chain.Database the node layer needs. We take
// this narrow interface instead of *chain.Database directly so tests can
// stub it.
type ChainView interface {
	HeadBlock() (uint32, error)
}

// Compile-time check that *chain.Database satisfies ChainView.
var _ ChainView = (*chain.Database)(nil)

// Config tunes a Node.
type Config struct {
	// Endpoint is the local "host:port" to listen on. Empty means
	// observer-only (we only dial outbound seeds).
	Endpoint string
	// SeedNodes is the list of "host:port" peers to dial on startup.
	SeedNodes []string
	// UserAgent is what we put in HelloMessage.user_agent.
	UserAgent string
	// MaxPeers caps the total connected peer count (inbound + outbound).
	// 0 = no cap.
	MaxPeers int
	// Logger receives one-line status messages ("peer connected", etc).
	// nil → os.Stderr.
	Logger io.Writer
}

// Node is the daemon-wide P2P singleton. One instance per process.
type Node struct {
	cfg      Config
	chain    ChainView
	delegate Delegate

	mu    sync.Mutex
	peers map[*Peer]struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	helloPayload []byte // built once at New
}

// New builds an unstarted *Node. delegate may be nil — in that case
// the node accepts and maintains peers but routes business messages
// nowhere (useful for the Phase-2.1 milestone of "just establish
// connections").
func New(cfg Config, ch ChainView, delegate Delegate) *Node {
	if cfg.UserAgent == "" {
		cfg.UserAgent = "ghive-test"
	}
	if cfg.Logger == nil {
		cfg.Logger = os.Stderr
	}
	n := &Node{
		cfg:      cfg,
		chain:    ch,
		delegate: delegate,
		peers:    make(map[*Peer]struct{}),
	}
	n.helloPayload = n.buildHelloPayload()
	return n
}

// buildHelloPayload assembles a stub HelloMessage body. Wire format will
// be replaced by agent-p2p-msg's FC-encoded version on merge.
//
// Layout (LE everywhere):
//
//   [u32 user_agent_len][user_agent bytes]
//   [32 bytes chain_id]
//   [u32 head_block_num]
//
// agent-p2p-msg's HelloMessage carries more fields (core_protocol_version,
// inbound_address, ports, node_public_key, signed_shared_secret, user_data),
// but for transport-only validation we just need bytes that survive the
// round-trip.
func (n *Node) buildHelloPayload() []byte {
	ua := []byte(n.cfg.UserAgent)
	headBlock := uint32(0)
	if n.chain != nil {
		if h, err := n.chain.HeadBlock(); err == nil {
			headBlock = h
		}
	}
	body := make([]byte, 4+len(ua)+32+4)
	binary.LittleEndian.PutUint32(body[0:4], uint32(len(ua)))
	copy(body[4:], ua)
	off := 4 + len(ua)
	copy(body[off:off+32], network.ChainID[:])
	off += 32
	binary.LittleEndian.PutUint32(body[off:off+4], headBlock)
	return body
}

// Start launches the listener and dial-out goroutines. Idempotent — calling
// Start twice does nothing on the second call.
func (n *Node) Start() error {
	if n.cancel != nil {
		return nil
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())

	if n.cfg.Endpoint != "" {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			err := transport.Listen(n.ctx, n.cfg.Endpoint, func(c *transport.Conn) {
				n.handleNewConn(c, DirectionInbound)
			})
			if err != nil && n.ctx.Err() == nil {
				n.logf("p2p: listener exited: %v", err)
			}
		}()
		n.logf("p2p: listening on %s", n.cfg.Endpoint)
	}

	for _, seed := range n.cfg.SeedNodes {
		seed := seed
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.dialOutLoop(seed)
		}()
	}

	return nil
}

// dialOutLoop keeps trying to dial seed forever, with retry/backoff,
// until the node is stopped.
func (n *Node) dialOutLoop(endpoint string) {
	const baseRetry = 5 * time.Second
	const maxRetry = 60 * time.Second
	retry := baseRetry
	for {
		if n.ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
		conn, err := transport.Dial(ctx, endpoint)
		cancel()
		if err != nil {
			n.logf("p2p: dial %s failed: %v (retrying in %s)", endpoint, err, retry)
			select {
			case <-time.After(retry):
				retry = minDuration(retry*2, maxRetry)
			case <-n.ctx.Done():
				return
			}
			continue
		}
		retry = baseRetry
		n.handleNewConn(conn, DirectionOutbound)
		// handleNewConn returns once the peer is fully torn down.
		// Reconnect after a short pause.
		select {
		case <-time.After(baseRetry):
		case <-n.ctx.Done():
			return
		}
	}
}

// handleNewConn wraps a fresh transport.Conn in a *Peer and runs it
// to completion. Returns when the peer is closed.
func (n *Node) handleNewConn(c *transport.Conn, dir Direction) {
	if n.cfg.MaxPeers > 0 {
		n.mu.Lock()
		full := len(n.peers) >= n.cfg.MaxPeers
		n.mu.Unlock()
		if full {
			c.Close()
			n.logf("p2p: dropping %s connection from %s — at peer cap",
				dir, c.RemoteAddr())
			return
		}
	}

	peer := newPeer(c, dir, n.helloPayload)
	n.mu.Lock()
	n.peers[peer] = struct{}{}
	n.mu.Unlock()

	wrapped := newLoggingDelegate(n.delegate, n)
	peer.run(n.ctx, wrapped)

	n.mu.Lock()
	delete(n.peers, peer)
	n.mu.Unlock()
}

// Stop signals the node to shut down and waits for all goroutines to exit.
// Calling Stop twice is safe; the second call returns immediately.
func (n *Node) Stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	n.cancel = nil

	// Close every active peer to break their read loops.
	n.mu.Lock()
	for p := range n.peers {
		p.close(nil)
	}
	n.mu.Unlock()

	n.wg.Wait()
}

// Peers returns a snapshot of the current peer set.
func (n *Node) Peers() []*Peer {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*Peer, 0, len(n.peers))
	for p := range n.peers {
		out = append(out, p)
	}
	return out
}

// PeerCount returns the current number of active peers.
func (n *Node) PeerCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.peers)
}

func (n *Node) logf(format string, args ...interface{}) {
	if n.cfg.Logger == nil {
		return
	}
	fmt.Fprintf(n.cfg.Logger, format+"\n", args...)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// loggingDelegate wraps a user delegate to log connect/disconnect events.
type loggingDelegate struct {
	inner Delegate
	n     *Node
}

func newLoggingDelegate(inner Delegate, n *Node) Delegate {
	return &loggingDelegate{inner: inner, n: n}
}

func (l *loggingDelegate) OnPeerConnected(p *Peer) {
	l.n.logf("p2p: peer connected: %s (%s)", p.Conn().RemoteAddr(), p.Direction())
	if l.inner != nil {
		l.inner.OnPeerConnected(p)
	}
}

func (l *loggingDelegate) OnPeerDisconnected(p *Peer, cause error) {
	if cause != nil {
		l.n.logf("p2p: peer dropped: %s (%v)", p.Conn().RemoteAddr(), cause)
	} else {
		l.n.logf("p2p: peer dropped: %s", p.Conn().RemoteAddr())
	}
	if l.inner != nil {
		l.inner.OnPeerDisconnected(p, cause)
	}
}

func (l *loggingDelegate) OnMessage(p *Peer, t proto.MessageType, body []byte) {
	if l.inner != nil {
		l.inner.OnMessage(p, t, body)
	}
}

// Direction.String for log lines.
func (d Direction) String() string {
	if d == DirectionInbound {
		return "inbound"
	}
	return "outbound"
}
