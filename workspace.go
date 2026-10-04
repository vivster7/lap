package lap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// dirtySnapshot hashes every modified or untracked (not ignored) file in the
// worktree. Writers are attributed the files whose hash changed while they
// ran; checks become stale when files they read change. Clean files are not
// hashed, so the cost scales with the size of the change, not the repo.
func dirtySnapshot(root string) map[string]string {
	cmd := exec.Command("git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	cmd.Dir = root
	out, err := cmd.Output()
	snap := map[string]string{}
	if err != nil {
		return snap
	}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) < 4 {
			continue
		}
		path := string(rec[3:])
		snap[path] = hashFile(filepath.Join(root, path))
	}
	return snap
}

func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "" // deleted
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.IsDir() {
		return "dir"
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unreadable"
	}
	return hex.EncodeToString(h.Sum(nil))
}

// changedSince returns paths whose state differs between two snapshots and
// that are relevant to the task (its selected files, or its globs, or any
// path for tasks without globs). A path missing from a snapshot is clean.
func changedSince(before, after map[string]string, t *Task, inv Invocation) []string {
	var out []string
	seen := map[string]bool{}
	check := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		b, inB := before[p]
		a, inA := after[p]
		if inB == inA && b == a {
			return
		}
		if relevant(p, t, inv) {
			out = append(out, p)
		}
	}
	for p := range before {
		check(p)
	}
	for p := range after {
		check(p)
	}
	sort.Strings(out)
	return out
}

func relevant(path string, t *Task, inv Invocation) bool {
	if !inv.All && len(inv.Files) > 0 {
		for _, f := range inv.Files {
			if f == path {
				return true
			}
		}
		return false
	}
	if len(t.Files) == 0 {
		return true
	}
	return matchAny(t.Files, path)
}

func matchAny(globs []string, path string) bool {
	for _, g := range globs {
		if !strings.Contains(g, "/") {
			g = "**/" + g
		}
		if ok, _ := doublestar.Match(g, path); ok {
			return true
		}
	}
	return false
}

func summarizePaths(ps []string) string {
	if len(ps) <= 3 {
		return strings.Join(ps, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ps[:3], ", "), len(ps)-3)
}

func numCPU() int { return runtime.NumCPU() }
