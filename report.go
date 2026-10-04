package lap

import (
	"strings"
	"time"

	"github.com/vivster7/lap/scope"
	"github.com/vivster7/lap/store"
)

// EventKind identifies a Reporter event.
type EventKind int

const (
	RunStarted EventKind = iota
	PlanReady
	TaskStarted
	TaskFinished // includes deferred, blocked and skipped tasks
	Note
	RunFinished
)

// Event is delivered to a Reporter. Reporters are called from the engine's
// control loop and must return quickly.
type Event struct {
	Kind    EventKind
	Time    time.Time
	Elapsed time.Duration // since run start
	Task    string
	Attempt *store.Attempt
	Plan    *Plan
	Report  *Report
	Text    string
	// Running lists tasks running after this event.
	Running []string
}

// Reporter consumes events.
type Reporter interface {
	Report(Event)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(Event)

func (f ReporterFunc) Report(e Event) { f(e) }

// Report is the final result of a run.
type Report struct {
	RunID  string
	RunDir string
	Root   string
	Scope  *scope.Scope
	Mode   Mode

	Budget  time.Duration
	Elapsed time.Duration

	// Attempts in completion order, including deferred, blocked and skipped
	// tasks (those have zero Start).
	Attempts []store.Attempt
	// Findings by task name.
	Findings    map[string][]Finding
	Suggestions []string
	Notes       []string
	Plan        *Plan

	// ExitCode follows the starter convention: 0 no findings (even when
	// coverage was partial), 1 findings, 3 tool/config/cleanup error, 130
	// interrupted.
	ExitCode int
}

// Count returns the number of attempts with outcome o.
func (r *Report) Count(o store.Outcome) int {
	n := 0
	for _, a := range r.Attempts {
		if a.Outcome == o {
			n++
		}
	}
	return n
}

// Latest returns the last attempt of each task (a remediation re-run
// supersedes the first attempt), in first-seen order.
func (r *Report) Latest() []store.Attempt {
	pos := map[string]int{}
	var out []store.Attempt
	for _, a := range r.Attempts {
		if i, ok := pos[a.Task]; ok {
			out[i] = a
			continue
		}
		pos[a.Task] = len(out)
		out = append(out, a)
	}
	return out
}

// LatestCount counts latest attempts with outcome o.
func (r *Report) LatestCount(o store.Outcome) int {
	n := 0
	for _, a := range r.Latest() {
		if a.Outcome == o {
			n++
		}
	}
	return n
}

// Complete reports whether every selected task ran to completion with its
// broadest variant.
func (r *Report) Complete() bool {
	for _, a := range r.Latest() {
		switch a.Outcome {
		case store.Deferred, store.Blocked, store.TimedOut, store.Interrupted, store.Stale, store.ToolError:
			return false
		}
		if strings.HasPrefix(a.Reason, narrowedReason) {
			return false
		}
	}
	return true
}

const narrowedReason = "narrowed: broader variant did not fit"

// PlanEntry is one task in a plan.
type PlanEntry struct {
	Task       string        `json:"task"`
	Phase      string        `json:"phase"`
	Status     string        `json:"status"` // run, deferred, skipped, blocked
	Variant    string        `json:"variant,omitempty"`
	Coverage   string        `json:"coverage,omitempty"`
	Narrowed   bool          `json:"narrowed,omitempty"`
	Estimate   time.Duration `json:"estimate_ns,omitempty"`
	Source     string        `json:"estimate_source,omitempty"`
	Confidence string        `json:"confidence,omitempty"`
	Start      time.Duration `json:"start_ns,omitempty"` // simulated offset
	End        time.Duration `json:"end_ns,omitempty"`
	CPU        int           `json:"cpu,omitempty"`
	All        bool          `json:"all_files,omitempty"`
	Files      int           `json:"files,omitempty"`
	Reason     string        `json:"reason,omitempty"`
}

// Plan is a prediction of what a run will do, made by simulating the policy
// with duration estimates. It is not a promise.
type Plan struct {
	Budget    time.Duration `json:"budget_ns"`
	Cutoff    time.Duration `json:"cutoff_ns"`
	CPU       int           `json:"cpu"`
	Memory    int64         `json:"memory"`
	Mode      string        `json:"mode"`
	ScopeDesc string        `json:"scope"`
	Entries   []PlanEntry   `json:"entries"`
	Notes     []string      `json:"notes,omitempty"`
}
