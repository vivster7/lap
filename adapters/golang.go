package adapters

import (
	"fmt"
	"strings"
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/store"
)

var goFiles = []string{"*.go"}
var goConfig = []string{"go.mod", "go.sum", "go.work"}

// Gofmt formats Go files (writer). In check-only mode it lists unformatted
// files and reports them as findings.
func Gofmt() lap.Task {
	return lap.Task{
		Name: "gofmt", Groups: []string{"fmt"}, Phase: lap.Prepare,
		Files: goFiles, PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{"gofmt", "-l"}
				if inv.Mode == lap.Fix {
					argv = append(argv, "-w")
				}
				return append(argv, files(inv, ".")...), nil
			},
			Estimate: 2 * time.Second,
		}},
		Classify: func(c *lap.Classification) {
			if c.ExitCode != 0 {
				c.Outcome = store.ToolError
				return
			}
			c.Outcome = store.Passed
			if c.Mode == lap.CheckOnly {
				for _, l := range strings.Split(strings.TrimSpace(c.Plain()), "\n") {
					if l = strings.TrimSpace(l); l != "" {
						c.Findings = append(c.Findings, lap.Finding{File: l, Message: "not gofmt-formatted"})
					}
				}
				if len(c.Findings) > 0 {
					c.Outcome = store.Findings
				}
			}
		},
		Suggest: "run without --check to apply gofmt",
	}
}

// goPackages returns package patterns for the changed Go files, or nil in
// full scope.
func goPackages(inv lap.Invocation) []string {
	ch := changed(inv, "*.go")
	if len(ch) == 0 {
		return nil
	}
	return dirsOf(ch)
}

// GoVet runs go vet: "all" over ./..., "changed" over the packages that
// contain changed files.
func GoVet() lap.Task {
	return lap.Task{
		Name: "go-vet", Groups: []string{"lint"}, Phase: lap.Check,
		Files: goFiles, Config: goConfig,
		Variants: []lap.Variant{
			{
				Name: "all", Coverage: "every package",
				Cmd: lap.Cmd("go", "vet", "./..."),
				CPU: lap.AllCPU, MinCPU: 2, Memory: 2 << 30,
			},
			{
				Name: "changed", Coverage: "packages with changed files",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					pkgs := goPackages(inv)
					if pkgs == nil {
						return []string{"go", "vet", "./..."}, nil
					}
					return append([]string{"go", "vet"}, pkgs...), nil
				},
				CPU: lap.AllCPU, MinCPU: 1, Memory: 1 << 30,
			},
		},
		Classify: lap.ClassifyFileLine(),
	}
}

// GoBuild compiles every package.
func GoBuild() lap.Task {
	return lap.Task{
		Name: "go-build", Groups: []string{"build"}, Phase: lap.Check,
		Files: goFiles, Config: goConfig,
		Variants: []lap.Variant{{
			Name: "all", Cmd: lap.Cmd("go", "build", "./..."),
			CPU: lap.AllCPU, MinCPU: 2, Memory: 2 << 30,
		}},
		Classify: lap.ClassifyFileLine(),
	}
}

// GoTest runs tests: "all" (./...), then "changed" (packages with changed
// files; not reverse dependencies) as a smaller variant.
func GoTest(extra ...string) lap.Task {
	return lap.Task{
		Name: "go-test", Groups: []string{"test"}, Phase: lap.Check,
		Files: goFiles, Config: goConfig,
		Variants: []lap.Variant{
			{
				Name: "all", Coverage: "every package",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					return append(append([]string{"go", "test", fmt.Sprintf("-p=%d", inv.Workers)}, extra...), "./..."), nil
				},
				CPU: lap.AllCPU, MinCPU: 2, Memory: 3 << 30,
			},
			{
				Name: "changed", Coverage: "packages with changed files (no reverse deps)",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					pkgs := goPackages(inv)
					if pkgs == nil {
						return nil, nil
					}
					return append(append([]string{"go", "test", fmt.Sprintf("-p=%d", inv.Workers)}, extra...), pkgs...), nil
				},
				CPU: lap.AllCPU, MinCPU: 1, Memory: 2 << 30,
			},
		},
		Classify: classifyGoTest,
	}
}

func classifyGoTest(c *lap.Classification) {
	if c.ExitCode == 0 {
		c.Outcome = store.Passed
		return
	}
	c.Outcome = store.Findings
	for _, l := range strings.Split(c.Plain(), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "--- FAIL:") || strings.HasPrefix(t, "FAIL\t") {
			c.Findings = append(c.Findings, lap.Finding{Message: t})
		}
	}
	if len(c.Findings) == 0 && strings.Contains(c.Plain(), "build failed") {
		c.Findings = append(c.Findings, lap.Finding{Message: "build failed"})
	}
}

// GolangciLint runs golangci-lint: "all", then "changed" packages.
func GolangciLint() lap.Task {
	return lap.Task{
		Name: "golangci-lint", Groups: []string{"lint"}, Phase: lap.Check,
		Files: goFiles, Config: append([]string{".golangci.yml", ".golangci.yaml", ".golangci.toml"}, goConfig...),
		Variants: []lap.Variant{
			{
				Name: "all", Coverage: "every package",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					return []string{"golangci-lint", "run", fmt.Sprintf("--concurrency=%d", inv.Workers), "./..."}, nil
				},
				CPU: lap.AllCPU, MinCPU: 2, Memory: 3 << 30,
			},
			{
				Name: "changed", Coverage: "packages with changed files",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					pkgs := goPackages(inv)
					if pkgs == nil {
						return nil, nil
					}
					return append([]string{"golangci-lint", "run", fmt.Sprintf("--concurrency=%d", inv.Workers)}, pkgs...), nil
				},
				CPU: lap.AllCPU, MinCPU: 1, Memory: 2 << 30,
			},
		},
		Classify: lap.ClassifyFileLine(),
	}
}

// Gazelle runs a standalone gazelle binary to update BUILD files (writer);
// check-only mode uses -mode=diff. Projects with custom Gazelle extensions
// should point argv at their own gazelle build.
func Gazelle(argv ...string) lap.Task {
	if len(argv) == 0 {
		argv = []string{"gazelle"}
	}
	return lap.Task{
		Name: "gazelle", Groups: []string{"gen"}, Phase: lap.Prepare,
		Files:  []string{"*.go", "*.proto", "BUILD", "BUILD.bazel"},
		Config: []string{"WORKSPACE", "WORKSPACE.bazel", "MODULE.bazel"},
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				out := append([]string(nil), argv...)
				if inv.Mode == lap.CheckOnly {
					out = append(out, "-mode=diff")
				}
				return out, nil
			},
			Estimate: 10 * time.Second,
		}},
		Suggest: "run without --check to update BUILD files",
	}
}

var _ = hasPrefixAny
