package forwardproxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

// HandleTransparentConn processes one outbound connection captured by an
// iptables REDIRECT (proxy-sidecar enforce-redirect mode). It is the
// transparent-listener analogue of handleConnect, and shares its semantics:
// the same outbound pipeline gates the connection on destination/identity, and
// the bytes are then blind-tunnelled, preserving the agent's end-to-end TLS
// (token-exchange and protocol parsers are no-ops on opaque TLS, exactly as on
// the CONNECT path).
//
// The crucial difference from handleConnect: there is NO HTTP CONNECT request.
// The agent believes it is talking directly to dst, so the proxy must emit no
// protocol bytes back — no "200 Connection Established", no hijack. It simply
// gates, dials dst, and copies bytes both ways. dst is "host:port" recovered
// from SO_ORIGINAL_DST by the transparent listener.
//
// GATING LIMITATION (important before enforce-redirect goes always-on):
// CONNECT carries a hostname in r.Host, so the outbound pipeline gates captured
// CONNECT traffic on the domain (e.g. "api.openai.com:443"). SO_ORIGINAL_DST
// yields only an IP:port, so here pctx.Host is an IP literal. Any host/domain-
// based egress policy (allowlist-by-domain, host->audience routing) therefore
// does NOT match captured bypass traffic the way it matches explicit-proxy
// traffic — the gate runs, but sees an IP. This is acceptable while the operator
// flag keeps enforce-redirect opt-in, but the always-on operator PR MUST first
// either (a) peek the TLS ClientHello SNI here and set pctx.Host to the SNI
// hostname for parity with CONNECT, or (b) consciously accept IP-level egress
// policy for captured traffic. Tracked as a prerequisite for the default flip.
//
// HandleTransparentConn owns clientConn's lifecycle and always closes it.
func (s *Server) HandleTransparentConn(clientConn net.Conn, dst string) {
	defer func() { _ = clientConn.Close() }()

	// Background context: there is no inbound *http.Request to tie cancellation
	// to. Tunnel teardown (either side closing) is what ends the connection;
	// the pipeline Run/Finish calls are short and don't need request scoping.
	ctx := context.Background()

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    http.MethodConnect, // synthetic: opaque tunnel, parity with handleConnect
		Scheme:    "tcp",              // marker: bytes are opaque, not HTTP
		Host:      dst,
		Headers:   http.Header{},
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	defer func() {
		s.OutboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
	}()

	if s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	// Gate on host/identity before opening the tunnel — identical to the
	// CONNECT path. Parsers see no body and degrade gracefully.
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		slog.Warn("transparent-proxy: outbound rejected by policy", "host", dst)
		return
	}

	upstream, err := net.DialTimeout("tcp", dst, connectDialTimeout)
	if err != nil {
		slog.Warn("transparent-proxy: upstream dial failed", "host", dst, "error", err)
		return
	}
	defer func() { _ = upstream.Close() }()

	enableKeepalive(upstream)
	enableKeepalive(clientConn)

	s.recordTunnelOpened(pctx)
	tunnel(clientConn, upstream)
}

// recordTunnelOpened emits the SessionRequest event for an opened opaque
// tunnel (CONNECT or transparent-redirect). Shared by handleConnect and
// HandleTransparentConn. MCP/Inference snapshots are nil by definition (the
// bytes are opaque); Invocations from gate plugins and plugin-public Plugins
// entries are still meaningful.
func (s *Server) recordTunnelOpened(pctx *pipeline.Context) {
	if s.Sessions == nil {
		return
	}
	sid := s.Sessions.ActiveSession()
	if sid == "" {
		sid = session.DefaultSessionID
	}
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionRequest,
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
	}
	if ev.Invocations != nil || plugins != nil {
		s.Sessions.Append(sid, ev)
	}
}

// tunnel bidirectionally copies between two connections until either side
// closes, then propagates the close to the other so both io.Copy goroutines
// exit. Close-on-each-side is idempotent on net.Conn. Shared by handleConnect
// and HandleTransparentConn.
func tunnel(a, b net.Conn) {
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.Close()
		_ = a.Close()
	}()
	_, _ = io.Copy(a, b)
	_ = a.Close()
	_ = b.Close()
}
