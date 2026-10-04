// Command dev is an example inner-loop command for the Vite monorepo
// (github.com/vitejs/vite), built with lap.
//
// It runs what Vite's contributors run locally (oxfmt, eslint, per-package
// typechecks, vitest unit tests), but only for what changed and within a
// time budget: each package's typecheck runs only when that package changed,
// eslint and oxfmt get just the changed files, and the unit suite narrows to
// `vitest --changed` when the whole suite would not fit. The e2e suites
// (playwright) are left to CI.
//
// Usage, from a Vite checkout after `pnpm install`:
//
//	go run github.com/vivster7/lap/examples/vite@latest [group|task ...]
package main

import (
	"path/filepath"
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/adapters"
	"github.com/vivster7/lap/cli"
)

func main() {
	format := adapters.Oxfmt()

	lint := adapters.ESLint()
	lint.Variants[0].Cmd = func(inv lap.Invocation) ([]string, error) {
		argv := []string{filepath.Join(inv.Root, "node_modules/.bin/eslint"), "--cache", "--format", "json",
			"--no-warn-ignored", "--concurrency", "auto"}
		if inv.All {
			return append(argv, "."), nil
		}
		return append(argv, inv.Files...), nil
	}

	// Vite's unit tests and type tests import the built package, so build it
	// first (rolldown, ~4s) whenever the package changed.
	build := lap.Task{
		Name:   "build-vite",
		Groups: []string{"build"},
		Phase:  lap.Check,
		Files:  []string{"packages/vite/**"},
		Config: []string{"pnpm-lock.yaml"},
		Variants: []lap.Variant{{
			Name:     "default",
			Cmd:      lap.Cmd("pnpm", "--filter", "./packages/vite", "run", "build"),
			CPU:      2,
			Estimate: 8 * time.Second,
		}},
		Classify: lap.ClassifyFileLine(),
	}
	vitest := adapters.Vitest()
	vitest.After = []string{"build-vite"}

	tasks := []lap.Task{format, lint, build,
		typecheck("typecheck-scripts", "scripts", "tsc -p scripts --noEmit"),
		typecheck("typecheck-vite", "packages/vite",
			"cd packages/vite && tsc && tsc -p src/node && tsc -p src/client && tsc -p src/module-runner && "+
				"tsc -p src/shared && tsc -p src/node/__tests_dts__ && tsc -p src/module-runner/__tests_dts__"),
		typecheck("typecheck-create-vite", "packages/create-vite", "cd packages/create-vite && tsc"),
		typecheck("typecheck-plugin-legacy", "packages/plugin-legacy", "cd packages/plugin-legacy && tsc"),
		vitest,
	}
	for i := range tasks {
		if tasks[i].Name == "typecheck-vite" {
			tasks[i].After = []string{"build-vite"}
		}
	}
	cli.Main(lap.Config{
		Name:   "dev",
		Budget: 60 * time.Second,
		Tasks:  tasks,
	})
}

// typecheck is a project-wide tsc task that applies only when files under
// dir (or the root TypeScript config) change.
func typecheck(name, dir, script string) lap.Task {
	return lap.Task{
		Name:   name,
		Groups: []string{"typecheck"},
		Phase:  lap.Check,
		Files:  []string{dir + "/**/*.ts", dir + "/**/*.tsx", dir + "/**/tsconfig*.json", dir + "/package.json"},
		Config: []string{"tsconfig*.json", "pnpm-lock.yaml"},
		Variants: []lap.Variant{{
			Name: "default",
			// Tasks run at the repository root; prefer the workspace's tsc.
			Cmd:    lap.Shell(`export PATH="$PWD/node_modules/.bin:$PATH"; ` + script),
			Memory: 2 << 30,
		}},
		Classify: lap.ClassifyFileLine(),
	}
}
