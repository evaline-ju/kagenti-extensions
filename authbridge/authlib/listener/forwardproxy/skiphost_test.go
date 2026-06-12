package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/skiphost"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

// markerPlugin records one Invocation per OnRequest call. Tests use it to
// assert whether the pipeline ran on a given request — if SkipHosts
// short-circuits correctly, calls counts AND session events stay at zero.
type markerPlugin struct {
	calls atomic.Int32
}

func (p *markerPlugin) Name() string                                  { return "marker" }
func (p *markerPlugin) Capabilities() pipeline.PluginCapabilities     { return pipeline.PluginCapabilities{} }
func (p *markerPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *markerPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.calls.Add(1)
	pctx.Record(pipeline.Invocation{
		Plugin: "marker",
		Action: pipeline.ActionObserve,
		Phase:  pipeline.InvocationPhaseRequest,
		Reason: "ran",
	})
	return pipeline.Action{Type: pipeline.Continue}
}

func newMarkerServer(t *testing.T, store *session.Store, skip *skiphost.Matcher) (*httptest.Server, *markerPlugin) {
	t.Helper()
	mp := &markerPlugin{}
	pp, err := plugintesting.BuildPipeline([]pipeline.Plugin{mp})
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(pp),
		Sessions:         store,
		Client:           http.DefaultClient,
		SkipHosts:        skip,
	}
	return httptest.NewServer(srv.Handler()), mp
}

// TestForwardProxy_SkipHosts_BypassesPipeline asserts the core property:
// a host matching SkipHosts produces NO plugin invocations and NO session
// events, while a non-matching host on the same proxy still runs both.
// This is the only behavioral guarantee that prevents OTel-style chatty
// infrastructure traffic from evicting the inbound A2A user intent out
// of the session buffer's FIFO eviction window.
func TestForwardProxy_SkipHosts_BypassesPipeline(t *testing.T) {
	upstreamHits := atomic.Int32{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// Match the loopback IP that httptest backends listen on. A pattern
	// like "127.0.0.1" works because skiphost strips the port before
	// matching. We deliberately do NOT use a glob so the test is brittle
	// to behavior, not to glob semantics (those are exercised in the
	// skiphost package's own tests).
	skip, err := skiphost.New([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	proxy, mp := newMarkerServer(t, store, skip)
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}

	// Skip path: backend URL is on 127.0.0.1, so it should match the
	// skip pattern. Pipeline must not run; session must remain empty.
	resp, err := proxyClient.Get(backend.URL + "/skip-me")
	if err != nil {
		t.Fatalf("skip request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("skip path: status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "upstream-ok" {
		t.Errorf("skip path: body = %q, want upstream-ok (transparent forward must still deliver upstream bytes)", string(body))
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("skip path: upstream hit count = %d, want 1 (skip must not block the request)", upstreamHits.Load())
	}
	if mp.calls.Load() != 0 {
		t.Errorf("skip path: pipeline plugin ran %d times, want 0 (SkipHosts must short-circuit before pipeline.Run)", mp.calls.Load())
	}
	if sessions := store.ListSessions(); len(sessions) != 0 {
		t.Errorf("skip path: %d session(s) recorded, want 0 (SkipHosts must skip recording, otherwise OTel-style traffic still evicts the A2A intent)", len(sessions))
	}
}

// TestForwardProxy_SkipHosts_NonMatchingRunsPipeline is the regression
// guard: a Server with a SkipHosts list set must still run the pipeline
// + record events for hosts that DO NOT match the list. Without this
// pairing the skip test above could pass trivially with a globally
// disabled pipeline.
func TestForwardProxy_SkipHosts_NonMatchingRunsPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	skip, err := skiphost.New([]string{"otel-collector*"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	proxy, mp := newMarkerServer(t, store, skip)
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Get(backend.URL + "/run-me")
	if err != nil {
		t.Fatalf("non-skip request failed: %v", err)
	}
	resp.Body.Close()

	if mp.calls.Load() != 1 {
		t.Errorf("non-skip path: pipeline plugin ran %d times, want 1 (host did not match skip list)", mp.calls.Load())
	}
	// The marker plugin recorded an Invocation, so a session event
	// should land. Bucket is DefaultSessionID since no inbound primed
	// an active session for this proxy instance.
	if sessions := store.ListSessions(); len(sessions) != 1 {
		t.Errorf("non-skip path: session count = %d, want 1 (Invocation should drive event recording)", len(sessions))
	}
}

// TestForwardProxy_SkipHosts_NilMatcherPreservesBehavior asserts the
// zero-value default: a Server constructed without SkipHosts (nil
// Matcher) behaves like today's code — pipeline runs, sessions record.
// This is the upgrade-safety contract for existing deployments that
// don't opt into skip_hosts.
func TestForwardProxy_SkipHosts_NilMatcherPreservesBehavior(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	proxy, mp := newMarkerServer(t, store, nil) // SkipHosts: nil
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Get(backend.URL + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if mp.calls.Load() != 1 {
		t.Errorf("nil SkipHosts: pipeline ran %d times, want 1", mp.calls.Load())
	}
}

// silence unused-import warning if mustParseURL ends up only used here
var _ = strings.TrimSpace
