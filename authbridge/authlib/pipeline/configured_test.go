package pipeline

import (
	"context"
	"encoding/json"
	"testing"
)

// fakePlugin is a minimal Plugin implementation for testing the wrapper.
// It records call counts so pass-through can be asserted.
type fakePlugin struct {
	name      string
	caps      PluginCapabilities
	requests  int
	responses int
}

func (f *fakePlugin) Name() string                  { return f.name }
func (f *fakePlugin) Capabilities() PluginCapabilities { return f.caps }
func (f *fakePlugin) OnRequest(ctx context.Context, pctx *Context) Action {
	f.requests++
	return Action{}
}
func (f *fakePlugin) OnResponse(ctx context.Context, pctx *Context) Action {
	f.responses++
	return Action{}
}

func TestConfiguredPluginRawConfig(t *testing.T) {
	raw := json.RawMessage(`{"issuer":"http://idp"}`)
	cp := wrapConfigured(&fakePlugin{name: "jwt-validation"}, raw)
	rc, ok := cp.(interface{ RawConfig() json.RawMessage })
	if !ok {
		t.Fatal("wrapper should expose RawConfig() via type-assertion")
	}
	got := string(rc.RawConfig())
	if got != `{"issuer":"http://idp"}` {
		t.Fatalf("RawConfig: %q want %q", got, `{"issuer":"http://idp"}`)
	}
}

func TestConfiguredPluginPassesThroughPluginMethods(t *testing.T) {
	fake := &fakePlugin{
		name: "jwt-validation",
		caps: PluginCapabilities{Reads: []string{"a"}, Writes: []string{"security"}},
	}
	cp := wrapConfigured(fake, json.RawMessage(`{}`))

	if cp.Name() != "jwt-validation" {
		t.Fatalf("Name pass-through broken: %q", cp.Name())
	}
	caps := cp.Capabilities()
	if len(caps.Reads) != 1 || caps.Reads[0] != "a" {
		t.Fatalf("Capabilities pass-through broken: %+v", caps)
	}
	if len(caps.Writes) != 1 || caps.Writes[0] != "security" {
		t.Fatalf("Capabilities pass-through broken: %+v", caps)
	}
	cp.OnRequest(context.Background(), nil)
	cp.OnResponse(context.Background(), nil)
	if fake.requests != 1 || fake.responses != 1 {
		t.Fatalf("OnRequest/OnResponse pass-through broken: req=%d resp=%d",
			fake.requests, fake.responses)
	}
}
