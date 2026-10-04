package adapters

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/store"
)

var pyFiles = []string{"*.py", "*.pyi"}
var pyConfig = []string{"pyproject.toml", "setup.cfg", "tox.ini"}

// RuffFormat formats Python files with `ruff format` (a writer; --check in
// check-only mode).
func RuffFormat() lap.Task {
	return lap.Task{
		Name:    "ruff-format",
		Groups:  []string{"fmt"},
		Phase:   lap.Prepare,
		Files:   pyFiles,
		Config:  []string{"pyproject.toml", "ruff.toml", ".ruff.toml"},
		PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{bin(inv.Root, "ruff"), "format"}
				if inv.Mode == lap.CheckOnly {
					argv = append(argv, "--check", "--diff")
				}
				return append(argv, files(inv, ".")...), nil
			},
			Estimate: 2 * time.Second,
		}},
		Classify: lap.ClassifyFileLine(2),
		Suggest:  "run without --check to apply formatting",
	}
}

// RuffCheck lints Python files with `ruff check`.
func RuffCheck() lap.Task {
	return lap.Task{
		Name:    "ruff-check",
		Groups:  []string{"lint"},
		Phase:   lap.Check,
		Files:   pyFiles,
		Config:  []string{"pyproject.toml", "ruff.toml", ".ruff.toml"},
		PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				return append([]string{bin(inv.Root, "ruff"), "check", "--output-format", "concise", "--no-fix"}, files(inv, ".")...), nil
			},
			Estimate: 2 * time.Second,
		}},
		Classify: classifyRuff,
	}
}

var ruffRE = regexp.MustCompile(`^(.+?):(\d+):(\d+): ([A-Z]+[0-9]*) (.*)$`)

func classifyRuff(c *lap.Classification) {
	switch c.ExitCode {
	case 0:
		c.Outcome = store.Passed
		return
	case 1:
		c.Outcome = store.Findings
	default:
		c.Outcome = store.ToolError
		return
	}
	for _, line := range strings.Split(c.Plain(), "\n") {
		m := ruffRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		c.Findings = append(c.Findings, lap.Finding{File: m[1], Line: ln, Col: col, Code: m[4], Message: m[5]})
	}
}

// Black formats Python files (writer).
func Black() lap.Task {
	return lap.Task{
		Name: "black", Groups: []string{"fmt"}, Phase: lap.Prepare,
		Files: pyFiles, Config: pyConfig, PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{bin(inv.Root, "black"), "--quiet"}
				if inv.Mode == lap.CheckOnly {
					argv = append(argv, "--check", "--diff")
				}
				return append(argv, files(inv, ".")...), nil
			},
			Estimate: 3 * time.Second,
		}},
		Classify: lap.ClassifyFileLine(123),
	}
}

// Isort sorts imports (writer).
func Isort() lap.Task {
	return lap.Task{
		Name: "isort", Groups: []string{"fmt"}, Phase: lap.Prepare,
		Files: pyFiles, Config: append([]string{".isort.cfg"}, pyConfig...), PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{bin(inv.Root, "isort"), "--quiet"}
				if inv.Mode == lap.CheckOnly {
					argv = append(argv, "--check-only", "--diff")
				}
				return append(argv, files(inv, ".")...), nil
			},
			Estimate: 3 * time.Second,
		}},
	}
}

// Flake8 lints Python files.
func Flake8() lap.Task {
	return lap.Task{
		Name: "flake8", Groups: []string{"lint"}, Phase: lap.Check,
		Files: pyFiles, Config: append([]string{".flake8"}, pyConfig...), PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{bin(inv.Root, "flake8")}
				if inv.Workers > 1 {
					argv = append(argv, "-j", fmt.Sprint(inv.Workers))
				}
				return append(argv, files(inv, ".")...), nil
			},
			CPU: lap.AllCPU, MinCPU: 1,
			Estimate: 5 * time.Second,
		}},
		Classify: lap.ClassifyFileLine(),
	}
}

// Mypy typechecks. Typechecking is semantic: it always reads the whole
// project, so it is project-wide whatever changed.
func Mypy(args ...string) lap.Task {
	if len(args) == 0 {
		args = []string{"."}
	}
	return lap.Task{
		Name: "mypy", Groups: []string{"typecheck"}, Phase: lap.Check,
		Files: pyFiles, Config: append([]string{"mypy.ini", ".mypy.ini"}, pyConfig...),
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				return append([]string{bin(inv.Root, "mypy")}, args...), nil
			},
			Memory:   2 << 30,
			Estimate: 30 * time.Second,
		}},
		Classify: lap.ClassifyFileLine(2),
	}
}

// Pytest runs tests. The "full" variant runs the given args (default: the
// whole suite) with pytest-xdist when Workers > 1 (pass xdist=false if the
// project does not use it); the "changed" variant runs only changed test
// files (test_*.py / *_test.py). Changed-file selection is not sound test
// impact analysis: the coverage label says so.
func Pytest(xdist bool, args ...string) lap.Task {
	workers := func(inv lap.Invocation) []string {
		if xdist && inv.Workers > 1 {
			return []string{"-n", fmt.Sprint(inv.Workers)}
		}
		return nil
	}
	return lap.Task{
		Name: "pytest", Groups: []string{"test"}, Phase: lap.Check,
		Files: pyFiles, Config: append([]string{"pytest.ini", "conftest.py"}, pyConfig...),
		Variants: []lap.Variant{
			{
				Name: "full", Coverage: "entire test suite",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					argv := append([]string{bin(inv.Root, "pytest"), "-q"}, workers(inv)...)
					return append(argv, args...), nil
				},
				CPU: lap.AllCPU, MinCPU: 2, Memory: 2 << 30,
			},
			{
				Name: "changed", Coverage: "changed test files only (not impact analysis)",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					var tests []string
					for _, f := range changed(inv, "test_*.py", "*_test.py") {
						tests = append(tests, f)
					}
					if len(tests) == 0 {
						return nil, nil
					}
					return append(append([]string{bin(inv.Root, "pytest"), "-q"}, workers(inv)...), tests...), nil
				},
				CPU: lap.AllCPU, MinCPU: 1, Memory: 1 << 30,
			},
		},
		Classify: classifyPytest,
	}
}

func classifyPytest(c *lap.Classification) {
	switch c.ExitCode {
	case 0:
		c.Outcome = store.Passed
	case 1:
		c.Outcome = store.Findings
		for _, line := range strings.Split(c.Plain(), "\n") {
			if strings.HasPrefix(line, "FAILED ") || strings.HasPrefix(line, "ERROR ") {
				c.Findings = append(c.Findings, lap.Finding{Message: strings.TrimSpace(line)})
			}
		}
	case 5:
		c.Outcome = store.Passed
		c.Reason = "no tests collected"
	default:
		c.Outcome = store.ToolError
	}
}
