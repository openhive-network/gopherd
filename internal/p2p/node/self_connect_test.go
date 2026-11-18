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

// A hello carrying the key of one of our own connections is ourselves (we
// dialled our public address, or one a peer advertised back to us); it is
// refused with connected_to_self and the endpoint is never dialled again.
// The fake server stands in for "us" by echoing our own key back.
func TestHelloWithOwnKeyRefusedAsSelf(t *testing.T) {
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
			if _, _, err := c.Receive(); err != nil {
				return
			}
			var key proto.PublicKeyData
			rk := c.RemotePublicKey()
			copy(key[:], rk[:])
			hello := proto.HelloMessage{
				UserAgent:           "mirror",
				CoreProtocolVersion: proto.CoreProtocolVersion,
				InboundAddress:      netip.MustParseAddr("127.0.0.1"),
				InboundPort:         1234,
				OutboundPort:        1234,
				NodePublicKey:       key,
				UserData:            proto.NewVarObject(),
			}
			b, _ := hello.Marshal()
			if c.Send(proto.HelloMessageType, b) != nil {
				return
			}
			for {
				typ, rb, err := c.Receive()
				if err != nil {
					return
				}
				if typ == proto.ConnectionRejectedMessageType {
					var rej proto.ConnectionRejectedMessage
					if rej.Unmarshal(rb) == nil {
						rejection <- rej
					}
					return
				}
				if typ == proto.ConnectionAcceptedMessageType {
					t.Error("accepted a hello carrying our own key")
					return
				}
			}
		})
	}()

	d := newCaptureDelegate()
	client := New(Config{SeedNodes: []string{addr}, UserAgent: "gopherd-test", Logger: devNullWriter{}}, nilChain{}, d)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	select {
	case rej := <-rejection:
		if rej.ReasonCode != proto.RejectConnectedToSelf {
			t.Errorf("reason code %d, want connected_to_self", rej.ReasonCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hello with our own key was not refused")
	}
	client.mu.Lock()
	_, self := client.selfEndpoints[addr]
	client.mu.Unlock()
	if !self {
		t.Error("the endpoint that reached ourselves was not recorded as ours")
	}
	cancel()
	wg.Wait()
}
