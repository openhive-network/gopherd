package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"hived-go/internal/chain"
	"hived-go/internal/crypto"
	"hived-go/internal/p2p/peerdb"
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

// Defaults for the dial scheduler. Exposed as vars so tests can shorten them.
//
// These are package-wide DEFAULTS used when Config.MaxConcurrentDials /
// Config.TargetOutboundPeers are zero. Callers that want to override them
// per-Node should set those Config fields rather than mutating the
// globals (the globals are kept for back-compat with discovery_test.go
// which speeds up DialTickInterval).
var (
	// DialTickInterval is how often the dialer wakes up to look for new
	// dial candidates (peerdb back-off changes, etc).
	DialTickInterval = 1 * time.Second
	// MaxConcurrentDials caps how many outbound dials can be in flight
	// at once. Bumped from 8 → 24 so we ramp up the peer set faster once
	// the peerdb fills via address_message gossip; the previous cap kept
	// us pinned to the seed list (8 mainnet seeds) for the first ~minute
	// of catch-up which throttled fan-out.
	MaxConcurrentDials = 24
	// TargetOutboundPeers is the number of active outbound peers we try
	// to maintain. Once we hit this we stop initiating new dials.
	// Bumped from 12 → 32 so adaptive FetchBatch has more parallel
	// requestees during catch-up. Each idle peer costs ~1 TCP socket +
	// the per-peer state map; 32 is well within OS file-descriptor limits.
	TargetOutboundPeers = 32
)

// Config tunes a Node.
type Config struct {
	// Endpoint is the local "host:port" to listen on. Empty means
	// observer-only (we only dial outbound seeds).
	Endpoint string
	// SeedNodes is the list of "host:port" peers to dial on startup.
	// They are stored as Sticky entries in the peer DB so they survive
	// repeated failures.
	SeedNodes []string
	// UserAgent is what we put in HelloMessage.user_agent.
	UserAgent string
	// MaxPeers caps the total connected peer count (inbound + outbound).
	// 0 = no cap.
	MaxPeers int
	// Logger receives one-line status messages ("peer connected", etc).
	// nil → os.Stderr.
	Logger io.Writer
	// PeerDBPath is the path to peers.json. Empty → memory-only DB
	// (suitable for tests). Production callers usually pass
	// filepath.Join(cfg.DataDir, peerdb.Filename).
	PeerDBPath string
	// MaxOutboundPeers, when > 0, overrides the package-wide
	// TargetOutboundPeers default for this Node only. The dial loop
	// stops initiating new outbound dials once active outbound count
	// reaches this value.
	MaxOutboundPeers int
	// MaxConcurrentDials, when > 0, overrides the package-wide
	// MaxConcurrentDials default for this Node only. Caps the number of
	// in-flight dial attempts at any given moment.
	MaxConcurrentDials int
}

// Node is the daemon-wide P2P singleton. One instance per process.
type Node struct {
	cfg      Config
	chain    ChainView
	delegate Delegate

	mu    sync.Mutex
	peers map[*Peer]*peerInfo
	// activeEndpoints maps connected-peer endpoint string → *Peer so the
	// dialer can fast-skip duplicates. Updated under mu.
	activeEndpoints map[string]*Peer
	// pendingDials tracks endpoints we're currently dialing (in-flight)
	// so we don't double-dial the same target while a connect attempt
	// is racing.
	pendingDials map[string]struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// helloPayload removed: now built per-peer in buildHelloPayload(c).
	peerdb       *peerdb.Store

	// localAddrPorts caches the parsed forms of cfg.Endpoint so
	// self-dial protection is cheap.
	selfEndpoints map[string]struct{}
}

// peerInfo carries discovery-loop-specific state about an active peer:
// its dial-target endpoint string (so we know which peerdb entry to
// touch on disconnect), and which direction the connection runs.
type peerInfo struct {
	endpoint string // canonical dialable form, "" if we don't know
	dir      Direction
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
		cfg:             cfg,
		chain:           ch,
		delegate:        delegate,
		peers:           make(map[*Peer]*peerInfo),
		activeEndpoints: make(map[string]*Peer),
		pendingDials:    make(map[string]struct{}),
		selfEndpoints:   make(map[string]struct{}),
	}
	n.populateSelfEndpoints()
	return n
}

// populateSelfEndpoints precomputes every variant of "this is us" so the
// discovery dialer doesn't connect to itself when the seed list happens
// to include our own listening address (a common misconfiguration in
// dynamically-generated testnet configs).
func (n *Node) populateSelfEndpoints() {
	if n.cfg.Endpoint == "" {
		return
	}
	host, port, err := net.SplitHostPort(n.cfg.Endpoint)
	if err != nil {
		// SplitHostPort failed; nothing reliable to add.
		return
	}
	add := func(s string) {
		if s != "" {
			n.selfEndpoints[s] = struct{}{}
		}
	}
	add(n.cfg.Endpoint)
	// For 0.0.0.0 / :: the node listens on every interface — accept
	// loopback and real IPs alike as "self". Only common variants
	// pre-computed; the runtime check on conn.LocalAddr also catches
	// late surprises.
	switch host {
	case "0.0.0.0", "":
		add(net.JoinHostPort("127.0.0.1", port))
		add(net.JoinHostPort("localhost", port))
	case "::", "[::]":
		add(net.JoinHostPort("::1", port))
		add(net.JoinHostPort("[::1]", port))
	}
	// Also normalize "127.0.0.1" / "localhost" pairs so a config that
	// uses one and a peer announce that uses the other still match.
	if host == "127.0.0.1" {
		add(net.JoinHostPort("localhost", port))
	}
	if host == "localhost" {
		add(net.JoinHostPort("127.0.0.1", port))
	}
}

// buildHelloPayload assembles the FC-encoded body of hived's hello_message
// for one outbound connection. Hived's on_hello_message handler verifies:
//
//  1. node_public_key matches the ephemeral pubkey we presented during
//     the stcp ECDHE handshake (so we own LocalPublicKey).
//  2. recovered pubkey from signed_shared_secret over sha256(shared_secret)
//     equals node_public_key (proves we computed the same shared secret,
//     i.e. no MITM).
//  3. user_data["chain_id"] (when present) matches the local chain.
//
// Each peer connection has a distinct ephemeral key + shared secret, so
// hello must be built lazily per-connection (after performHandshake
// completes). The returned bytes are the FC-encoded body; the envelope
// header is added by transport.Conn.Send.
func (n *Node) buildHelloPayload(c *transport.Conn) ([]byte, error) {
	priv := c.LocalPrivateKey()
	if priv == nil {
		return nil, fmt.Errorf("hello: stcp local ephemeral key not exposed")
	}
	shared := c.SharedSecret()
	digest := sha256.Sum256(shared[:])
	sig, err := crypto.SignCompact(priv, digest)
	if err != nil {
		return nil, fmt.Errorf("hello: sign shared secret: %w", err)
	}

	localPub := c.LocalPublicKey()
	var nodePub proto.PublicKeyData
	copy(nodePub[:], localPub[:])
	var sigBytes proto.CompactSignature
	copy(sigBytes[:], sig[:])

	// Inbound port = our listening port (0 if outbound-only).
	var inboundPort uint16
	if n.cfg.Endpoint != "" {
		if _, p, err := net.SplitHostPort(n.cfg.Endpoint); err == nil {
			if v, err := strconv.ParseUint(p, 10, 16); err == nil {
				inboundPort = uint16(v)
			}
		}
	}
	// outbound_port mirrors inbound_port for hived (see node.cpp:on_hello).
	outboundPort := inboundPort
	// inbound_address is filled by the peer side from the TCP connection
	// (see hived's on_hello_message — it reads originating_peer's
	// inbound_address from this field but actually uses get_remote_endpoint
	// for routing). Send loopback as a placeholder; hived ignores.
	inboundAddr := netip.MustParseAddr("0.0.0.0")

	// user_data carries chain_id + node_id + version stamps. All fields
	// are optional per hived's parse_hello_user_data_for_peer, but
	// chain_id is checked against the local chain when present, which is
	// the difference between "known peer on the same network" and
	// "stranger; close politely". We send the canonical fields hived
	// sends back.
	ud := proto.NewVarObject()
	ud.Set("chain_id", proto.NewVariantString(network.ChainID.Hex()))
	// node_id: stable identity. For now reuse our ephemeral pubkey hex
	// (hived dedups by this; same value across reconnects is a separate
	// identity-key concern we'll layer in later).
	ud.Set("node_id", proto.NewVariantString(hex.EncodeToString(localPub[:])))
	ud.Set("graphene_git_revision_sha", proto.NewVariantString("ghive-go"))
	ud.Set("graphene_git_revision_unix_timestamp", proto.NewVariantUint64(uint64(time.Now().Unix())))
	ud.Set("platform", proto.NewVariantString(runtime.GOOS))
	ud.Set("bitness", proto.NewVariantUint64(64))

	hello := proto.HelloMessage{
		UserAgent:           n.cfg.UserAgent,
		CoreProtocolVersion: proto.CoreProtocolVersion,
		InboundAddress:      inboundAddr,
		InboundPort:         inboundPort,
		OutboundPort:        outboundPort,
		NodePublicKey:       nodePub,
		SignedSharedSecret:  sigBytes,
		UserData:            ud,
	}
	body, err := hello.Marshal()
	if err != nil {
		return nil, fmt.Errorf("hello: marshal: %w", err)
	}
	return body, nil
}

// buildLegacyHelloPayload mirrors buildHelloPayload but emits the IPv4-only
// legacy_hello_message body (graphene::net::legacy_hello_message, msg_type
// 5006). hived's send_hello_message sends both the new hello and this legacy
// form back-to-back so older peers without 5018 support can still complete
// the handshake. Returns the serialized body or nil if the local inbound
// address isn't IPv4 (legacy_address can't carry v6).
func (n *Node) buildLegacyHelloPayload(c *transport.Conn) ([]byte, error) {
	priv := c.LocalPrivateKey()
	if priv == nil {
		return nil, fmt.Errorf("legacy hello: stcp local ephemeral key not exposed")
	}
	shared := c.SharedSecret()
	digest := sha256.Sum256(shared[:])
	sig, err := crypto.SignCompact(priv, digest)
	if err != nil {
		return nil, fmt.Errorf("legacy hello: sign shared secret: %w", err)
	}

	localPub := c.LocalPublicKey()
	var nodePub proto.PublicKeyData
	copy(nodePub[:], localPub[:])
	var sigBytes proto.CompactSignature
	copy(sigBytes[:], sig[:])

	var inboundPort uint16
	if n.cfg.Endpoint != "" {
		if _, p, err := net.SplitHostPort(n.cfg.Endpoint); err == nil {
			if v, err := strconv.ParseUint(p, 10, 16); err == nil {
				inboundPort = uint16(v)
			}
		}
	}
	outboundPort := inboundPort
	inboundAddr := netip.MustParseAddr("0.0.0.0")

	ud := proto.NewVarObject()
	ud.Set("chain_id", proto.NewVariantString(network.ChainID.Hex()))
	ud.Set("node_id", proto.NewVariantString(hex.EncodeToString(localPub[:])))
	ud.Set("graphene_git_revision_sha", proto.NewVariantString("ghive-go"))
	ud.Set("graphene_git_revision_unix_timestamp", proto.NewVariantUint64(uint64(time.Now().Unix())))
	ud.Set("platform", proto.NewVariantString(runtime.GOOS))
	ud.Set("bitness", proto.NewVariantUint64(64))

	legacy := proto.LegacyHelloMessage{
		UserAgent:           n.cfg.UserAgent,
		CoreProtocolVersion: proto.CoreProtocolVersion,
		InboundAddress:      inboundAddr,
		InboundPort:         inboundPort,
		OutboundPort:        outboundPort,
		NodePublicKey:       nodePub,
		SignedSharedSecret:  sigBytes,
		UserData:            ud,
	}
	body, err := legacy.Marshal()
	if err != nil {
		return nil, fmt.Errorf("legacy hello: marshal: %w", err)
	}
	return body, nil
}

// SetDelegate installs (or replaces) the application-layer delegate.
// MUST be called before Start; once peers are running there is no
// guard against the delegate pointer being read from another
// goroutine while we replace it.
func (n *Node) SetDelegate(d Delegate) {
	n.delegate = d
	// rebuild helloPayload not necessary — delegate doesn't influence it.
}

// Start launches the listener and dial-out goroutines. Idempotent — calling
// Start twice does nothing on the second call.
func (n *Node) Start() error {
	if n.cancel != nil {
		return nil
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())

	// Boot the peer DB (loads peers.json off disk if it exists). Each
	// configured seed becomes a Sticky entry so it always survives
	// failure-driven demotion.
	pdb, err := peerdb.Open(n.cfg.PeerDBPath, peerdb.Options{})
	if err != nil {
		return fmt.Errorf("p2p: open peer DB: %w", err)
	}
	n.peerdb = pdb
	for _, sd := range n.cfg.SeedNodes {
		n.peerdb.AddSticky(sd)
	}
	n.peerdb.Run() // start background flush

	if n.cfg.Endpoint != "" {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			err := transport.Listen(n.ctx, n.cfg.Endpoint, func(c *transport.Conn) {
				n.handleNewConn(c, DirectionInbound, "")
			})
			if err != nil && n.ctx.Err() == nil {
				n.logf("p2p: listener exited: %v", err)
			}
		}()
		n.logf("p2p: listening on %s", n.cfg.Endpoint)
	}

	// Discovery-driven dial loop. One goroutine that wakes periodically
	// and fans out new dials based on peerdb.DialableNow().
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.discoveryLoop()
	}()

	return nil
}

// discoveryLoop periodically polls the peer DB and spawns dials for
// candidates whose back-off has elapsed and that aren't already
// connected. Caps in-flight dials at MaxConcurrentDials and stops
// dialing once we have TargetOutboundPeers active outbound connections.
func (n *Node) discoveryLoop() {
	// First-tick fast path: scan immediately so seed nodes get dialed
	// without waiting a full tick.
	n.dialPass()
	t := time.NewTicker(DialTickInterval)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			n.dialPass()
		}
	}
}

// dialPass runs one round of "look at peerdb, fire off new dials".
func (n *Node) dialPass() {
	n.mu.Lock()
	// Build the skip set: every endpoint we're already connected to, every
	// pending dial, and every form of our own listening address.
	skip := make(map[string]struct{}, len(n.activeEndpoints)+len(n.pendingDials)+len(n.selfEndpoints))
	for ep := range n.activeEndpoints {
		skip[ep] = struct{}{}
	}
	for ep := range n.pendingDials {
		skip[ep] = struct{}{}
	}
	for ep := range n.selfEndpoints {
		skip[ep] = struct{}{}
	}
	outboundCount := 0
	for _, info := range n.peers {
		if info.dir == DirectionOutbound {
			outboundCount++
		}
	}
	pending := len(n.pendingDials)
	n.mu.Unlock()

	target := n.cfg.MaxOutboundPeers
	if target <= 0 {
		target = TargetOutboundPeers
	}
	maxConc := n.cfg.MaxConcurrentDials
	if maxConc <= 0 {
		maxConc = MaxConcurrentDials
	}

	if outboundCount+pending >= target {
		return
	}
	want := target - (outboundCount + pending)
	if want <= 0 {
		return
	}
	if want > maxConc-pending {
		want = maxConc - pending
	}
	if want <= 0 {
		return
	}

	candidates := n.peerdb.DialableNow(skip)
	if len(candidates) == 0 {
		return
	}
	if len(candidates) > want {
		candidates = candidates[:want]
	}

	for _, ep := range candidates {
		n.mu.Lock()
		// Recheck under the lock — another tick or async OnConnect may
		// have stolen the slot.
		if _, dup := n.activeEndpoints[ep]; dup {
			n.mu.Unlock()
			continue
		}
		if _, pend := n.pendingDials[ep]; pend {
			n.mu.Unlock()
			continue
		}
		if _, self := n.selfEndpoints[ep]; self {
			n.mu.Unlock()
			continue
		}
		n.pendingDials[ep] = struct{}{}
		n.mu.Unlock()

		ep := ep
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.dialOnce(ep)
		}()
	}
}

// dialOnce performs a single dial attempt. Records attempt + outcome in
// the peer DB and (on success) hands the conn to handleNewConn.
func (n *Node) dialOnce(endpoint string) {
	n.peerdb.MarkAttempt(endpoint)
	ctx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
	conn, err := transport.Dial(ctx, endpoint)
	cancel()
	if err != nil {
		n.peerdb.MarkFailed(endpoint)
		n.logf("p2p: dial %s failed: %v", endpoint, err)
		n.mu.Lock()
		delete(n.pendingDials, endpoint)
		n.mu.Unlock()
		return
	}
	// Self-dial detection: if our local-side socket address matches the
	// remote's reported address (which happens when a misconfigured seed
	// list includes our own listening port), bail before exchanging
	// anything.
	if n.isSelfDial(conn) {
		n.logf("p2p: self-dial detected on %s; closing", endpoint)
		conn.Close()
		n.peerdb.MarkFailed(endpoint)
		n.mu.Lock()
		delete(n.pendingDials, endpoint)
		n.mu.Unlock()
		return
	}
	// Hand off — handleNewConn clears pendingDials and registers the
	// peer in activeEndpoints.
	n.handleNewConn(conn, DirectionOutbound, endpoint)
}

// isSelfDial returns true if the new outbound conn ended up looped back
// to our own listener. We compare the peer's RemoteAddr against our
// known self-endpoints (and against our own LocalAddr's port — same-
// port + same-host is a slam-dunk self loop).
func (n *Node) isSelfDial(c *transport.Conn) bool {
	remote := c.RemoteAddr()
	if remote == nil {
		return false
	}
	rs := remote.String()
	if _, ok := n.selfEndpoints[rs]; ok {
		return true
	}
	// Also check ip:port equality with our own listener via our local
	// addr. If our outbound socket's local port == listener port AND the
	// remote ip is loopback, that's a self-loop.
	local := c.LocalAddr()
	if local == nil || n.cfg.Endpoint == "" {
		return false
	}
	_, ourPort, err := net.SplitHostPort(n.cfg.Endpoint)
	if err != nil {
		return false
	}
	rap, err := netip.ParseAddrPort(rs)
	if err != nil {
		return false
	}
	if fmt.Sprintf("%d", rap.Port()) != ourPort {
		return false
	}
	return rap.Addr().IsLoopback()
}

// handleNewConn wraps a fresh transport.Conn in a *Peer and runs it
// to completion. Returns when the peer is closed.
//
// dialEndpoint is the canonical endpoint string we used to dial out to
// this peer (or "" for inbound conns whose dial-form we don't know).
func (n *Node) handleNewConn(c *transport.Conn, dir Direction, dialEndpoint string) {
	if n.cfg.MaxPeers > 0 {
		n.mu.Lock()
		full := len(n.peers) >= n.cfg.MaxPeers
		n.mu.Unlock()
		if full {
			c.Close()
			n.logf("p2p: dropping %s connection from %s — at peer cap",
				dir, c.RemoteAddr())
			if dialEndpoint != "" {
				n.mu.Lock()
				delete(n.pendingDials, dialEndpoint)
				n.mu.Unlock()
			}
			return
		}
	}

	peer := newPeer(c, dir, n.buildHelloPayload, n.buildLegacyHelloPayload)
	info := &peerInfo{endpoint: dialEndpoint, dir: dir}
	n.mu.Lock()
	n.peers[peer] = info
	if dialEndpoint != "" {
		n.activeEndpoints[dialEndpoint] = peer
		delete(n.pendingDials, dialEndpoint)
	}
	// Inbound conns: track by RemoteAddr string so a second incoming
	// conn from the same address can be deduped.
	if dir == DirectionInbound {
		ra := c.RemoteAddr().String()
		n.activeEndpoints[ra] = peer
	}
	n.mu.Unlock()

	if dialEndpoint != "" {
		n.peerdb.MarkConnected(dialEndpoint)
	}

	wrapped := newLoggingDelegate(n.delegate, n)
	peer.run(n.ctx, wrapped)

	n.mu.Lock()
	delete(n.peers, peer)
	if dialEndpoint != "" {
		delete(n.activeEndpoints, dialEndpoint)
	}
	if dir == DirectionInbound {
		delete(n.activeEndpoints, c.RemoteAddr().String())
	}
	n.mu.Unlock()
}

// handlePeerAddresses processes an incoming address_message from a peer.
// Each entry is fed into the peer DB (modulo self-filter and dedup);
// the discovery loop will pick newly-discovered endpoints up on its
// next tick.
func (n *Node) handlePeerAddresses(p *Peer, addrs []proto.AddressInfo) {
	if n.peerdb == nil {
		return
	}
	added := 0
	for _, a := range addrs {
		ep := a.RemoteEndpoint.Addr.String()
		if ep == "" {
			continue
		}
		// Strip out unspecified addresses (0.0.0.0/0 etc) — they're not
		// dialable.
		ip := a.RemoteEndpoint.Addr.Addr()
		if !ip.IsValid() || ip.IsUnspecified() {
			continue
		}
		if a.RemoteEndpoint.Addr.Port() == 0 {
			continue
		}
		// Skip our own endpoint variants.
		if _, self := n.selfEndpoints[ep]; self {
			continue
		}
		if _, exists := n.peerdb.Get(ep); !exists {
			added++
		}
		n.peerdb.Discover(ep)
	}
	if added > 0 {
		n.logf("p2p: learned %d new peer(s) from %s", added, p.Conn().RemoteAddr())
	}
}

// buildAddressReply assembles an AddressMessage payload for an
// address_request from a peer. We share every active peer's known
// endpoint plus our own listening endpoint.
//
// Note: hived also includes nodes from its address book (i.e. ones we
// know about but aren't currently connected to). For now we're more
// conservative: only currently-active peers + self. This still bootstraps
// full mesh in the testnet smoke test (each node has at least node-1 as
// a seed; once all nodes peer with node-1, node-1's reply contains every
// other node's endpoint).
func (n *Node) buildAddressReply(_ *Peer) []proto.AddressInfo {
	now := uint32(time.Now().Unix())
	var out []proto.AddressInfo

	// Our own listener (if any).
	if ep := n.cfg.Endpoint; ep != "" {
		if ai, ok := makeAddressInfo(ep, now, proto.DirectionInbound); ok {
			out = append(out, ai)
		}
	}

	n.mu.Lock()
	for _, info := range n.peers {
		if info.endpoint == "" {
			continue
		}
		if ai, ok := makeAddressInfo(info.endpoint, now, proto.DirectionOutbound); ok {
			out = append(out, ai)
		}
	}
	n.mu.Unlock()

	return out
}

// makeAddressInfo parses a "host:port" string into a proto.AddressInfo.
// Names that don't resolve to a literal IP are skipped (we'd need a
// resolver call; for the testnet they're always literals).
func makeAddressInfo(endpoint string, now uint32, dir proto.PeerConnectionDirection) (proto.AddressInfo, bool) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return proto.AddressInfo{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Try resolving "localhost" → 127.0.0.1 explicitly to keep the
		// testnet path working without a syscall round trip.
		switch host {
		case "localhost":
			addr = netip.MustParseAddr("127.0.0.1")
		default:
			return proto.AddressInfo{}, false
		}
	}
	var port uint16
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return proto.AddressInfo{}, false
	}
	return proto.AddressInfo{
		RemoteEndpoint: proto.MakeEndpoint(addr, port),
		LastSeenTime:   now,
		Direction:      dir,
	}, true
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

	// Final flush of the peer DB.
	if n.peerdb != nil {
		_ = n.peerdb.Close()
	}
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

// Broadcast fans the (typeTag, body) pair out to every active peer EXCEPT
// `exclude` (which may be nil). Used by:
//
//   - the witness plugin: after locally producing a block, push it to
//     all peers (exclude=nil).
//   - the gossip layer: after applying a block we received from peer X,
//     re-broadcast to all peers OTHER than X (exclude=X).
//
// Send errors are logged but not surfaced — a wedged peer should be
// pruned by its heartbeat loop, not by a failed broadcast.
func (n *Node) Broadcast(typeTag proto.MessageType, body []byte, exclude *Peer) {
	n.mu.Lock()
	peers := make([]*Peer, 0, len(n.peers))
	for p := range n.peers {
		if p == exclude {
			continue
		}
		peers = append(peers, p)
	}
	n.mu.Unlock()
	for _, p := range peers {
		if p.State() != StateActive {
			continue
		}
		if err := p.Send(typeTag, body); err != nil {
			n.logf("p2p: broadcast %s to %s: %v", typeTag, p.Conn().RemoteAddr(), err)
		}
	}
}

// Logf exposes the node's logger for daemon-level subsystems (witness,
// gossip) that want their messages on the same stream.
func (n *Node) Logf(format string, args ...interface{}) {
	n.logf(format, args...)
}

// PeerCount returns the current number of active peers.
func (n *Node) PeerCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.peers)
}

// PeerDB exposes the underlying address book (nil before Start).
func (n *Node) PeerDB() *peerdb.Store { return n.peerdb }

func (n *Node) logf(format string, args ...interface{}) {
	if n.cfg.Logger == nil {
		return
	}
	fmt.Fprintf(n.cfg.Logger, format+"\n", args...)
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

func (l *loggingDelegate) OnBlockReceived(p *Peer, body []byte) {
	if l.inner != nil {
		l.inner.OnBlockReceived(p, body)
	}
}

func (l *loggingDelegate) OnTransactionReceived(p *Peer, body []byte) {
	if l.inner != nil {
		l.inner.OnTransactionReceived(p, body)
	}
}

func (l *loggingDelegate) OnAddressesReceived(p *Peer, addrs []proto.AddressInfo) {
	l.n.handlePeerAddresses(p, addrs)
	if l.inner != nil {
		l.inner.OnAddressesReceived(p, addrs)
	}
}

func (l *loggingDelegate) BuildAddressReply(p *Peer) []proto.AddressInfo {
	return l.n.buildAddressReply(p)
}

// Direction.String for log lines.
func (d Direction) String() string {
	if d == DirectionInbound {
		return "inbound"
	}
	return "outbound"
}
