// Package lap is a small framework for building your own bounded local
// verification command: formatters, generators, linters, typechecks and fast
// tests run in parallel inside a time budget, without overwhelming the
// laptop, and the command tells you clearly what it did not get to.
//
// A company owns a small main program that declares Tasks and hands them to
// [Run] (or to the starter CLI in package cli). The core owns the parts that
// are hard to get right: process supervision (package proc), terminal capture
// (package term), scheduling under a deadline with a machine-wide resource
// pool (package pool), change selection (package scope) and run history
// (package store).
package lap

import (
	"fmt"
	"time"

	"github.com/vivster7/lap/scope"
	"github.com/vivster7/lap/term"
)

// Phase orders work. All Prepare tasks (formatters, generators: tasks that
// write the workspace) finish before any Check task starts.
type Phase int

const (
	Prepare Phase = iota
	Check
)

func (p Phase) String() string {
	if p == Prepare {
		return "prepare"
	}
	return "check"
}

// Mode selects whether writers may modify the workspace.
type Mode int

const (
	// Fix lets Prepare tasks rewrite files (the default local mode).
	Fix Mode = iota
	// CheckOnly asks every task for its read-only form; writers report
	// what they would change instead of changing it.
	CheckOnly
)

func (m Mode) String() string {
	if m == CheckOnly {
		return "check"
	}
	return "fix"
}

// AllCPU as Variant.CPU asks for as many CPU slots as are free, up to the
// run's allowance.
const AllCPU = -1

// Invocation is what a Variant's command builder receives.
type Invocation struct {
	Root    string   // absolute repository toplevel
	Mode    Mode     // Fix or CheckOnly
	Files   []string // selected repo-relative files (PerFile tasks, not All)
	All     bool     // full scope: use the project-wide form
	Workers int      // CPU slots granted: pass to -j / -n / --concurrency
	Scope   *scope.Scope
}

// Variant is one way to run a task. Tasks list variants broadest first; the
// scheduler picks the broadest one that fits the remaining time.
type Variant struct {
	Name     string // e.g. "full", "changed"
	Coverage string // human description of what this variant verifies

	// Cmd returns the argv to execute. Returning a nil argv skips the task
	// (for example, nothing relevant to do).
	Cmd func(inv Invocation) ([]string, error)
	Dir string   // working directory relative to Root
	Env []string // extra environment entries

	// CPU is the number of CPU slots wanted (default 1), or AllCPU.
	// MinCPU is the fewest slots worth starting with (default: CPU, or 1 for
	// AllCPU). Workers in the Invocation is the number granted.
	CPU, MinCPU int
	// Memory is the expected peak memory in bytes (0: DefaultMemory).
	Memory int64
	// Estimate is the configured duration fallback when there is no history.
	Estimate time.Duration
	// Capture selects how output is captured (default term.PTY).
	Capture term.Mode
}

// DefaultMemory is reserved for variants that do not declare Memory.
const DefaultMemory = 512 << 20

// Task is one unit of verification.
type Task struct {
	Name     string
	Groups   []string // e.g. "fmt", "lint", "typecheck", "test"
	Phase    Phase
	After    []string // explicit prerequisites (task names)
	Priority int      // higher runs first

	// Files are selection globs (doublestar; patterns without "/" match at
	// any depth). The task applies when a changed file matches. Nil means
	// the task always applies.
	Files []string
	// Config globs: a change to any of these forces full scope for the task.
	Config []string
	// PerFile tasks receive the matching changed files in Invocation.Files;
	// other tasks run project-wide.
	PerFile bool
	// MaxFiles switches a PerFile task to its project-wide form when more
	// files than this are selected (default 1000).
	MaxFiles int

	Variants []Variant

	// Classify refines the outcome from the exit status and output
	// (optional). The default treats exit 0 as passed and any other exit as
	// findings, 126/127 as a tool error.
	Classify func(c *Classification)
	// Suggest is shown when the task reports findings, e.g.
	// "run `dev fmt` to fix".
	Suggest string
}

func (t *Task) validate() error {
	if t.Name == "" {
		return fmt.Errorf("lap: task with empty name")
	}
	if len(t.Variants) == 0 {
		return fmt.Errorf("lap: task %q has no variants", t.Name)
	}
	for i, v := range t.Variants {
		if v.Cmd == nil {
			return fmt.Errorf("lap: task %q variant %d has no Cmd", t.Name, i)
		}
		if v.CPU < AllCPU {
			return fmt.Errorf("lap: task %q variant %q: bad CPU %d", t.Name, v.Name, v.CPU)
		}
	}
	return nil
}

func (v Variant) name() string {
	if v.Name == "" {
		return "default"
	}
	return v.Name
}

func (v Variant) memory() int64 {
	if v.Memory > 0 {
		return v.Memory
	}
	return DefaultMemory
}

// cpuRange returns the minimum and maximum slots for this variant under an
// allowance.
func (v Variant) cpuRange(allowance int) (min, max int) {
	max = v.CPU
	switch {
	case max == AllCPU:
		max = allowance
		min = 1
	case max <= 0:
		max = 1
		min = 1
	default:
		min = max
	}
	if v.MinCPU > 0 {
		min = v.MinCPU
	}
	if max > allowance {
		max = allowance
	}
	if min > max {
		min = max
	}
	if min < 1 {
		min = 1
	}
	return min, max
}

func (t *Task) hasGroup(g string) bool {
	for _, x := range t.Groups {
		if x == g {
			return true
		}
	}
	return false
}

// Cmd is a convenience Variant command builder for a fixed argv. PerFile
// tasks get their selected files appended unless the invocation is All.
func Cmd(argv ...string) func(Invocation) ([]string, error) {
	return func(inv Invocation) ([]string, error) {
		out := append([]string(nil), argv...)
		if !inv.All && len(inv.Files) > 0 {
			out = append(out, inv.Files...)
		}
		return out, nil
	}
}

// Shell runs a shell command line with sh -c. Selected files are passed as
// positional parameters ("$@") when present.
func Shell(script string) func(Invocation) ([]string, error) {
	return func(inv Invocation) ([]string, error) {
		out := []string{"sh", "-c", script, "sh"}
		if !inv.All {
			out = append(out, inv.Files...)
		}
		return out, nil
	}
}
