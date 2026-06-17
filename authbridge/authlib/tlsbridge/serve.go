package tlsbridge

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"

	"golang.org/x/net/http2"
)

// oneConnListener serves exactly one already-accepted conn to http.Server.Serve,
// so keep-alive (multiple requests on the same TLS conn) works.
type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil, net.ErrClosed
	}
	c := l.conn
	l.conn = nil
	return c, nil
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "tls-bridge" }

// ServeConn drives an already-terminated TLS conn through handler with HTTP
// keep-alive, negotiating h2 when ALPN selected it.
func ServeConn(tconn *tls.Conn, handler http.Handler) {
	srv := &http.Server{Handler: handler}
	if tconn.ConnectionState().NegotiatedProtocol == "h2" {
		h2s := &http2.Server{}
		h2s.ServeConn(tconn, &http2.ServeConnOpts{Handler: handler, BaseConfig: srv})
		return
	}
	_ = srv.Serve(&oneConnListener{conn: tconn})
}
