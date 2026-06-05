// Package transparentproxy implements an outbound transparent proxy listener
// for proxy-sidecar enforce-redirect mode. Unlike the forward proxy (which
// requires the agent to honor HTTP_PROXY and speak explicit CONNECT), this
// listener receives connections that iptables transparently REDIRECTed to it.
// The agent believes it is connecting directly to the destination, so the
// listener recovers the original destination from the kernel via
// SO_ORIGINAL_DST and hands the connection to a ConnHandler that gates and
// blind-tunnels it — emitting no proxy-protocol bytes back to the agent.
//
// This is the Go equivalent of Envoy's original_dst listener filter +
// ORIGINAL_DST cluster used by envoy-sidecar mode; the auth pipeline behind
// the ConnHandler is identical to the forward proxy's CONNECT path.
package transparentproxy

import (
	"errors"
	"log/slog"
	"net"
)

// ConnHandler processes one accepted outbound connection whose original
// destination has been recovered. dst is "host:port". The handler owns the
// connection's lifecycle, including closing it.
type ConnHandler func(conn net.Conn, dst string)

// Server accepts iptables-REDIRECTed connections and dispatches them to a
// ConnHandler after recovering each connection's original destination.
type Server struct {
	handle ConnHandler
}

// NewServer returns a transparent proxy server that dispatches each accepted,
// destination-recovered connection to handle. In proxy-sidecar mode handle is
// forwardproxy.Server.HandleTransparentConn, so transparent and explicit-proxy
// egress share one auth pipeline.
func NewServer(handle ConnHandler) *Server {
	if handle == nil {
		// Defensive: a nil handler would panic at dispatch and take down the
		// process. Fall back to closing the connection so a misconfiguration
		// degrades to "no capture" rather than a crash.
		handle = func(conn net.Conn, _ string) {
			slog.Error("transparent-proxy: nil connection handler; closing connection",
				"remote", conn.RemoteAddr().String())
			_ = conn.Close()
		}
	}
	return &Server{handle: handle}
}

// Serve accepts connections on ln until it is closed, recovering each
// connection's original destination and dispatching to the handler in its own
// goroutine. Returns nil when ln is closed (graceful shutdown), or the accept
// error otherwise.
func (s *Server) Serve(ln *net.TCPListener) error {
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.dispatch(conn)
	}
}

func (s *Server) dispatch(conn *net.TCPConn) {
	dst, err := originalDst(conn)
	if err != nil {
		// No recoverable original destination means this connection did not
		// arrive via the REDIRECT (e.g. a direct dial to the listener port).
		// Drop it rather than guess a destination — we will not blind-tunnel
		// to an attacker-chosen target.
		slog.Warn("transparent-proxy: dropping connection with no original destination",
			"remote", conn.RemoteAddr().String(), "error", err)
		_ = conn.Close()
		return
	}
	// Defense-in-depth against a self-redirect loop: a genuinely REDIRECTed
	// connection never has a loopback original destination (the enforce-redirect
	// iptables rules RETURN loopback before the REDIRECT). So a loopback dst here
	// means a direct dial to the listener port, where SO_ORIGINAL_DST reports the
	// listener's own address — tunnelling to it would spiral into ever more
	// connections/goroutines. Drop it.
	if host, _, splitErr := net.SplitHostPort(dst); splitErr == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			slog.Warn("transparent-proxy: dropping connection whose original destination is loopback (not a redirect; would self-loop)",
				"remote", conn.RemoteAddr().String(), "dst", dst)
			_ = conn.Close()
			return
		}
	}
	s.handle(conn, dst)
}
