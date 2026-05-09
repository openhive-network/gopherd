package node

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hived-go/internal/p2p/proto"
	"hived-go/internal/p2p/transport"
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
			if err := c.Send(proto.ConnectionRejectedMessage, []byte("nope")); err != nil {
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

// Sanity: the atomic.Int32 in Peer doesn't drift if no goroutine writes.
func TestPeerInitialState(t *testing.T) {
	dummy := &Peer{}
	dummy.state.Store(int32(StateConnecting))
	if dummy.State() != StateConnecting {
		t.Fatalf("got %v want %v", dummy.State(), StateConnecting)
	}
	// Suppress unused-import warning if this is the only place we use atomic
	// outside peer.go's own atomic ops.
	_ = atomic.Int32{}
	_ = errors.New
}
