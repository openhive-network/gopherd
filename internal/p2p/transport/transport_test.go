package transport

import (
	"context"
	"sync"
	"testing"
	"time"

	"gopherd/internal/p2p/proto"
)

// TestHandshakeAndRoundTrip spawns a listener and a dialer on 127.0.0.1
// and verifies that the stcp handshake completes both ways AND that a
// HelloMessage-shaped payload survives the encrypt/decrypt round-trip.
//
// The "HelloMessage" body here is just a sentinel byte slice — the real
// FC encoding lives in agent-p2p-msg. Transport-layer parity only needs
// to prove (1) the framing+padding is correct and (2) the AES streams
// stay in sync.
func TestHandshakeAndRoundTrip(t *testing.T) {
	l, err := ListenerAddr("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Server: accept exactly one connection, read one message, echo it back.
	var serverErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := ServeListener(ctx, l, func(c *Conn) {
			defer c.Close()
			typeTag, body, err := c.Receive()
			if err != nil {
				serverErr = err
				return
			}
			if err := c.Send(typeTag, body); err != nil {
				serverErr = err
				return
			}
		})
		// ServeListener returns ctx.Err() once we Cancel; that's expected.
		if err != nil && ctx.Err() == nil {
			serverErr = err
		}
	}()

	// Client: dial, send a body, receive the echo.
	client, err := Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	want := []byte("hello, peer — this is a stcp test payload")
	if err := client.Send(proto.HelloMessageType, want); err != nil {
		t.Fatalf("Send: %v", err)
	}
	gotType, gotBody, err := client.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if gotType != proto.HelloMessageType {
		t.Errorf("type: got %d want %d", gotType, proto.HelloMessageType)
	}
	if string(gotBody) != string(want) {
		t.Errorf("body: got %q want %q", gotBody, want)
	}

	// Pubkeys must mirror across the link.
	// (Server closed its end after echoing; we can still inspect what
	// the client recorded.)
	if client.RemotePublicKey().IsZero() {
		t.Errorf("client did not record remote pubkey")
	}
	if client.LocalPublicKey().IsZero() {
		t.Errorf("client did not record local pubkey")
	}

	cancel()
	wg.Wait()
	if serverErr != nil {
		t.Fatalf("server: %v", serverErr)
	}
}

// TestSendsLargerThanOneBlock verifies the multi-block read path on
// Receive: any payload > 8 bytes will spill past the first AES block.
func TestSendsLargerThanOneBlock(t *testing.T) {
	l, err := ListenerAddr("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = ServeListener(ctx, l, func(c *Conn) {
			defer c.Close()
			for {
				typeTag, body, err := c.Receive()
				if err != nil {
					return
				}
				if err := c.Send(typeTag, body); err != nil {
					return
				}
			}
		})
	}()

	client, err := Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, n := range []int{0, 1, 7, 8, 9, 15, 16, 17, 1000, 32 * 1024} {
		body := make([]byte, n)
		for i := range body {
			body[i] = byte(i * 7)
		}
		if err := client.Send(proto.HelloMessageType, body); err != nil {
			t.Fatalf("Send n=%d: %v", n, err)
		}
		_, got, err := client.Receive()
		if err != nil {
			t.Fatalf("Receive n=%d: %v", n, err)
		}
		if string(got) != string(body) {
			t.Fatalf("size %d: round-trip mismatch", n)
		}
	}

	cancel()
	wg.Wait()
}
