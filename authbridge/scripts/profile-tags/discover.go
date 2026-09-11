package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// optional names plugins that deliberately belong to no shipped profile.
// Membership here is a decision, not an omission: the accounting guard treats an
// unlisted, unprofiled plugin as a bug. Both entries are heavy — contextguru
// adds ~16 MiB and sessionbudget ~6 MiB (go-redis) to a proxy binary — so they
// are linked only when a caller asks for them explicitly.
var optional = map[string]bool{
	"contextguru":   true,
	"sessionbudget": true,
}

// discoverTagPattern matches `include_plugin_<name>` in a //go:build directive.
// Unanchored at the end so a compound directive still yields the name.
var discoverTagPattern = regexp.MustCompile(`^//go:build\s+include_plugin_(\w+)`)

// discoverPlugins returns every plugin name registered anywhere under cmdDir,
// derived from the plugins_*.go build directives themselves. Deriving from
// source rather than a hand-kept list is what makes the accounting guard
// meaningful: a plugin cannot be added without this seeing it.
func discoverPlugins(cmdDir string) ([]string, error) {
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", cmdDir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(cmdDir, e.Name(), "plugins_*.go"))
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", e.Name(), err)
		}
		for _, path := range matches {
			// `plugins_*_test.go` matches the same glob but is not a directive.
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			name, err := extractIncludeSuffix(path)
			if err != nil {
				return nil, err
			}
			if name != "" {
				seen[name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// extractIncludeSuffix returns the suffix of an `include_plugin_*` directive, or
// "" if the file has none.
func extractIncludeSuffix(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if m := discoverTagPattern.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return m[1], nil
		}
	}
	return "", nil
}
