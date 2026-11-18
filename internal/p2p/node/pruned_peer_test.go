package node

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"gopherd/internal/p2p/proto"
	"gopherd/internal/p2p/transport"
)

// handshakeDelegate is a captureDelegate that also describes a chain to the
// handshake (HandshakeView).
type handshakeDelegate struct {
	*captureDelegate
	head, tail uint32
}

func (d handshakeDelegate) HelloHead() (uint32, [20]byte, uint32, bool) {
	return d.head, [20]byte{0, 0, 0, 7}, 1_800_000_000, true
}
func (d handshakeDelegate) ServableTail() uint32 { return d.tail }

func TestPeerNeedsBlocksWePruned(t *testing.T) {
	for _, tc := range []struct {
		peer, tail uint32
		want       bool
	}{
		{0, 0, false}, {0, 20, false}, {0, 21, true}, {979, 1000, true}, {980, 1000, false},
	} {
		if got := PeerNeedsBlocksWePruned(tc.peer, tc.tail); got != tc.want {
			t.Errorf("peer %d tail %d: got %v", tc.peer, tc.tail, got)
		}
	}
}

// hived 7ae0cbdfb: our hello carries last_known_block_number, and a peer
// whose hello says it is far below the oldest block we serve is refused at
// the handshake with a reason, never accepted.
func TestHelloCarriesHeadAndRefusesPeerBelowTail(t *testing.T) {
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawListener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ourHead := make(chan uint32, 1)
	rejection := make(chan proto.ConnectionRejectedMessage, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = transport.ServeListener(ctx, rawListener, func(c *transport.Conn) {
			defer c.Close()
			typ, body, err := c.Receive()
			if err != nil {
				return
			}
			if typ == proto.HelloMessageType {
				var hm proto.HelloMessage
				if hm.Unmarshal(body) == nil {
					if n, ok := helloLastKnownBlock(hm.UserData); ok {
						ourHead <- n
					}
				}
			}
			ud := proto.NewVarObject()
			ud.Set("last_known_block_number", proto.NewVariantUint64(5))
			hello := proto.HelloMessage{
				UserAgent:           "far-behind",
				CoreProtocolVersion: proto.CoreProtocolVersion,
				InboundAddress:      netip.MustParseAddr("127.0.0.1"),
				InboundPort:         1234,
				OutboundPort:        1234,
				UserData:            ud,
			}
			b, err := hello.Marshal()
			if err != nil || c.Send(proto.HelloMessageType, b) != nil {
				return
			}
			for {
				typ, rb, err := c.Receive()
				if err != nil {
					return
				}
				switch typ {
				case proto.ConnectionRejectedMessageType:
					var rej proto.ConnectionRejectedMessage
					if rej.Unmarshal(rb) == nil {
						rejection <- rej
					}
					return
				case proto.ConnectionAcceptedMessageType:
					t.Error("accepted a peer that needs blocks we pruned")
					return
				}
			}
		})
	}()

	d := handshakeDelegate{captureDelegate: newCaptureDelegate(), head: 5000, tail: 3000}
	client := New(Config{SeedNodes: []string{rawListener.Addr().String()}, UserAgent: "gopherd-test", Logger: devNullWriter{}}, nilChain{}, d)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	select {
	case n := <-ourHead:
		if n != 5000 {
			t.Errorf("our hello said last_known_block_number=%d, want 5000", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("our hello carried no last_known_block_number")
	}
	select {
	case rej := <-rejection:
		if !strings.Contains(rej.ReasonString, "#3000") {
			t.Errorf("rejection reason %q does not name our tail", rej.ReasonString)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer below our tail was not refused")
	}
	cancel()
	wg.Wait()
}

// A peer that accepts us and then closes with closing_due_to_error (what a
// pruned hived does to a peer it cannot help, 7ae0cbdfb) must count as a
// failed dial so the backoff applies; before, a completed handshake reset
// the failures and we redialled every second.
func TestClosingDueToErrorAfterHandshakeCountsAsFailure(t *testing.T) {
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawListener.Close()
	addr := rawListener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = transport.ServeListener(ctx, rawListener, func(c *transport.Conn) {
			defer c.Close()
			if _, _, err := c.Receive(); err != nil {
				return
			}
			hello := proto.HelloMessage{
				UserAgent:           "pruned-hived",
				CoreProtocolVersion: proto.CoreProtocolVersion,
				InboundAddress:      netip.MustParseAddr("127.0.0.1"),
				InboundPort:         1234,
				OutboundPort:        1234,
				UserData:            proto.NewVarObject(),
			}
			b, _ := hello.Marshal()
			_ = c.Send(proto.HelloMessageType, b)
			_ = c.Send(proto.ConnectionAcceptedMessageType, nil)
			time.Sleep(200 * time.Millisecond)
			cc := proto.ClosingConnectionMessage{ReasonForClosing: "I can't help you sync", ClosingDueToError: true}
			b, _ = cc.Marshal()
			_ = c.Send(proto.ClosingConnectionMessageType, b)
			time.Sleep(200 * time.Millisecond)
		})
	}()

	d := newCaptureDelegate()
	client := New(Config{SeedNodes: []string{addr}, UserAgent: "gopherd-test", Logger: devNullWriter{}}, nilChain{}, d)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	select {
	case <-d.connectedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not complete")
	}
	select {
	case <-d.disconnectedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("peer was not dropped")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if e, ok := client.peerdb.Get(addr); ok && e.ConnFailures > 0 {
			break
		}
		if time.Now().After(deadline) {
			e, _ := client.peerdb.Get(addr)
			t.Fatalf("failures after a closing_due_to_error drop: %+v, want > 0", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
}
