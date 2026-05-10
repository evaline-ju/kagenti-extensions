package reverseproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

type mockVerifier struct {
	claims *validation.Claims
	err    error
}

func (m *mockVerifier) Verify(_ context.Context, _ string, _ string) (*validation.Claims, error) {
	return m.claims, m.err
}

func inboundPipelineFromAuth(t *testing.T, a *auth.Auth) *pipeline.Holder {
	t.Helper()
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{plugintesting.NewJWTValidation(a, false)})
	if err != nil {
		t.Fatalf("building inbound pipeline: %v", err)
	}
	return pipeline.NewHolder(p)
}

func TestReverseProxy_AllowedRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "my-app"},
	})
	srv, err := NewServer(inboundPipelineFromAuth(t, a), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestReverseProxy_DeniedRequest(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("invalid token")},
		Identity: auth.IdentityConfig{Audience: "my-app"},
	})
	srv, _ := NewServer(inboundPipelineFromAuth(t, a), nil, "http://localhost:9999")

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestReverseProxy_BypassPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("agent-card"))
	}))
	defer backend.Close()

	matcher, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("should not be called")},
		Bypass:   matcher,
	})
	srv, _ := NewServer(inboundPipelineFromAuth(t, a), nil, backend.URL)

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// No auth header, but bypass path should be allowed
	req, _ := http.NewRequest("GET", proxy.URL+"/.well-known/agent.json", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for bypass path", resp.StatusCode)
	}
}

// --- Body Buffering Tests ---

// bodyRecorderPlugin records whether it received a body during OnRequest.
type bodyRecorderPlugin struct {
	receivedBody []byte
}

func (p *bodyRecorderPlugin) Name() string { return "body-recorder" }
func (p *bodyRecorderPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{BodyAccess: true}
}
func (p *bodyRecorderPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.receivedBody = pctx.Body
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *bodyRecorderPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// bodyMutatorPlugin declares WritesBody and rewrites the request body
// to a fixed payload. The pipeline validator requires WritesBody run
// after any ReadsBody plugin, which this satisfies by itself (no reader
// present when used alone).
type bodyMutatorPlugin struct {
	newBody []byte
}

func (p *bodyMutatorPlugin) Name() string { return "body-mutator" }
func (p *bodyMutatorPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesBody: true}
}
func (p *bodyMutatorPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.SetBody(p.newBody)
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *bodyMutatorPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestReverseProxy_RequestBodyMutation: a WritesBody plugin that
// rewrites pctx.Body via SetBody must cause the upstream backend to
// receive the new bytes with a correct Content-Length header. Confirms
// that the reverseproxy request-path propagation is wired to the
// BodyMutated flag.
func TestReverseProxy_RequestBodyMutation(t *testing.T) {
	newBody := `{"sanitized":"payload"}`
	var (
		gotBody   []byte
		gotLength string
		gotEnc    string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotLength = r.Header.Get("Content-Length")
		gotEnc = r.Header.Get("Content-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	mutator := &bodyMutatorPlugin{newBody: []byte(newBody)}
	p, err := pipeline.New([]pipeline.Plugin{mutator})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	orig := `{"original":"prompt"}`
	req, _ := http.NewRequest("POST", proxy.URL+"/agent", strings.NewReader(orig))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip") // plugin may have decompressed; listener clears
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if string(gotBody) != newBody {
		t.Errorf("backend got body = %q, want %q (listener did not propagate mutation)", gotBody, newBody)
	}
	if gotLength != "23" { // len(`{"sanitized":"payload"}`)
		t.Errorf("Content-Length = %q, want 23 (mutation rewrite)", gotLength)
	}
	if gotEnc != "" {
		t.Errorf("Content-Encoding = %q, want empty (listener should clear on mutation)", gotEnc)
	}
}

func TestReverseProxy_BodyBuffering(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"method":"tools/call","id":1,"params":{"name":"get_weather"}}`
	req, _ := http.NewRequest("POST", proxy.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if string(recorder.receivedBody) != body {
		t.Errorf("plugin got body = %q, want %q", recorder.receivedBody, body)
	}
}

func TestReverseProxy_BodyTooLarge(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend should not be reached for oversized body")
	}))
	defer backend.Close()

	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	bigBody := strings.Repeat("x", maxBodySize+1)
	req, _ := http.NewRequest("POST", proxy.URL+"/mcp", strings.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestReverseProxy_BodyNotBuffered_WhenNotNeeded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "test"},
	})
	srv, err := NewServer(inboundPipelineFromAuth(t, a), nil, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"data":"should not be buffered"}`
	req, _ := http.NewRequest("POST", proxy.URL+"/api", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
