package node

import (
	"net"
	"testing"
	"time"
)

// TestDiscoveryPropagatesEndpoints stands up THREE nodes in a chain
// (A↔B configured, C↔B configured, A and C have NO direct seed of each
// other) and verifies that after a few discovery ticks, A learns about
// C via B's address_message reply and dials it.
//
// Layout:
//
//   nodeA seed=B
//   nodeB endpoint=B (no seeds)
//   nodeC seed=B
//
// Expected: B is the rendezvous; once both A and C have peered with B,
// B's address_reply contains the other side's endpoint, and the dialer
// in A and C wires up the third leg.
func TestDiscoveryPropagatesEndpoints(t *testing.T) {
	// Speed up the dial loop for tests.
	prevTick := DialTickInterval
	DialTickInterval = 50 * time.Millisecond
	defer func() { DialTickInterval = prevTick }()

	// Pre-bind to learn A and B's addresses without races.
	listenerB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrB := listenerB.Addr().String()
	listenerB.Close()

	listenerA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrA := listenerA.Addr().String()
	listenerA.Close()

	listenerC, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrC := listenerC.Addr().String()
	listenerC.Close()

	// node-B: rendezvous, no seeds.
	delB := newCaptureDelegate()
	nodeB := New(Config{
		Endpoint:  addrB,
		UserAgent: "ghive-B",
		Logger:    devNullWriter{},
	}, nilChain{}, delB)
	if err := nodeB.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeB.Stop()

	// Give B's listener a moment to bind.
	time.Sleep(50 * time.Millisecond)

	// node-A: seed=B only.
	delA := newCaptureDelegate()
	nodeA := New(Config{
		Endpoint:  addrA,
		SeedNodes: []string{addrB},
		UserAgent: "ghive-A",
		Logger:    devNullWriter{},
	}, nilChain{}, delA)
	if err := nodeA.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeA.Stop()

	// node-C: seed=B only.
	delC := newCaptureDelegate()
	nodeC := New(Config{
		Endpoint:  addrC,
		SeedNodes: []string{addrB},
		UserAgent: "ghive-C",
		Logger:    devNullWriter{},
	}, nilChain{}, delC)
	if err := nodeC.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeC.Stop()

	// Wait for EVERY condition asserted below, not just the peer counts.
	// A's peerdb entry for C is written on connect, slightly after PeerCount
	// rises, so waiting only on the counts left a window where the counts were
	// satisfied but the peerdb assertion had not landed yet — a flake that
	// showed up reliably under -race, which widens that window.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, inDB := nodeA.PeerDB().Get(addrC)
		if nodeA.PeerCount() >= 2 && nodeC.PeerCount() >= 2 && inDB {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := nodeA.PeerCount(); got < 2 {
		t.Errorf("nodeA did not discover C via B: PeerCount=%d (want >=2)", got)
	}
	if got := nodeC.PeerCount(); got < 2 {
		t.Errorf("nodeC did not discover A via B: PeerCount=%d (want >=2)", got)
	}
	// And A's peer DB should know about C now.
	if _, ok := nodeA.PeerDB().Get(addrC); !ok {
		t.Errorf("nodeA's peer DB does not contain %s", addrC)
	}
}

// TestSelfDialProtection verifies the discovery dialer skips an
// endpoint that maps to our own listener.
func TestSelfDialProtection(t *testing.T) {
	prevTick := DialTickInterval
	DialTickInterval = 30 * time.Millisecond
	defer func() { DialTickInterval = prevTick }()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	// Node lists ITSELF as a seed — should be skipped silently.
	del := newCaptureDelegate()
	n := New(Config{
		Endpoint:  addr,
		SeedNodes: []string{addr},
		UserAgent: "ghive-self",
		Logger:    devNullWriter{},
	}, nilChain{}, del)
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	defer n.Stop()

	// Give the dial loop several ticks to attempt and fail.
	time.Sleep(300 * time.Millisecond)

	if got := n.PeerCount(); got != 0 {
		t.Errorf("self-dial protection breached: got %d peers, want 0", got)
	}
	// And no spurious connect callbacks.
	del.mu.Lock()
	if del.connected != 0 {
		t.Errorf("OnPeerConnected fired %d times for self-dial", del.connected)
	}
	del.mu.Unlock()
}

// TestPeerDBPersistsAcrossRestart verifies that an endpoint discovered
// in one Node instance survives into a second Node opened against the
// same data dir.
func TestPeerDBPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/peers.json"

	// First incarnation: discover one endpoint.
	{
		n := New(Config{
			SeedNodes:  []string{"10.99.99.99:1"},
			UserAgent:  "ghive-restart",
			Logger:     devNullWriter{},
			PeerDBPath: dbPath,
		}, nilChain{}, nil)
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		// Inject an extra learned address.
		n.PeerDB().Discover("10.99.99.100:1")
		_ = n.PeerDB().Save()
		n.Stop()
	}

	// Second incarnation: peerdb should already know both endpoints.
	{
		n := New(Config{
			SeedNodes:  []string{"10.99.99.99:1"},
			UserAgent:  "ghive-restart",
			Logger:     devNullWriter{},
			PeerDBPath: dbPath,
		}, nilChain{}, nil)
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		defer n.Stop()
		if _, ok := n.PeerDB().Get("10.99.99.99:1"); !ok {
			t.Errorf("seed endpoint not persisted across restart")
		}
		if _, ok := n.PeerDB().Get("10.99.99.100:1"); !ok {
			t.Errorf("discovered endpoint not persisted across restart")
		}
	}
}
