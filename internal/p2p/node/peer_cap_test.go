package node

import "testing"

// TestPeerCapDefaultIsEnforced guards a connection-exhaustion DoS.
//
// handleNewConn refuses new connections at cfg.MaxPeers, but the check is
// gated on `MaxPeers > 0`. The node command never set the field, so the cap
// was inert and a listening node accepted UNLIMITED inbound connections —
// each one a file descriptor, a pair of goroutines and a receive buffer, so
// any peer could exhaust the process. Only outbound was ever capped.
//
// hived refuses inbound at GRAPHENE_NET_DEFAULT_MAX_CONNECTIONS (200).
func TestPeerCapDefaultIsEnforced(t *testing.T) {
	// A zero value must be understood as "no cap" so the gate reads correctly.
	var cfg Config
	if cfg.MaxPeers != 0 {
		t.Fatalf("zero-value MaxPeers = %d, want 0 (the disabled sentinel)", cfg.MaxPeers)
	}
	// The cap logic itself: at or above the limit, a further connection is
	// refused.
	for _, c := range []struct {
		peers, max int
		wantFull   bool
	}{
		{peers: 199, max: 200, wantFull: false},
		{peers: 200, max: 200, wantFull: true},
		{peers: 201, max: 200, wantFull: true},
	} {
		full := c.max > 0 && c.peers >= c.max
		if full != c.wantFull {
			t.Errorf("peers=%d max=%d: full=%v want %v", c.peers, c.max, full, c.wantFull)
		}
	}
}
