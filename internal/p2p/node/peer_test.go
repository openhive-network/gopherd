package node

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"gopherd/internal/p2p/proto"
	"gopherd/internal/p2p/transport"
)

// captureDelegate is a test helper that records connect/disconnect events.
type captureDelegate struct {
	mu             sync.Mutex
	connected      int
	disconnected   int
	lastDisconnErr error
	connectedCh    chan struct{}
	disconnectedCh chan struct{}
}

func newCaptureDelegate() *captureDelegate {
	return &captureDelegate{
		connectedCh:    make(chan struct{}, 4),
		disconnectedCh: make(chan struct{}, 4),
	}
}

func (d *captureDelegate) OnPeerConnected(p *Peer) {
	d.mu.Lock()
	d.connected++
	d.mu.Unlock()
	select {
	case d.connectedCh <- struct{}{}:
	default:
	}
}

func (d *captureDelegate) OnPeerDisconnected(p *Peer, err error) {
	d.mu.Lock()
	d.disconnected++
	d.lastDisconnErr = err
	d.mu.Unlock()
	select {
	case d.disconnectedCh <- struct{}{}:
	default:
	}
}

func (d *captureDelegate) OnMessage(p *Peer, t proto.MessageType, body []byte) {}

func (d *captureDelegate) OnBlockReceived(p *Peer, body []byte) {}

func (d *captureDelegate) OnTransactionReceived(p *Peer, body []byte) {}

func (d *captureDelegate) OnAddressesReceived(p *Peer, addrs []proto.AddressInfo) {}

func (d *captureDelegate) BuildAddressReply(p *Peer) []proto.AddressInfo { return nil }

// TestPeerCleanCloseOnRejection wires two ghive Nodes together over a real
// loopback TCP socket and forces one side to send a ConnectionRejected
// message. The other side's Peer should:
//   - never reach StateActive
//   - close cleanly without panicking
//   - surface a non-nil cause via OnPeerDisconnected
func TestPeerCleanCloseOnRejection(t *testing.T) {
	// Pre-bind to discover the port.
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawListener.Close()
	addr := rawListener.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Server: accept ONE connection, do the stcp handshake via the
	// transport layer, then immediately send a ConnectionRejected
	// message instead of an accept. Don't even read the client's hello.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = transport.ServeListener(ctx, rawListener, func(c *transport.Conn) {
			defer c.Close()
			// Read the client's HelloMessage so the wire stays in sync,
			// then reject.
			if _, _, err := c.Receive(); err != nil {
				t.Logf("server receive: %v", err)
				return
			}
			if err := c.Send(proto.ConnectionRejectedMessageType, []byte("nope")); err != nil {
				t.Logf("server send: %v", err)
				return
			}
			// Give the client a moment to ingest the rejection before
			// we close (otherwise the client would see EOF first).
			time.Sleep(50 * time.Millisecond)
		})
	}()

	// Client: a one-peer ghive Node dialing the server.
	delegate := newCaptureDelegate()
	clientCfg := Config{
		SeedNodes: []string{addr},
		UserAgent: "ghive-test-client",
		Logger:    devNullWriter{},
	}
	client := New(clientCfg, nilChain{}, delegate)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	// Wait for the disconnect callback.
	select {
	case <-delegate.disconnectedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnPeerDisconnected")
	}

	delegate.mu.Lock()
	if delegate.connected != 0 {
		t.Errorf("connected count: got %d want 0", delegate.connected)
	}
	if delegate.disconnected == 0 {
		t.Errorf("disconnected count: got 0 want >=1")
	}
	if delegate.lastDisconnErr == nil {
		t.Errorf("expected non-nil disconnect cause")
	}
	delegate.mu.Unlock()

	cancel()
	wg.Wait()
}

// TestTwoNodesPeerEachOther stands up two real Nodes back-to-back and
// verifies that one dialing the other's listener results in BOTH delegates
// firing OnPeerConnected (i.e. both sides reach StateActive).
//
// Demonstrates: hello round-trip + connection_accepted round-trip work
// over the real listener+dialer plumbing.
func TestTwoNodesPeerEachOther(t *testing.T) {
	listenerA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrA := listenerA.Addr().String()
	listenerA.Close()

	delA := newCaptureDelegate()
	delB := newCaptureDelegate()

	cfgA := Config{
		Endpoint:  addrA,
		UserAgent: "ghive-A",
		Logger:    devNullWriter{},
	}
	nodeA := New(cfgA, nilChain{}, delA)
	if err := nodeA.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeA.Stop()

	// Give A's listener a moment to bind.
	time.Sleep(50 * time.Millisecond)

	cfgB := Config{
		SeedNodes: []string{addrA},
		UserAgent: "ghive-B",
		Logger:    devNullWriter{},
	}
	nodeB := New(cfgB, nilChain{}, delB)
	if err := nodeB.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeB.Stop()

	// Both delegates should fire OnPeerConnected within a generous window.
	if !waitForCount(&delA.connected, &delA.mu, 1, 3*time.Second) {
		t.Errorf("nodeA never saw a peer connect (connected=%d)", delA.connected)
	}
	if !waitForCount(&delB.connected, &delB.mu, 1, 3*time.Second) {
		t.Errorf("nodeB never saw a peer connect (connected=%d)", delB.connected)
	}
}

// waitForCount polls counter under mu until it reaches want or the deadline
// elapses.
func waitForCount(counter *int, mu *sync.Mutex, want int, dur time.Duration) bool {
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := *counter
		mu.Unlock()
		if got >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// nilChain is a ChainView that pretends the head block is 0. Used by tests
// that don't want to spin up the full state machinery.
type nilChain struct{}

func (nilChain) HeadBlock() (uint32, error) { return 0, nil }

// devNullWriter discards everything written to it; quiets the test logs.
type devNullWriter struct{}

func (devNullWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestParseLegacyAddressMessage exercises the legacy_address_message
// parser used by peer.go to consume mainnet hived's address-gossip.
// We hand-build a body with two well-formed entries (one IPv4 unicast,
// one loopback that should be filtered) + check we extract just one.
//
// fc::ip::address packs as a LE uint32 of the IP — e.g. wire bytes
// {01, 02, 03, 04} decode to address 4.3.2.1 (we reverse the bytes).
// The test pins this round-trip explicitly because interop with the
// existing proto.ReadLegacyAddress (which reads bytes-as-network-order)
// is wrong: it works in isolation but not against real hived.
func TestParseLegacyAddressMessage(t *testing.T) {
	// Two entries:
	//   - Wire {1, 2, 3, 4} → 4.3.2.1:2001 (unicast, kept)
	//   - Wire {1, 0, 0, 127} → 127.0.0.1:2001 (loopback, filtered)
	const entrySize = 53
	body := []byte{0x02} // varint 2
	mk := func(wireIP [4]byte, port uint16) []byte {
		b := make([]byte, entrySize)
		copy(b[0:4], wireIP[:])
		b[4] = byte(port)
		b[5] = byte(port >> 8)
		// last_seen, latency, node_id, direction, firewalled — all zero,
		// the parser doesn't care.
		return b
	}
	body = append(body, mk([4]byte{1, 2, 3, 4}, 2001)...)
	body = append(body, mk([4]byte{1, 0, 0, 127}, 2001)...)

	got, err := parseLegacyAddressMessage(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries; want 1 (loopback should be filtered)", len(got))
	}
	if got[0].RemoteEndpoint.Addr.String() != "4.3.2.1:2001" {
		t.Errorf("entry 0 endpoint = %q, want 4.3.2.1:2001", got[0].RemoteEndpoint.Addr.String())
	}
}

// TestParseLegacyAddressMessageRejectsTruncated verifies the parser
// returns an error on a body that claims more entries than fit.
func TestParseLegacyAddressMessageRejectsTruncated(t *testing.T) {
	// Claim 5 entries, supply 0 bodies.
	body := []byte{0x05}
	if _, err := parseLegacyAddressMessage(body); err == nil {
		t.Fatal("expected error on truncated body")
	}
}

// Sanity: the atomic.Int32 in Peer doesn't drift if no goroutine writes.
func TestPeerInitialState(t *testing.T) {
	dummy := &Peer{}
	dummy.state.Store(int32(StateConnecting))
	if dummy.State() != StateConnecting {
		t.Fatalf("got %v want %v", dummy.State(), StateConnecting)
	}
}
