package adapters

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/store"
	"github.com/vivster7/lap/term"
)

var jsFiles = []string{"*.js", "*.jsx", "*.ts", "*.tsx", "*.mjs", "*.cjs", "*.mts", "*.cts"}

// Prettier formats files (writer; --check in check-only mode).
func Prettier() lap.Task {
	return lap.Task{
		Name: "prettier", Groups: []string{"fmt"}, Phase: lap.Prepare,
		Files:   append([]string{"*.json", "*.md", "*.css", "*.scss", "*.yml", "*.yaml", "*.html", "*.vue"}, jsFiles...),
		Config:  []string{".prettierrc", ".prettierrc.*", "prettier.config.*", ".prettierignore"},
		PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{bin(inv.Root, "prettier"), "--ignore-unknown", "--log-level", "warn"}
				if inv.Mode == lap.CheckOnly {
					argv = append(argv, "--check")
				} else {
					argv = append(argv, "--write")
				}
				return append(argv, files(inv, ".")...), nil
			},
			Estimate: 4 * time.Second,
		}},
		Classify: func(c *lap.Classification) {
			switch c.ExitCode {
			case 0:
				c.Outcome = store.Passed
			case 1:
				c.Outcome = store.Findings
				for _, l := range strings.Split(c.Plain(), "\n") {
					if f, ok := strings.CutPrefix(strings.TrimSpace(l), "[warn] "); ok && !strings.Contains(f, " ") {
						c.Findings = append(c.Findings, lap.Finding{File: f, Message: "not formatted"})
					}
				}
			default:
				c.Outcome = store.ToolError
			}
		},
		Suggest: "run without --check to apply prettier",
	}
}

// ESLint lints JS/TS files, reading JSON results from a stdout pipe.
func ESLint() lap.Task {
	return lap.Task{
		Name: "eslint", Groups: []string{"lint"}, Phase: lap.Check,
		Files:   jsFiles,
		Config:  []string{"eslint.config.*", ".eslintrc", ".eslintrc.*", "package.json"},
		PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{bin(inv.Root, "eslint"), "--format", "json", "--no-warn-ignored"}
				if inv.Workers > 1 {
					argv = append(argv, "--concurrency", fmt.Sprint(inv.Workers))
				}
				return append(argv, files(inv, ".")...), nil
			},
			CPU: lap.AllCPU, MinCPU: 1, Memory: 2 << 30,
			Capture:  term.Pipes,
			Estimate: 10 * time.Second,
		}},
		Classify: classifyESLint,
	}
}

type eslintFile struct {
	FilePath string `json:"filePath"`
	Messages []struct {
		RuleID   string `json:"ruleId"`
		Severity int    `json:"severity"`
		Message  string `json:"message"`
		Line     int    `json:"line"`
		Column   int    `json:"column"`
	} `json:"messages"`
}

func classifyESLint(c *lap.Classification) {
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
	out, err := c.Stream(term.StreamStdout)
	if err != nil {
		return
	}
	var res []eslintFile
	if json.Unmarshal(out, &res) != nil {
		return
	}
	for _, f := range res {
		for _, m := range f.Messages {
			if m.Severity < 2 {
				continue
			}
			c.Findings = append(c.Findings, lap.Finding{File: f.FilePath, Line: m.Line, Col: m.Column, Code: m.RuleID, Message: m.Message})
		}
	}
}

// TSC typechecks a TypeScript project (project-wide; semantic).
func TSC(args ...string) lap.Task {
	if len(args) == 0 {
		args = []string{"--noEmit", "-p", "."}
	}
	return lap.Task{
		Name: "tsc", Groups: []string{"typecheck"}, Phase: lap.Check,
		Files:  []string{"*.ts", "*.tsx", "*.mts", "*.cts"},
		Config: []string{"tsconfig*.json", "package.json"},
		Variants: []lap.Variant{{
			Name:     "default",
			Cmd:      func(inv lap.Invocation) ([]string, error) { return append([]string{bin(inv.Root, "tsc"), "--pretty", "false"}, args...), nil },
			Memory:   3 << 30,
			Estimate: 30 * time.Second,
		}},
		Classify: classifyTSC,
	}
}

var tscRE = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\): error (TS\d+): (.*)$`)

func classifyTSC(c *lap.Classification) {
	if c.ExitCode == 0 {
		c.Outcome = store.Passed
		return
	}
	c.Outcome = store.Findings
	for _, l := range strings.Split(c.Plain(), "\n") {
		m := tscRE.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		c.Findings = append(c.Findings, lap.Finding{File: m[1], Line: ln, Col: col, Code: m[4], Message: m[5]})
	}
	if len(c.Findings) == 0 && c.ExitCode > 2 {
		c.Outcome = store.ToolError
	}
}

// Vitest runs tests: "full" (vitest run), then "changed" (vitest run
// --changed <base>, vitest's own module-graph selection).
func Vitest(args ...string) lap.Task {
	return lap.Task{
		Name: "vitest", Groups: []string{"test"}, Phase: lap.Check,
		Files:  jsFiles,
		Config: []string{"vitest.config.*", "vite.config.*", "package.json"},
		Variants: []lap.Variant{
			{
				Name: "full", Coverage: "entire test suite",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					return append([]string{bin(inv.Root, "vitest"), "run", fmt.Sprintf("--maxWorkers=%d", inv.Workers)}, args...), nil
				},
				CPU: lap.AllCPU, MinCPU: 2, Memory: 3 << 30,
			},
			{
				Name: "changed", Coverage: "tests related to changed files (vitest --changed)",
				Cmd: func(inv lap.Invocation) ([]string, error) {
					if inv.Scope == nil || inv.Scope.All || inv.Scope.Base == "" {
						return nil, nil
					}
					return append([]string{bin(inv.Root, "vitest"), "run", "--changed", inv.Scope.Base, "--passWithNoTests",
						fmt.Sprintf("--maxWorkers=%d", inv.Workers)}, args...), nil
				},
				CPU: lap.AllCPU, MinCPU: 1, Memory: 2 << 30,
			},
		},
	}
}

// Biome formats and lints (writer: biome check --write).
func Biome() lap.Task {
	return lap.Task{
		Name: "biome", Groups: []string{"fmt", "lint"}, Phase: lap.Prepare,
		Files:   append([]string{"*.json", "*.jsonc", "*.css"}, jsFiles...),
		Config:  []string{"biome.json", "biome.jsonc"},
		PerFile: true,
		Variants: []lap.Variant{{
			Name: "default",
			Cmd: func(inv lap.Invocation) ([]string, error) {
				argv := []string{bin(inv.Root, "biome"), "check", "--reporter", "summary"}
				if inv.Mode == lap.Fix {
					argv = append(argv, "--write")
				}
				return append(argv, files(inv, ".")...), nil
			},
			Estimate: 3 * time.Second,
		}},
	}
}
