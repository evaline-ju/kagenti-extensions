package tlsbridge

import (
	"crypto/tls"
	"net"
)

// Terminator wraps a sniffed client conn as a tls.Server, using the Minter to
// forge a per-SNI leaf. ALPN offers h2 + http/1.1.
type Terminator struct {
	minter *Minter
}

func NewTerminator(m *Minter) *Terminator { return &Terminator{minter: m} }

// Terminate completes the server-side TLS handshake against the agent. host is
// the dialed name/IP, used to mint when the ClientHello carries no SNI.
func (t *Terminator) Terminate(client net.Conn, host string) (*tls.Conn, error) {
	cfg := &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if chi.ServerName != "" {
				return t.minter.GetCertificateForHost(chi.ServerName)
			}
			return t.minter.GetCertificateForHost(host)
		},
	}
	conn := tls.Server(client, cfg)
	if err := conn.Handshake(); err != nil {
		return nil, err
	}
	return conn, nil
}
