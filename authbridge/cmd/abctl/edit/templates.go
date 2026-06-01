package edit

import (
	"fmt"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/apiclient"
)

// FenceMarker delimits the active pipeline subtree (above) from the
// commented templates reference (below) inside the abctl edit tempfile.
// The save path strips everything from this line onward before applying.
//
// The exact bytes matter: detection is a literal line match. Keep them
// in sync with templates_test.go and configmap.go's fence-stripping.
const FenceMarker = "# === ABCTL TEMPLATES BELOW (stripped on save) ==="

// templatesBanner is the prose shown immediately below FenceMarker,
// telling the operator how to use the reference.
const templatesBanner = `# Reference: every plugin in the catalog. Copy a block above the fence,
# strip the leading "# " from each line, then adjust indentation to fit
# your inbound: or outbound: chain (typically 6-space "- name:" indent).
# This entire section is removed before kubectl apply.`

// RenderTemplates returns a fence marker followed by a commented YAML
// template block per plugin in the catalog. Returns nil for an empty
// catalog so callers can append unconditionally.
//
// Every emitted line starts with "#" — the templates section is pure
// comments. If the operator deletes the fence marker by accident, the
// templates still parse as comment-only YAML (no semantic effect),
// which is the safe-fallback the plan committed to.
//
// Output starts directly with the fence marker (no leading blank
// separator). This matters at save time: the active pipeline subtree
// already ends with its own newline, so a stripped buffer should equal
// the original subtree byte-for-byte. A blank-line separator here
// would survive the strip and surface as a spurious `+` line in the
// no-changes diff.
func RenderTemplates(plugins []apiclient.PluginCatalogEntry) []byte {
	if len(plugins) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(FenceMarker)
	b.WriteString("\n")
	b.WriteString(templatesBanner)
	b.WriteString("\n")
	for _, p := range plugins {
		renderPluginTemplate(&b, p)
	}
	return []byte(b.String())
}

func renderPluginTemplate(b *strings.Builder, p apiclient.PluginCatalogEntry) {
	b.WriteString("\n# --- ")
	b.WriteString(p.Name)
	b.WriteString(" ---\n")
	if p.Description != "" {
		// Description is single-line (Go struct tags can't carry newlines)
		// so a one-line "# <description>" comment is sufficient.
		b.WriteString("# ")
		b.WriteString(p.Description)
		b.WriteString("\n")
	}

	// Split fields into required vs optional. Required render first so
	// the operator's eye lands on must-fill slots; the "Required:" header
	// is always emitted (even when none) so the answer is explicit
	// rather than inferred from absence.
	var required, optional []apiclient.PluginFieldEntry
	for _, f := range p.Fields {
		if f.Required {
			required = append(required, f)
		} else {
			optional = append(optional, f)
		}
	}
	if len(p.Fields) > 0 {
		b.WriteString("# Required: ")
		if len(required) == 0 {
			b.WriteString("(none — every field is optional)")
		} else {
			names := make([]string, len(required))
			for i, f := range required {
				names[i] = f.Name
			}
			b.WriteString(strings.Join(names, ", "))
		}
		b.WriteString("\n")
	}

	b.WriteString("#       - name: ")
	b.WriteString(p.Name)
	b.WriteString("\n")
	if len(p.Fields) == 0 {
		b.WriteString("#         # (no configurable fields)\n")
		return
	}
	b.WriteString("#         config:\n")
	for _, f := range required {
		renderField(b, f)
	}
	for _, f := range optional {
		renderField(b, f)
	}
}

// renderField emits two lines per field for readability:
//
//	#           # [REQUIRED] <description>
//	#           <name>: <placeholder>
//
// or, for optional fields:
//
//	#           # [optional, default=X, enum=a|b] <description>
//	#           <name>: <placeholder>
//
// The bracket prefix scans at the left margin so the operator can tell
// required from optional without parsing inline notes.
func renderField(b *strings.Builder, f apiclient.PluginFieldEntry) {
	// Annotation line.
	b.WriteString("#           # ")
	if f.Required {
		b.WriteString("[REQUIRED]")
	} else {
		var attrs []string
		attrs = append(attrs, "optional")
		if f.Default != "" {
			attrs = append(attrs, fmt.Sprintf("default=%s", f.Default))
		}
		if len(f.Enum) > 0 {
			attrs = append(attrs, "enum="+strings.Join(f.Enum, "|"))
		}
		b.WriteString("[")
		b.WriteString(strings.Join(attrs, ", "))
		b.WriteString("]")
	}
	if f.Description != "" {
		b.WriteString(" ")
		b.WriteString(f.Description)
	}
	b.WriteString("\n")

	// Required field with an enum: still surface the choices, since
	// they're constraint information, not a default note.
	if f.Required && len(f.Enum) > 0 {
		b.WriteString("#           #   choices: ")
		b.WriteString(strings.Join(f.Enum, " | "))
		b.WriteString("\n")
	}

	// Value line.
	b.WriteString("#           ")
	b.WriteString(f.Name)
	b.WriteString(": ")
	b.WriteString(placeholderFor(f))
	b.WriteString("\n")
}

// StripTemplates returns edited with the fence marker and everything
// after it removed. If the fence marker is absent, edited is returned
// unchanged — the safe-fallback contract from the plan: a missing
// fence marker means the operator either deleted it deliberately or
// the catalog wasn't available at fetch time, and either way the
// remaining buffer is what gets applied.
//
// Detection is a strict line match: a line whose first non-whitespace
// content is exactly FenceMarker. Whitespace before the marker is
// tolerated (some editors auto-indent comment lines), but the marker
// itself must be intact. Truncation occurs at the start of that line
// so the trailing newline of the previous line — the active pipeline
// subtree's terminator — survives.
func StripTemplates(edited []byte) []byte {
	target := FenceMarker
	// Walk lines manually rather than splitting; preserves the byte
	// position needed for the truncation cut.
	i := 0
	for i < len(edited) {
		// Find the next line's end.
		end := i
		for end < len(edited) && edited[end] != '\n' {
			end++
		}
		// Strip leading whitespace for the comparison.
		start := i
		for start < end && (edited[start] == ' ' || edited[start] == '\t') {
			start++
		}
		if end-start >= len(target) && string(edited[start:start+len(target)]) == target {
			// Truncate at i — the start of this line — discarding the
			// fence and everything after.
			return edited[:i]
		}
		// Advance past the newline.
		i = end + 1
	}
	return edited
}

// placeholderFor picks the YAML placeholder that goes after the field
// name. Documented defaults take priority for primitive types so the
// operator sees the actual fallback inline; otherwise an empty value
// matching the field's type.
func placeholderFor(f apiclient.PluginFieldEntry) string {
	if f.Default != "" && f.Type != "object" {
		// Quote strings so the line is valid YAML when uncommented.
		if f.Type == "string" {
			return fmt.Sprintf("%q", f.Default)
		}
		return f.Default
	}
	switch f.Type {
	case "string":
		return `""`
	case "int":
		return "0"
	case "bool":
		return "false"
	case "[]string":
		return "[]"
	case "object":
		return "{}"
	}
	return `""`
}
