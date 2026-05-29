package pipeline

import (
	"encoding/json"
)

// configuredPlugin wraps a plugin built via Configurable.Configure with the
// raw config bytes it was constructed from, so the session API can surface
// them on /v1/pipeline. All Plugin interface methods forward to the embedded
// plugin — zero observable behavior change at the request hot path.
//
// Optional plugin interfaces (Initializer, Shutdowner, Finisher, Readier)
// are NOT promoted through the embedded Plugin interface — Go does not
// promote method-set membership through an embedded interface. The wrapper
// implements each of those four interfaces explicitly and forwards
// conditionally; see the methods below (added in a follow-up commit).
type configuredPlugin struct {
	Plugin
	raw json.RawMessage
}

// wrapConfigured returns a Plugin whose dynamic type is *configuredPlugin.
// Callers (registry.Build) invoke this only after Configurable.Configure
// returns nil; non-Configurable plugins pass through unwrapped.
func wrapConfigured(p Plugin, raw json.RawMessage) Plugin {
	return &configuredPlugin{Plugin: p, raw: raw}
}

// RawConfig returns the raw config bytes the wrapped plugin was configured
// with. Used by sessionapi describePipeline to populate /v1/pipeline's
// Config field.
func (c *configuredPlugin) RawConfig() json.RawMessage { return c.raw }
