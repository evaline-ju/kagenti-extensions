package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cmdDir and repoRoot are resolved relative to the process working directory,
// matching the `go -C <this-module>` convention every call site uses.
const (
	cmdDir   = "../../cmd"
	repoRoot = "../../.."
)

// legacyTagEscape exempts a file from the retired-tag-form guard. Prose that
// legitimately discusses the old convention — a release note, an ADR, a design
// doc explaining what changed — should not have to choose between being accurate
// and passing CI, because the easy way out of that is to weaken the guard.
const legacyTagEscape = "allow-legacy-plugin-tag"

// skipDir reports directories no guard should descend into. .worktrees matters
// most: sibling worktrees hold other branches, and scanning them would fail this
// module's tests based on code that is not in this tree.
func skipDir(name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", ".worktrees", ".venv":
		return true
	}
	return false
}

// buildFile reports whether a file can plausibly name a profile: the workflow,
// shell and make surfaces that invoke the generator.
func buildFile(name string) bool {
	switch filepath.Ext(name) {
	case ".yaml", ".yml", ".sh":
		return true
	}
	return name == "Makefile" || strings.HasPrefix(name, "Dockerfile")
}

// blankPluginImport matches a blank import of a plugin package.
var blankPluginImport = regexp.MustCompile(`_\s+"github\.com/rossoctl/cortex/authbridge/authlib/plugins/(\w+)"`)

// TestNoExcludePluginTagsRemain guards the convention itself. Under all-opt-in
// nothing links by default, so `exclude_plugin_*` has no meaning — but Go does
// NOT error on an unsatisfied build tag, so a leftover `-tags exclude_plugin_x`
// is a silent no-op rather than a build failure. Anything still naming the old
// form is either dead or actively lying about what it excludes.
func TestNoExcludePluginTagsRemain(t *testing.T) {
	// Assembled at runtime so this test file does not match itself.
	needle := "exclude" + "_plugin_"
	var hits []string
	// Walk from the repository root, not just authbridge/: the convention is also
	// described in the top-level CLAUDE.md, LOCAL_TESTING_GUIDE.md and
	// local-build-and-test.sh, and a stale description there misleads just as much.
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".yaml", ".yml", ".sh", ".md":
		default:
			return nil
		}
		if strings.HasSuffix(path, "guards_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(data)
		if strings.Contains(text, legacyTagEscape) {
			return nil
		}
		if strings.Contains(text, needle) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(hits) > 0 {
		t.Errorf("%d file(s) still reference the removed tag form %q:\n  %s\n\n"+
			"If a file legitimately discusses the old convention (release note, ADR, "+
			"design history), add the marker %q to it rather than relaxing this guard.",
			len(hits), needle, strings.Join(hits, "\n  "), legacyTagEscape)
	}
}

// TestNoUnconditionalPluginImports guards the hole that shipped in
// authbridge-envoy and authbridge-cpex: both blank-imported plugin packages
// straight from main.go, so those plugins could not be excluded by any tag and
// no error said so. Every plugin must enter through a tagged plugins_*.go file.
func TestNoUnconditionalPluginImports(t *testing.T) {
	var hits []string
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		t.Fatalf("read %s: %v", cmdDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		goFiles, err := filepath.Glob(filepath.Join(cmdDir, e.Name(), "*.go"))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		for _, f := range goFiles {
			base := filepath.Base(f)
			if strings.HasPrefix(base, "plugins_") || strings.HasSuffix(base, "_test.go") {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			for _, m := range blankPluginImport.FindAllStringSubmatch(string(data), -1) {
				hits = append(hits, f+" imports "+m[1])
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("%d unconditional plugin import(s) outside a tagged plugins_*.go:\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

// TestEveryPluginIsAccountedFor is the guard against a plugin silently vanishing
// from production. Under opt-out, a new plugin reached every artifact for free;
// under opt-in a forgotten manifest entry means it reaches none, and nothing
// fails. Every plugin discovered in the tree must be named by a profile or
// listed as deliberately optional.
func TestEveryPluginIsAccountedFor(t *testing.T) {
	found, err := discoverPlugins(cmdDir)
	if err != nil {
		t.Fatalf("discoverPlugins: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no plugins discovered — the build-tag convention changed and this guard went blind")
	}
	inProfile := map[string]bool{}
	for _, names := range profiles {
		for _, n := range names {
			inProfile[n] = true
		}
	}
	var orphans []string
	for _, n := range found {
		if !inProfile[n] && !optional[n] {
			orphans = append(orphans, n)
		}
	}
	if len(orphans) > 0 {
		t.Errorf("plugin(s) in no profile and not marked optional: %s", strings.Join(orphans, ", "))
	}
}

// reservedFileSuffixes are the GOOS and GOARCH tokens Go treats as an implicit
// build constraint when they appear as a filename suffix. Only the ones a plugin
// name could plausibly collide with are listed; extend when a new plugin trips it.
var reservedFileSuffixes = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mipsle": true,
	"ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

// TestNoPluginFileShadowedByGOOSGOARCH guards a silent drop that Go itself
// causes. A file named plugins_sparc.go carries an implicit GOARCH=sparc
// constraint, so it compiles on no normal machine — the plugin vanishes with no
// build error, and a tag-scanning guard still "finds" it because the directive is
// right there in the source. cmd/authbridge-proxy already works around this by
// naming its file plugins_sparcplugin.go; nothing enforced it until now.
func TestNoPluginFileShadowedByGOOSGOARCH(t *testing.T) {
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		t.Fatalf("read %s: %v", cmdDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(cmdDir, e.Name(), "plugins_*.go"))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		for _, path := range matches {
			base := strings.TrimSuffix(filepath.Base(path), ".go")
			suffix := base[strings.LastIndex(base, "_")+1:]
			if reservedFileSuffixes[suffix] {
				t.Errorf("%s: filename suffix %q is a GOOS/GOARCH token, so Go excludes "+
					"this file on every other platform and the plugin is silently dropped; "+
					"rename it (e.g. plugins_%splugin.go)", path, suffix, suffix)
			}
		}
	}
}

// profileInvocations match the shapes a call site uses to name a profile. All
// three deliberately require the name to start with a letter, so a shell
// indirection (`"${profile}"`, `"$1"`) does not match — those values live in
// loops this cannot evaluate, and the loop's literal list is caught by the third
// pattern instead.
var profileInvocations = []*regexp.Regexp{
	// Direct: go -C .../profile-tags run . full
	regexp.MustCompile(`profile-tags run \. "?([a-z][a-z0-9_]*)`),
	// Via the shell helper both workflows define: profile_tags full
	regexp.MustCompile(`profile_tags ([a-z][a-z0-9_]*)`),
	// The loop list in ci.yaml: profiles="local full lite"
	regexp.MustCompile(`profiles="([a-z0-9_ ]+)"`),
}

// TestCallSitesUseKnownProfiles guards the failure this refactor actually hit:
// the tag-form guard passed while .github/workflows/ci.yaml still invoked the
// deleted generator, because a broken call site contains no build tag to find. A
// profile name is a string in YAML and shell — a typo or a rename produces a
// non-zero exit deep in a build, or worse an empty tag list.
//
// Only literal names are checked; `$1`/`${profile}` indirections are skipped,
// since their values live in shell loops this cannot evaluate.
func TestCallSitesUseKnownProfiles(t *testing.T) {
	checked := 0
	// Walk the whole repository, not just its top level. A stale call site is
	// exactly what this guard exists for, and one can live in a nested Makefile,
	// scripts/*.sh or Dockerfile as easily as in .github/workflows.
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !buildFile(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, re := range profileInvocations {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				for _, name := range strings.Fields(m[1]) {
					checked++
					if _, ok := profiles[name]; !ok {
						t.Errorf("%s names profile %q, which is not defined (known: %v)",
							path, name, known())
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked == 0 {
		t.Error("no profile-tags call sites found — either the invocation shape changed " +
			"or CI stopped resolving profiles, and this guard went blind")
	}
}

// TestNoStaleProfileEntries is the mirror: a profile naming a plugin that no
// longer exists yields `-tags include_plugin_gone`, which Go accepts silently,
// so the artifact quietly ships without it.
func TestNoStaleProfileEntries(t *testing.T) {
	found, err := discoverPlugins(cmdDir)
	if err != nil {
		t.Fatalf("discoverPlugins: %v", err)
	}
	exists := map[string]bool{}
	for _, n := range found {
		exists[n] = true
	}
	for profile, names := range profiles {
		for _, n := range names {
			if !exists[n] {
				t.Errorf("profile %q names %q, which has no plugins_%s.go anywhere under %s",
					profile, n, n, cmdDir)
			}
		}
	}
	for n := range optional {
		if !exists[n] {
			t.Errorf("optional names %q, which has no plugins_%s.go anywhere under %s", n, n, cmdDir)
		}
	}
}
