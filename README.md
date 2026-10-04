# lap

**Build your own bounded inner-loop command.** `lap` is a small Go framework
for the command you run before pushing: formatters, generators, linters,
typechecks and fast tests, run in parallel **inside a time budget** (60s by
default), without making the laptop unusable — and honest about what it did
not get to.

```
$ dev
dev · budget 1m0s · fix mode · 9 cpu · 1 changed files vs origin/main (merge-base 961c9ba409)
plan: 3 to run, 1 deferred, 0 skipped (dev plan for details)
  ▸ gofmt [default · 1 file · 1 cpu]
  ✎ gofmt 0s fixed 1 file(s)
  – go-build deferred all needs ~57s (timed out after 57.031s (lower bound)), 57s left
  ▸ go-test [changed · 3 cpu]
  ▸ golangci-lint [all · 3 cpu]
  ✓ go-test 600ms narrowed: broader variant did not fit
  ✗ golangci-lint 35.7s 1 finding

── golangci-lint (findings, exit 1) ──
model/labels/labels_common.go:247:6: func lapDemoHelper is unused (unused)

dev in 35.8s: 1 passed, 1 fixed, 1 findings, 1 deferred
not verified locally: go-build, go-test (narrowed to changed)
run 20261004T053630Z-4b3fb0 · dev logs <task> · dev why
```
*(the [Prometheus example](examples/prometheus) on a real change)*

Inspired by [pi](https://github.com/badlogic/pi-mono): a small core that does
the hard parts well, and a **company-owned `main` program** that composes it.
Tools are installed and versioned by [mise](https://mise.jdx.dev); lap owns
scheduling, process supervision, terminal capture and run history.

## Why

Pre-commit frameworks run hooks; task runners run a graph; neither treats
*time* as the constraint. Locally, the useful question is "what can I verify
on this machine in the next minute?":

- **A deadline, not a timeout per task.** Every run has one budget covering
  startup, preparation, checks and cleanup. Work that will not fit is
  **deferred up front** (with the reason), not started and killed.
- **Variants.** A task lists ways to run it, broadest first — `go test ./...`,
  then only changed packages. The scheduler picks the broadest variant that
  fits, and the report says when coverage was narrowed.
- **History.** Durations are learned per task/variant/scope size and machine;
  timeouts count as lower bounds, not fast samples.
- **The laptop stays usable.** CPU/memory admission against a per-user,
  machine-wide pool shared by every lap run (other worktrees, other repos,
  parallel coding agents), with tools' own worker counts pinned to the CPU
  slots they were granted, at lower priority.
- **Formatters first.** Writers run in a preparation phase; overlapping writers
  run in order; checks start after preparation and see its result.
- **Real process cleanup.** Every task runs in its own session or process
  group; on timeout the whole tree is terminated and *verified* gone
  (grandchildren included — `mise --timeout` and Go's `exec.CommandContext`
  both leave them running).
- **Output you can inspect later.** Each task runs under a real PTY (colors,
  progress bars, TTY detection behave as in your terminal); the exact bytes are
  journaled and rendered into plain and terminal views: `dev logs <task>`.
- **Exit 0 on partial coverage.** A bounded run is expected not to finish
  everything; exit 1 means findings. Coverage is always in the report.

## Quick start

A company's `dev` is a small Go program:

```go
package main

import (
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/adapters"
	"github.com/vivster7/lap/cli"
)

func main() {
	test := adapters.Pytest(true) // full suite, then only changed test files
	cli.Main(lap.Config{
		Name:   "dev",
		Budget: 60 * time.Second,
		Tasks: []lap.Task{
			adapters.RuffFormat(), // writer: runs first
			adapters.RuffCheck(),
			adapters.Mypy(),
			test,
		},
	})
}
```

```toml
# mise.toml
[tools]
go = "1.27"
ruff = "latest"

[tasks.dev]
run = "go run ./tools/dev"
```

Build it during setup (`go build -o bin/dev ./tools/dev`) so compilation is not
inside the timed run; `go run` is fine while iterating.

```
dev [group|task ...]     run what applies to your changes, within the budget
dev plan                 what would run, which variant, and why
dev --check              read-only: formatters report instead of rewriting
dev --all                full scope instead of changed files
dev --timeout 2m         a different budget
dev --autofix            apply rule fixes in one remediation pass
dev logs <task> [--raw]  output of the task's last attempt
dev why                  decisions and outcomes of the last run
dev stats                timing and outcome history
dev pool                 who holds machine-wide CPU/memory tokens
```

## Writing tasks

A `lap.Task` is plain data; adapters return ready-made ones you can edit.

```go
lap.Task{
	Name:    "tests",
	Groups:  []string{"test"},       // `dev test`
	Phase:   lap.Check,              // or lap.Prepare for writers
	Files:   []string{"**/*.py"},    // applies when a matching file changed
	Config:  []string{"pytest.ini"}, // a change here forces full scope
	PerFile: false,                  // true: changed files are passed as args
	Variants: []lap.Variant{
		{Name: "full", Cmd: lap.Cmd("pytest", "-q"), CPU: lap.AllCPU, Estimate: 5 * time.Minute},
		{Name: "changed", Cmd: changedTests, CPU: lap.AllCPU, MinCPU: 1},
	},
	Classify: myClassifier, // exit code + output -> passed/findings/error + findings
}
```

A variant's `Cmd` receives an `Invocation` (repo root, fix/check mode, selected
files, the CPU slots granted, the change scope) and returns an argv. Rules map
a task's findings to a suggestion or a fix (`lap.Rule`); with `--autofix` the
fix runs once, and the affected task is re-checked if time remains.

Adapters exist for ruff, black, isort, flake8, mypy, pytest, gofmt, go
vet/build/test, golangci-lint, gazelle, prettier, oxfmt, biome, eslint, tsc
and vitest. Anything else is a few lines.

## Examples

Real `dev` commands for large open-source projects, run on real changes:

| example | project | what it shows |
|---|---|---|
| [examples/prometheus](examples/prometheus) | Go, 1.7k files | full test suite deferred, changed packages tested; golangci-lint finds the bug |
| [examples/django](examples/django) | Python, 7k files | black + isort fix the file, flake8 findings, related test apps in 30s instead of the full suite |
| [examples/vite](examples/vite) | TS monorepo | per-package typechecks selected by path, build → tests dependency, vitest |
| [cmd/dev](cmd/dev) | lap itself | `mise run dev` |

## Packages

| package | role |
|---|---|
| `lap` | tasks, variants, policy, engine, plans, rules, reports |
| `cli` | starter command line (run, plan, logs, why, stats, tasks, pool) |
| `adapters` | ready-made tasks for common tools |
| `proc` | process supervision: owned groups/sessions, verified cleanup |
| `term` | PTY/pipe capture, byte-exact journal, rendered and plain views |
| `run` | one supervised execution (`proc` + `term`) |
| `pool` | machine-wide, per-user CPU/memory tokens (flock, no daemon) |
| `scope` | git change selection (merge-base with the default branch) |
| `store` | run records, shared timing history, estimates, retention |

The scheduling `Policy` is an interface: a pure function from a snapshot
(ready tasks with estimates, remaining time, free resources) to admissions and
deferrals. The engine validates every admission; policies never launch
processes.

## Status and limits

Pre-1.0. Linux is tested; macOS compiles and runs in CI, but PTY hangup and
non-blocking master behavior there are less proven. Known limits, by design
or not yet done (see [docs/design.md](docs/design.md)):

- If the lap process itself is SIGKILLed, its tasks' tokens are released while
  orphaned tasks may still run; recovery is documented, not automatic.
- A grandchild that starts its own session and closes its output can escape
  cleanup verification.
- Changed-file test variants are selection heuristics, not impact analysis;
  the report labels them as narrowed coverage.
- Writers run in place: an interrupted formatter can leave partial changes
  (reported, never rolled back).
- No result cache of its own (tools' caches are used); Windows is out of scope.

MIT licensed.
