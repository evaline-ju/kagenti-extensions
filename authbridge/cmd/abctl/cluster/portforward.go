package cluster

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"
)

// freeLocalPort returns an unused TCP port on 127.0.0.1.
//
// There is a TOCTOU race between this call and whoever binds next; in
// practice the kubectl subprocess binds within milliseconds and we accept
// the small window of risk for a much simpler design.
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForAccept blocks until 127.0.0.1:port accepts a TCP connection or
// the context is cancelled. Used to detect that `kubectl port-forward`
// has finished setting up its local listener.
func waitForAccept(ctx context.Context, port int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", addr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
