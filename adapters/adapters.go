// Package adapters provides ready-made lap.Tasks for common tools. Each
// constructor returns a plain lap.Task value: change any field (name, groups,
// globs, variants, estimates) before handing it to lap.
//
// Adapters never install anything: tools come from mise (or the project's
// own node_modules / virtualenv), installed by an explicit setup step.
package adapters

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vivster7/lap"
)

// bin returns a project-local executable when present (node_modules/.bin,
// .venv/bin), otherwise the bare name for PATH lookup.
func bin(root, name string) string {
	for _, dir := range []string{"node_modules/.bin", ".venv/bin", "venv/bin"} {
		p := filepath.Join(root, dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return name
}

// files returns inv.Files, or the fallback (usually ".") for project-wide
// invocations.
func files(inv lap.Invocation, fallback ...string) []string {
	if inv.All || len(inv.Files) == 0 {
		return fallback
	}
	return inv.Files
}

// dirsOf returns the sorted unique parent directories of paths, as "./dir".
func dirsOf(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		d := "./" + filepath.ToSlash(filepath.Dir(p))
		if d == "./." {
			d = "."
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// changed returns the changed files matching globs (empty in full scope).
func changed(inv lap.Invocation, globs ...string) []string {
	if inv.Scope == nil || inv.Scope.All {
		return nil
	}
	return inv.Scope.Match(globs)
}

func hasPrefixAny(s string, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
