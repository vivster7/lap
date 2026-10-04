// Command dev is an example inner-loop command for the Django repository
// (github.com/django/django), built with lap.
//
// It mirrors Django's pre-commit hooks (black, isort, flake8, biome) and
// adds tests. Django's full test suite takes many minutes, so within a 60s
// budget dev runs the test modules related to what you changed: tests/<app>
// directories you edited, and test apps named after the django/ packages you
// edited (django/contrib/auth -> tests/auth_tests). That mapping is a
// heuristic, not impact analysis; the report says the full suite was not
// verified.
//
// Usage, from a Django checkout with a .venv (see examples/django/README.md):
//
//	go run github.com/vivster7/lap/examples/django@latest [group|task ...]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/adapters"
	"github.com/vivster7/lap/cli"
	"github.com/vivster7/lap/store"
)

func main() {
	black := adapters.Black()
	isort := adapters.Isort()
	flake8 := adapters.Flake8()

	biome := adapters.Biome()
	biome.Files = []string{"*.js", "*.mjs", "*.css", "*.json"}
	biome.Variants[0].Cmd = func(inv lap.Invocation) ([]string, error) {
		argv := []string{"biome", "check", "--files-ignore-unknown=true", "--no-errors-on-unmatched"}
		if inv.Mode == lap.Fix {
			argv = append(argv, "--write")
		}
		if inv.All {
			return append(argv, "."), nil
		}
		return append(argv, inv.Files...), nil
	}

	tests := lap.Task{
		Name:   "tests",
		Groups: []string{"test"},
		Phase:  lap.Check,
		Files:  []string{"django/**/*.py", "tests/**/*.py"},
		Variants: []lap.Variant{
			{
				Name: "full", Coverage: "entire test suite (sqlite)",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					return []string{python(inv), "tests/runtests.py", "--noinput", parallel(inv)}, nil
				},
				CPU: lap.AllCPU, MinCPU: 4, Memory: 4 << 30,
				// The full suite is ~20k tests: minutes, not seconds.
				Estimate: 10 * time.Minute,
			},
			{
				Name: "related", Coverage: "test apps related to changed paths (heuristic)",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					labels := testLabels(inv)
					if len(labels) == 0 {
						return nil, nil
					}
					return append([]string{python(inv), "tests/runtests.py", "--noinput", parallel(inv)}, labels...), nil
				},
				CPU: lap.AllCPU, MinCPU: 1, Memory: 2 << 30,
			},
		},
		Classify: classifyRuntests,
	}

	cli.Main(lap.Config{
		Name:   "dev",
		Budget: 60 * time.Second,
		Tasks:  []lap.Task{black, isort, biome, flake8, tests},
	})
}

func python(inv lap.Invocation) string {
	p := filepath.Join(inv.Root, ".venv", "bin", "python")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return "python3"
}

func parallel(inv lap.Invocation) string {
	return fmt.Sprintf("--parallel=%d", max(1, inv.Workers))
}

// testLabels maps changed paths to runtests.py labels.
func testLabels(inv lap.Invocation) []string {
	if inv.Scope == nil {
		return nil
	}
	seen := map[string]bool{}
	add := func(label string) {
		if st, err := os.Stat(filepath.Join(inv.Root, "tests", label)); err == nil && st.IsDir() {
			seen[label] = true
		}
	}
	for _, f := range inv.Scope.Match([]string{"django/**/*.py", "tests/**/*.py"}) {
		parts := strings.Split(f, "/")
		switch {
		case parts[0] == "tests" && len(parts) > 2:
			add(parts[1])
		case parts[0] == "django":
			// django/contrib/auth/forms.py -> auth, auth_tests, contrib...
			for _, p := range parts[1 : len(parts)-1] {
				add(p)
				add(p + "_tests")
			}
			mod := strings.TrimSuffix(parts[len(parts)-1], ".py")
			if mod != "__init__" {
				add(mod)
				add(mod + "_tests")
			}
		}
	}
	var out []string
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func classifyRuntests(c *lap.Classification) {
	if c.ExitCode == 0 {
		c.Outcome = store.Passed
		return
	}
	c.Outcome = store.Findings
	for _, l := range strings.Split(c.Plain(), "\n") {
		if strings.HasPrefix(l, "FAIL: ") || strings.HasPrefix(l, "ERROR: ") {
			c.Findings = append(c.Findings, lap.Finding{Message: strings.TrimSpace(l)})
		}
	}
}
