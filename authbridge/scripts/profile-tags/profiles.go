package main

import (
	"fmt"
	"sort"
)

// profiles is the declarative source of truth for which plugins each shipped
// artifact links. One rule governs the whole tree: a plugin is compiled in only
// when a tag names it, so an artifact's contents are exactly its profile entry.
//
// Keys are the build-tag suffix, which may differ from the plugin's registered
// name (e.g. `litellm_budgettrack` vs `litellm-budget-track`).
var profiles = map[string][]string{
	// local is the desktop/laptop artifact. It mirrors the plugin list
	// cmd/authbridge-proxy/local.go writes into ~/.cortex/config.yaml.
	"local": {
		"a2aparser",
		"inferenceparser",
		"mcpparser",
		"toolprune",
	},
	// full is the Kubernetes/sidecar artifact: every plugin that was default-on
	// before the all-opt-in convention. Deliberately NOT "every plugin" — adding
	// the two optional ones would take the proxy from 33.4 to 55.5 MiB.
	"full": {
		"a2aparser",
		"ibac",
		"inferenceparser",
		"jwtvalidation",
		"lineage",
		"litellm_budgettrack",
		"mcpparser",
		"opa",
		"sparc",
		"staticinject",
		"tokenbroker",
		"tokenexchange",
		"toolprune",
	},
	// lite carries the set the retired lite-tags generator kept, so the published
	// -lite artifact is unchanged by the convention refactor. Note it shares no
	// plugin with `local`: lite is a sidecar minimum (identity and credential
	// injection) and cannot show a single token count, so it is not and never was
	// a candidate for the desktop download.
	"lite": {
		"jwtvalidation",
		"litellm_budgettrack",
		"staticinject",
		"tokenexchange",
	},
	// envoy is what cmd/authbridge-envoy linked before the convention change:
	// eight plugins imported unconditionally from main.go plus three tagged files.
	// No litellm_budgettrack or staticinject — that is the pre-existing shape,
	// preserved deliberately rather than harmonised in the same change.
	"envoy": {
		"a2aparser",
		"ibac",
		"inferenceparser",
		"jwtvalidation",
		"lineage",
		"mcpparser",
		"opa",
		"sparc",
		"tokenbroker",
		"tokenexchange",
		"toolprune",
	},
	// cpex is what cmd/authbridge-cpex linked before the convention change, all
	// nine imported unconditionally. It is the only artifact carrying the cpex
	// plugin, which is registered nowhere else in the tree.
	"cpex": {
		"a2aparser",
		"cpex",
		"ibac",
		"inferenceparser",
		"jwtvalidation",
		"mcpparser",
		"sparc",
		"tokenbroker",
		"tokenexchange",
	},
}

// Tags returns the include_plugin_* build tags for a named profile, sorted so
// the output is stable across runs and diffable in CI logs.
func Tags(profile string) ([]string, error) {
	names, ok := profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q (known: %v)", profile, known())
	}
	// Fail closed on an empty profile: an empty CSV becomes `go build -tags ""`,
	// which under an all-opt-in convention links no plugins at all and would ship
	// a proxy that refuses every config it is handed.
	if len(names) == 0 {
		return nil, fmt.Errorf("profile %q names no plugins", profile)
	}
	tags := make([]string, 0, len(names))
	for _, n := range names {
		tags = append(tags, "include_plugin_"+n)
	}
	sort.Strings(tags)
	return tags, nil
}

// known lists the defined profile names, for error messages.
func known() []string {
	out := make([]string, 0, len(profiles))
	for p := range profiles {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
