package tlsbridge

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// Engine bundles everything the forward proxy needs to bridge TLS.
// A nil *Engine means the bridge is disabled.
type Engine struct {
	Decision *Decision
	Term     *Terminator
	Skip     *SkipSet
	Upstream *http.Client
	CAPEM    []byte
}

// RunTrustSelfCheck logs a loud WARN when the bridge CA is not present in the
// trust file the agent runtime is told to use (SSL_CERT_FILE / NODE_EXTRA_CA_CERTS
// / REQUESTS_CA_BUNDLE). A trust-miss is then a visible signal, not an opaque
// in-agent handshake error. Best-effort. In Phase 1 (test-only) no agent trust
// env is set, so this simply notes that egress will safely tunnel.
func RunTrustSelfCheck(caPEM []byte) {
	want := strings.TrimSpace(string(caPEM))
	for _, env := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE"} {
		p := os.Getenv(env)
		if p == "" {
			continue
		}
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), want) {
			slog.Info("tls-bridge trust self-check OK", "env", env, "path", p)
			return
		}
	}
	slog.Warn("tls-bridge trust self-check: CA not found in any agent trust file " +
		"(SSL_CERT_FILE/NODE_EXTRA_CA_CERTS/REQUESTS_CA_BUNDLE); agent will not trust " +
		"minted leaves and egress will safely tunnel (expected in Phase 1 / test-only)")
}
