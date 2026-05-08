# Plugin Author Reference

**Audience:** plugin authors who already know the basics and need the
contract — field names, invariants, error behaviour, the rules that the
framework enforces at startup.

**See also:**
- [`GUIDE.md`](./GUIDE.md) — step-by-step tutorial for writing a new plugin.
- [`../pipeline/README.md`](../pipeline/README.md) — framework architecture:
  how the pipeline composes plugins, the lifecycle, the Context / Extensions
  wire shape.

How plugins under `authbridge/authlib/plugins/` receive, validate, and
apply their configuration; emit session events; and register themselves
with the pipeline builder. Everything here is convention — the framework
only requires `pipeline.Configurable` if the plugin has any config at
all. The rest of this document exists so that the sixth and tenth
plugin don't each invent their own style.

## Scope

- What the YAML entry for a plugin looks like.
- How a plugin decodes that YAML into a typed config struct.
- How a plugin applies defaults and runs validation.
- What the framework does and doesn't do on your behalf.
- A template you can copy for a new plugin.

## YAML entry shape

Each plugin appears in the pipeline as either a bare name or a full entry:

```yaml
pipeline:
  inbound:
    plugins:
      - a2a-parser                       # bare name — no config
      - name: jwt-validation
        id: jwt-validation               # optional; defaults to name
        config:
          issuer: "http://keycloak..."
          audience_file: "/shared/client-id.txt"
          bypass_paths:
            - "/healthz"
```

- **`name`** — required. Must match a key in the plugin registry.
- **`id`** — optional. Defaults to `name`. Lets two instances of the same
  plugin coexist with different config (not yet exercised, but the shape
  is reserved).
- **`config`** — optional. Arbitrary YAML sub-tree owned by the plugin.
  The framework does not interpret it; it's captured as `json.RawMessage`
  and handed to `Configure`.

## The Configurable interface

```go
type Configurable interface {
    Configure(raw json.RawMessage) error
}
```

The framework calls `Configure` exactly once per plugin instance, during
pipeline construction, before `Start`. Plugins without config don't
implement this interface — the builder type-asserts and skips them.

If a plugin **does not** implement `Configurable` but the YAML entry
has a non-empty `config:` block, the builder fails with a clear
`"plugin %q does not accept configuration"` error. This catches
misconfigurations (typo in plugin name, leftover config after a
refactor) at startup.

## The four-step Configure pattern

Every Configurable plugin follows the same shape:

```go
func (p *Plugin) Configure(raw json.RawMessage) error {
    var c pluginConfig
    if len(raw) > 0 {
        dec := json.NewDecoder(bytes.NewReader(raw))
        dec.DisallowUnknownFields()             // 1. strict decode
        if err := dec.Decode(&c); err != nil {
            return fmt.Errorf("plugin config: %w", err)
        }
    }
    c.applyDefaults()                           // 2. fill in defaults
    if err := c.validate(); err != nil {        // 3. validate
        return fmt.Errorf("plugin config: %w", err)
    }
    // 4. construct internal state
    p.verifier = newVerifier(c.Issuer, c.JWKSURL)
    p.bypass = bypass.New(c.BypassPaths)
    return nil
}
```

### 1. Strict decode (`DisallowUnknownFields`)

Always. A stale or misspelled key is a mistake, not a preference. Loud
failure at startup beats a silent wrong default at request time.

### 2. `applyDefaults()`

Fills zero-value fields with sensible defaults and derives computed
fields. Keep it pure — no I/O, no file reads — so it can be unit-tested
with the config struct alone.

```go
func (c *pluginConfig) applyDefaults() {
    if c.DefaultPolicy == "" {
        c.DefaultPolicy = "passthrough"
    }
    if c.JWKSURL == "" && c.Issuer != "" {
        c.JWKSURL = c.Issuer + "/protocol/openid-connect/certs"
    }
}
```

When you need to distinguish "unset" from "explicitly set to zero" —
typically for booleans — use `*bool` / `*int` in the struct and convert
to plain values after `applyDefaults`. `SessionConfig.Enabled` in
`authlib/config` is the reference pattern.

### 3. `validate()`

Rejects configurations the plugin cannot operate with. Run validation
**after** `applyDefaults` so derived fields are in place.

```go
func (c *pluginConfig) validate() error {
    if c.Issuer == "" {
        return errors.New("issuer is required")
    }
    if c.DefaultPolicy != "passthrough" && c.DefaultPolicy != "exchange" {
        return fmt.Errorf("default_policy must be passthrough or exchange, got %q", c.DefaultPolicy)
    }
    return nil
}
```

Return errors phrased for an operator reading a pod log, not a developer
reading a stack trace.

### 4. Construct internal state

This is the only step allowed to do I/O (read credential files, open
connections, etc.). Everything the plugin needs at request time should
be materialized here, not lazily on first `OnRequest` — lazy init
hides config errors until traffic arrives.

## File-sourced values

Several plugins accept either an inline value or a file path for the
same datum (e.g. `client_secret` vs `client_secret_file`). The
convention:

- Both fields live in the config struct; the file variant has the
  `_file` suffix.
- `applyDefaults` does not read the file.
- `validate` requires exactly one to be set.
- Internal state construction calls the file-read helper from
  `authlib/config` (not a new one), which tolerates transient absence
  during pod boot (client-registration may still be writing).

## What Configure MUST NOT do

- **Block forever.** Configure runs before traffic starts; the process
  is still holding the startup deadline. Use bounded waits with
  timeouts, not unbounded blocking reads.
- **Start background goroutines.** Use `Init(ctx)` from the
  `pipeline.Initializer` interface for that — it runs after Configure
  and has a process context you can key your goroutine's lifetime to.
- **Mutate global state.** Plugins run in a single process today, but
  the config → runtime mapping must stay per-instance. Two instances
  of the same plugin with different config must not clobber each other.
- **Persist the raw bytes.** Decode into your typed struct and drop
  the `json.RawMessage`. Holding it leaks the original YAML, which
  may contain secrets, into any log that dumps the plugin for
  debugging.

## Testing

Each Configurable plugin ships three kinds of tests:

1. **Config round-trip.** Given a YAML snippet, does Configure produce
   the expected internal state? Exercise defaults-applied and defaults-
   rejected paths explicitly.
2. **Validation failures.** One test per validation error path — name
   a missing-required field, a malformed value, a conflicting pair.
   Assert the error message names the bad field.
3. **Behavior integration.** The existing `OnRequest` / `OnResponse`
   tests, but wired through Configure rather than hand-built internal
   state. This is what keeps the config layer and the plugin behavior
   honest about each other.

## Template

Copy this into a new plugin file as the starting point. Replace
`myPlugin` with your plugin's identifier.

```go
package plugins

import (
    "bytes"
    "encoding/json"
    "errors"
    "fmt"

    "github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// myPluginConfig is the plugin's private config schema. Fields are JSON-
// tagged so Configure can DisallowUnknownFields against operator-supplied
// YAML (YAML → JSON round-trip preserves key names).
type myPluginConfig struct {
    SomeKnob   string   `json:"some_knob"`
    SomePaths  []string `json:"some_paths"`
    // ...
}

func (c *myPluginConfig) applyDefaults() {
    if c.SomeKnob == "" {
        c.SomeKnob = "default-value"
    }
}

func (c *myPluginConfig) validate() error {
    if c.SomeKnob == "" {
        return errors.New("some_knob is required")
    }
    return nil
}

type MyPlugin struct {
    // internal state populated by Configure
}

func (p *MyPlugin) Configure(raw json.RawMessage) error {
    var c myPluginConfig
    if len(raw) > 0 {
        dec := json.NewDecoder(bytes.NewReader(raw))
        dec.DisallowUnknownFields()
        if err := dec.Decode(&c); err != nil {
            return fmt.Errorf("my-plugin config: %w", err)
        }
    }
    c.applyDefaults()
    if err := c.validate(); err != nil {
        return fmt.Errorf("my-plugin config: %w", err)
    }
    // construct internal state from c
    return nil
}

func (p *MyPlugin) Name() string                             { return "my-plugin" }
func (p *MyPlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (p *MyPlugin) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}
func (p *MyPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}
```

## Strictness asymmetry: plugin config vs. runtime top-level

The plugin-level config inside each `plugins[].config` subtree is
**strict** — `DisallowUnknownFields` is part of the Configure
convention, so a typo or a stale key fails the plugin at boot.

The runtime YAML's **top-level** keys (`mode`, `listener`, `pipeline`,
`session`, `stats`) are **forgiving**: unknown top-level keys are
silently ignored by the YAML decoder. This is deliberate forward-
compat — adding a new top-level section (say, `observability:`) in a
future release must not break older binaries reading a newer config.

The obvious gap — an operator keeping the pre-migration top-level
schema (`inbound:`, `outbound:`, `identity:`, `bypass:`, `routes:`)
would have their config silently accepted with those keys dropped —
is closed by `config.Validate`, which errors when either pipeline
list is empty. The error message names the likely cause so the
operator is pointed at the migration, not left wondering why
authentication isn't happening.

## Emitting session events

Every plugin MUST emit at least one `Invocation` record per active
`OnRequest` / `OnResponse` call. Plugins may also populate one of the
typed protocol extensions (`MCP`, `A2A`, `Inference`) when they carry
structured semantic payload, and may additionally publish plugin-
specific events through the `Custom` escape-hatch map.

> For a tutorial on emitting Invocations — the `pctx.Record` / `Allow`
> / `Skip` / `Observe` / `Modify` / `DenyAndRecord` helpers with
> runnable examples — see [`GUIDE.md` Step 2](./GUIDE.md#step-2--record-what-your-plugin-did).
> This section is the field-level reference for the `Invocation`
> struct, the 5-value action vocabulary, and the rules around the
> Custom escape-hatch map.

### 1. Invocation record — field reference

An `Invocation` says *which* plugin ran and *what* it did, in a
5-value vocabulary shared across all plugins. abctl renders one row
per invocation. Every plugin that runs on a pipeline pass produces
at least one.

```go
type Invocation struct {
    Plugin           string           // plugin.Name(); framework-filled
    Action           InvocationAction // 5-value: allow | deny | skip | modify | observe
    Phase            InvocationPhase  // "request" | "response"; framework-filled
    Reason           string           // machine-stable code
    Path             string           // request path; framework-filled

    // Optional diagnostic fields; populated selectively:
    ExpectedIssuer, ExpectedAudience string
    TokenSubject                     string
    TokenAudience, TokenScopes       []string
    RouteMatched                     bool
    RouteHost, TargetAudience        string
    RequestedScopes                  []string
    CacheHit                         bool
}
```

The framework fills `Plugin`, `Phase`, and `Path` when the plugin
emits via `pctx.Record` / `Allow` / `Skip` / `Observe` / `Modify` /
`DenyAndRecord`. A plugin may override those fields explicitly — but
only in test harnesses where the plugin runs outside a
`Pipeline.Run` dispatch loop.

**The 5-value action vocabulary** (complete):

| Action | Meaning | Example |
|---|---|---|
| `allow` | Gate plugin permitted the request | jwt-validation on valid token |
| `deny` | Gate plugin rejected the request; pipeline stops | jwt-validation on bad token, token-exchange on IdP failure |
| `skip` | Plugin ran but didn't act on this message | jwt-validation bypass path; parser whose body didn't match |
| `modify` | Plugin mutated the message | token-exchange replaced the Authorization header |
| `observe` | Plugin attached diagnostic data; flow unchanged | parsers extracting MCP / A2A / Inference state |

`Reason` is a stable machine-readable label (e.g. `path_bypass`,
`no_matching_route`, `jwt_failed`, `matched_tools/call`) that
discriminates within an Action value. abctl filters can match
either — `/skip` shows every skip action regardless of reason;
`/path_bypass` narrows to that specific skip flavour.

**Which diagnostic fields to populate:**

- Auth gates (jwt-validation and kin): `ExpectedIssuer`,
  `ExpectedAudience`, `TokenSubject`, `TokenAudience`, `TokenScopes`.
- Outbound routers (token-exchange and kin): `RouteMatched`,
  `RouteHost`, `TargetAudience`, `RequestedScopes`, `CacheHit`.
- Parsers: usually none — their semantic payload lives on the typed
  extension slot (A2A / MCP / Inference). Emit with just Action +
  Reason.

**NEVER put raw tokens, signatures, or secrets in an Invocation.**
The session store has no auth.

### 2. Named protocol extension (optional, for parsers)

`MCP`, `A2A`, `Inference` are typed slots on `pipeline.Extensions`.
A parser that successfully extracts structured state populates the
matching slot AND emits an `Invocation` with `ActionObserve`. The
slot carries the parsed payload; the Invocation carries the
attribution.

Adding a new named extension is a core-library change: edit
`pipeline/extensions.go`, `pipeline/session.go` (wire + JSON round-
trip), the listener (snapshot + recorder), and abctl if you want
bespoke rendering. Most new plugins don't need one — they emit an
Invocation and publish extra context through the Custom map
(below).

### 3. Escape-hatch map (`Custom` with `/event` suffix)

For plugin-specific observability that doesn't warrant a category yet,
write to `pctx.Extensions.Custom` with a key ending in
`pipeline.PluginEventSuffix` (`"/event"`):

```go
// Plugin-PUBLIC event. Listener serializes this to SessionEvent.Plugins
// under key "rate-limiter" (suffix stripped).
pctx.Extensions.Custom["rate-limiter"+pipeline.PluginEventSuffix] = rateLimiterEvent{
    Allowed:    true,
    TokensLeft: 42,
}

// Plugin-PRIVATE cross-phase state. Never serialized. Used via the
// typed SetState / GetState generics.
pipeline.SetState(pctx, "rate-limiter", &rateLimiterState{Bucket: b})
```

The `/event` suffix is the opt-in marker: the listener only promotes
matching keys into `SessionEvent.Plugins`. Private state stays out.

Rules for plugin-public events:

- **Value must be JSON-marshalable.** The listener calls `json.Marshal`;
  failures downgrade to `slog.Debug` and skip the entry (a misbehaving
  plugin can't break the session stream).
- **NEVER put raw credentials or tokens in the value.** The session
  store has no auth on it — only safe-to-log data belongs there.
- **Key prefix MUST be the plugin's `Name()`.** Keeps namespaces clean
  so unrelated plugins don't collide.
- **Payload schema is plugin-owned.** No central registry; abctl
  treats unknown keys as raw JSON in the detail pane.

### Graduation: when to promote map → named category

Graduate to a typed slot when ≥2 of these are true:

1. **Two or more plugins share the shape.** That's the signal the
   "category" concept is worth codifying — it prevents N plugins from
   each shipping their own near-identical struct.
2. **abctl or the session API grows conditional logic on the key.**
   If consumers already parse the payload, making the schema compile-
   checked is a net win.
3. **The data is populated on nearly every deployment.** Core
   semantics (auth, protocol) graduate; niche plugins stay in the map.

Don't graduate speculatively — the map path has no cost if you stay
in it.

## Registering a plugin

A plugin advertises itself to the pipeline builder through `RegisterPlugin`
in its package `init()`. The registration is open — any package that
imports `authlib/plugins` can register a plugin, regardless of whether it
lives in this module. The pattern mirrors `database/sql` drivers and
`log/slog` handlers.

> For a step-by-step walkthrough (in-tree file layout, out-of-tree
> module + side-effect import, operator YAML wiring), see
> [`GUIDE.md` Step 6](./GUIDE.md#step-6--out-of-tree-plugins). This
> section is the field-level reference: the factory shape and the
> panic-on-misuse guarantees that define the registry's contract.

### Factory shape

```go
// authbridge/authlib/plugins/jwtvalidation.go
func init() {
    RegisterPlugin("jwt-validation", func() pipeline.Plugin { return NewJWTValidation() })
}
```

The factory is called once per pipeline instance during `Build`. It must
return a fresh `pipeline.Plugin`; the registry does not cache the returned
value. Two pipeline entries with the same name produce two independent
plugin instances, each decoded from its own `config:` block.

### Rules and guardrails

- **Double-registration panics.** If two packages both register under the
  same name, the second call panics at process start. This is the
  correct behaviour: silent last-write-wins would let a version
  conflict poison the pipeline composition in ways that only surface as
  mysterious runtime behaviour.
- **Empty name panics.** An empty plugin name cannot be referenced from
  YAML; registering under one is a programmer bug, not a recoverable
  condition.
- **Nil factory panics.** A nil factory would defer the crash until
  `Build` tried to call it; panic at registration is closer to the bug.
- **Unknown plugin fails Build.** `Build` rejects entries whose name
  isn't in the registry; the error message includes every registered
  name so typos are easy to spot.

### Testing against the registry

Tests that need a fake plugin use `RegisterPlugin` + `t.Cleanup` with
`UnregisterPlugin`:

```go
func TestMyScenario(t *testing.T) {
    plugins.RegisterPlugin("fake-auth", func() pipeline.Plugin {
        return &fakeAuth{}
    })
    t.Cleanup(func() { plugins.UnregisterPlugin("fake-auth") })

    p, err := plugins.Build([]config.PluginEntry{{Name: "fake-auth"}})
    // ... assert on p ...
}
```

`UnregisterPlugin` is test-only by convention — production code should
never call it. It exists to keep tests isolated from each other under
`-parallel`.

## Cross-references

- `authbridge/authlib/pipeline/configurable.go` — the interface.
- `authbridge/authlib/pipeline/README.md` — how plugins compose and
  run; Configure's place in the lifecycle.
- `authbridge/authlib/config/config.go` — `PluginEntry` YAML shape and
  parsing.
- `authbridge/authlib/plugins/registry.go` — how Build calls Configure.
- `authbridge/authlib/pipeline/extensions.go` — named categories
  (`MCP`, `A2A`, `Inference`, `Auth`) + `Custom` map + escape-hatch
  convention.
- `authbridge/authlib/pipeline/session.go` — `SessionEvent` wire shape
  and the `SessionDenied` phase.
