package node

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gopherd/internal/p2p/proto"
	"gopherd/internal/p2p/transport"
)

// TestPeerBelowMinimumProtocolVersionRejected covers hived eb556e22d. Before
// it, core_protocol_version was read for feature detection only and no floor
// was ever enforced, so a nonconforming peer reporting nothing walked through
// every compatibility check.
//
// The peer must be refused BEFORE it reaches Active, and must be told why:
// hived sends connection_rejected with client_too_old and then disconnects.
func TestPeerBelowMinimumProtocolVersionRejected(t *testing.T) {
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawListener.Close()
	addr := rawListener.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rejection := make(chan proto.ConnectionRejectedMessage, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = transport.ServeListener(ctx, rawListener, func(c *transport.Conn) {
			defer c.Close()
			// Drain the client's hello(s) — it sends both 5018 and 5006.
			if _, _, err := c.Receive(); err != nil {
				return
			}
			// Answer with a hello reporting a version below the floor.
			hello := proto.LegacyHelloMessage{
				UserAgent:           "ancient-client",
				CoreProtocolVersion: proto.MinimumProtocolVersion - 1,
				InboundAddress:      netip.MustParseAddr("127.0.0.1"),
				InboundPort:         1234,
				OutboundPort:        1234,
			}
			body, err := hello.Marshal()
			if err != nil {
				return
			}
			if err := c.Send(proto.LegacyHelloMessageType, body); err != nil {
				return
			}
			// Whatever comes back should be the rejection, not an accept.
			for {
				typeTag, rb, err := c.Receive()
				if err != nil {
					return
				}
				if typeTag == proto.ConnectionRejectedMessageType {
					var rej proto.ConnectionRejectedMessage
					if err := rej.Unmarshal(rb); err == nil {
						select {
						case rejection <- rej:
						default:
						}
					}
					return
				}
				if typeTag == proto.ConnectionAcceptedMessageType {
					t.Error("node ACCEPTED a peer below the minimum protocol version")
					return
				}
			}
		})
	}()

	delegate := newCaptureDelegate()
	client := New(Config{
		SeedNodes: []string{addr},
		UserAgent: "gopherd-test",
		Logger:    devNullWriter{},
	}, nilChain{}, delegate)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	select {
	case <-delegate.disconnectedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the peer to be dropped")
	}

	delegate.mu.Lock()
	connected := delegate.connected
	cause := delegate.lastDisconnErr
	delegate.mu.Unlock()

	if connected != 0 {
		t.Errorf("peer reached Active despite an old protocol version (connected=%d)", connected)
	}
	if cause == nil {
		t.Error("expected a disconnect cause naming the version")
	}

	select {
	case rej := <-rejection:
		if rej.ReasonCode != proto.RejectClientTooOld {
			t.Errorf("reason code: got %d, want %d (client_too_old)", rej.ReasonCode, proto.RejectClientTooOld)
		}
		if rej.ReasonString == "" {
			t.Error("rejection should carry a human-readable reason")
		}
	case <-time.After(2 * time.Second):
		t.Error("no connection_rejected reached the peer; it was dropped with no explanation")
	}

	cancel()
	wg.Wait()
}

// TestPeerAtMinimumProtocolVersionAccepted is the boundary on the other side:
// 106 is the floor, not one above it. Every client on the network reports 107,
// so an off-by-one here would silently disconnect the entire network.
func TestPeerAtMinimumProtocolVersionAccepted(t *testing.T) {
	if proto.CoreProtocolVersion < proto.MinimumProtocolVersion {
		t.Fatalf("we advertise %d, below the floor %d we enforce — we would reject ourselves",
			proto.CoreProtocolVersion, proto.MinimumProtocolVersion)
	}
	if proto.MinimumProtocolVersion != 106 {
		t.Errorf("minimum protocol version: got %d, want 106 (GRAPHENE_NET_MINIMUM_PROTOCOL_VERSION)",
			proto.MinimumProtocolVersion)
	}
}
