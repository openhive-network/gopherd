package transport

import (
	"context"
	"errors"
	"net"
)

// Handler is invoked once per accepted, fully-handshaken inbound peer.
// The transport owns conn until handler returns; the handler should
// hand it off to a long-lived goroutine if it wants to keep using it,
// or close it explicitly.
type Handler func(*Conn)

// Listen binds to endpoint, accepts inbound TCP connections, runs the
// stcp handshake on each, and dispatches to handler in its own
// goroutine.
//
// Listen blocks until ctx is canceled or the underlying listener errors
// out. Accept errors that are temporary (per net.Error.Temporary) are
// logged-and-continued; permanent errors break the loop.
func Listen(ctx context.Context, endpoint string, handler Handler) error {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", endpoint)
	if err != nil {
		return err
	}
	defer l.Close()
	// Close the listener when ctx fires, breaking Accept.
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		raw, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go func(raw net.Conn) {
			conn, err := performHandshake(raw)
			if err != nil {
				raw.Close()
				return
			}
			handler(conn)
		}(raw)
	}
}

// ListenerAddr is a convenience that opens a listener and returns it
// alongside its bound address. Useful for tests that bind to :0 and
// need to discover the assigned port.
func ListenerAddr(endpoint string) (net.Listener, error) {
	return net.Listen("tcp", endpoint)
}

// ServeListener runs the accept loop on an already-opened net.Listener.
// Mirrors Listen but lets the caller pre-bind (e.g. for :0 + port
// discovery).
func ServeListener(ctx context.Context, l net.Listener, handler Handler) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		raw, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go func(raw net.Conn) {
			conn, err := performHandshake(raw)
			if err != nil {
				raw.Close()
				return
			}
			handler(conn)
		}(raw)
	}
}
