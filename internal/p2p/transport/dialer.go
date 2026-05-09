package transport

import (
	"context"
	"net"
)

// Dial opens a TCP connection to endpoint and runs the stcp handshake.
// On success, returns an established *Conn. On failure, returns the
// underlying net error or a handshake-derived error.
//
// endpoint is a "host:port" string suitable for net.Dial.
func Dial(ctx context.Context, endpoint string) (*Conn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, err
	}
	conn, err := performHandshake(raw)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}
