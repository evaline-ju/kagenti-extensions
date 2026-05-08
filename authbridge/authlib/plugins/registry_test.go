package plugins

import (
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// TestBuiltinsRegistered verifies every in-tree plugin is discoverable
// through the new registry — the list is the public contract that
// operator YAML depends on, so a regression here breaks deployments.
func TestBuiltinsRegistered(t *testing.T) {
	want := map[string]bool{
		"jwt-validation":   true,
		"token-exchange":   true,
		"a2a-parser":       true,
		"mcp-parser":       true,
		"inference-parser": true,
	}
	got := RegisteredPlugins()
	gotSet := make(map[string]bool, len(got))
	for _, n := range got {
		gotSet[n] = true
	}
	for name := range want {
		if !gotSet[name] {
			t.Errorf("built-in plugin %q missing from registry; got: %v", name, got)
		}
	}
}

// TestRegisterPlugin_DoubleRegistration_Panics locks the strict-fail
// policy. Silent last-write-wins would let a deployment with two
// incompatible copies of the same plugin corrupt the pipeline
// composition; panic on registration catches it at process start.
func TestRegisterPlugin_DoubleRegistration_Panics(t *testing.T) {
	name := "test-double-register"
	RegisterPlugin(name, func() pipeline.Plugin { return nil })
	t.Cleanup(func() { UnregisterPlugin(name) })

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on double-registration")
		}
	}()
	RegisterPlugin(name, func() pipeline.Plugin { return nil })
}

func TestRegisterPlugin_EmptyName_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty name")
		}
	}()
	RegisterPlugin("", func() pipeline.Plugin { return nil })
}

func TestRegisterPlugin_NilFactory_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on nil factory")
		}
	}()
	RegisterPlugin("test-nil-factory", nil)
}

// TestUnregisterPlugin verifies the test-isolation helper. After
// registering + unregistering, the name is absent from RegisteredPlugins
// and Build rejects it as unknown.
func TestUnregisterPlugin(t *testing.T) {
	name := "test-unregister"
	RegisterPlugin(name, func() pipeline.Plugin { return nil })
	if !contains(RegisteredPlugins(), name) {
		t.Fatalf("plugin not registered after RegisterPlugin")
	}
	if !UnregisterPlugin(name) {
		t.Errorf("UnregisterPlugin returned false for a registered name")
	}
	if contains(RegisteredPlugins(), name) {
		t.Errorf("plugin still in registry after UnregisterPlugin")
	}
	// Second unregister should be a no-op (returns false).
	if UnregisterPlugin(name) {
		t.Errorf("UnregisterPlugin returned true for an unregistered name")
	}
}

// TestBuild_UnknownPlugin_ListsRegistered verifies the "unknown plugin"
// error includes the list of registered names so operators get a
// typo-catching diagnostic instead of a generic not-found.
func TestBuild_UnknownPlugin_ListsRegistered(t *testing.T) {
	_, err := Build([]config.PluginEntry{{Name: "not-a-real-plugin"}})
	if err == nil {
		t.Fatalf("expected error for unknown plugin")
	}
	msg := err.Error()
	if !containsSubstring(msg, "not-a-real-plugin") {
		t.Errorf("error should name the unknown plugin: %q", msg)
	}
	if !containsSubstring(msg, "jwt-validation") {
		t.Errorf("error should list registered plugins (for typo diagnostics): %q", msg)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
