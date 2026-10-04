package store

import "time"

// Outcome is the execution result of one task attempt.
type Outcome string

const (
	Passed      Outcome = "passed"
	Fixed       Outcome = "fixed" // writer changed files
	Findings    Outcome = "findings"
	ToolError   Outcome = "error"
	TimedOut    Outcome = "timeout"
	Deferred    Outcome = "deferred"    // not admitted (budget/resources)
	Blocked     Outcome = "blocked"     // prerequisite incomplete
	Skipped     Outcome = "skipped"     // not applicable / no files
	Interrupted Outcome = "interrupted" // writer interrupted, or user cancel
	Stale       Outcome = "stale"       // inputs changed during run
)

// Completed reports whether the outcome is a finished, useful execution
// (passed, fixed or findings) whose duration describes the task.
func (o Outcome) Completed() bool { return o == Passed || o == Fixed || o == Findings }

// Attempt is one task attempt. It is stored as AttemptDir/meta.json and as
// one line of the history index.
type Attempt struct {
	RunID     string `json:"run_id"`
	AttemptID string `json:"attempt_id"`
	Task      string `json:"task"`
	Variant   string `json:"variant,omitempty"`
	Phase     string `json:"phase,omitempty"` // "prepare"|"check"|"remediation"

	Groups     []string `json:"groups,omitempty"`
	Mode       string   `json:"mode,omitempty"` // "fix"|"check"
	Worktree   string   `json:"worktree,omitempty"`
	Workers    int      `json:"workers,omitempty"`
	CPU        int      `json:"cpu,omitempty"`
	Memory     int64    `json:"memory,omitempty"`
	ScopeAll   bool     `json:"scope_all,omitempty"`
	ScopeFiles int      `json:"scope_files,omitempty"`
	// ScopeBucket defaults to ScopeBucketFor(ScopeAll, ScopeFiles) when recorded.
	ScopeBucket string `json:"scope_bucket,omitempty"`

	Queued time.Time `json:"queued,omitzero"`
	Start  time.Time `json:"start,omitzero"` // zero for deferred/blocked/skipped
	End    time.Time `json:"end,omitzero"`

	ExitCode int     `json:"exit_code"`
	Signal   string  `json:"signal,omitempty"`
	Outcome  Outcome `json:"outcome"`
	Reason   string  `json:"reason,omitempty"`   // human reason for deferred/blocked/skipped/error
	Censored bool    `json:"censored,omitempty"` // killed by deadline: duration is a lower bound
	Findings int     `json:"findings,omitempty"`

	ChangedFiles []string `json:"changed_files,omitempty"` // writers: files changed

	CaptureMode     string `json:"capture_mode,omitempty"`
	CaptureDir      string `json:"capture_dir,omitempty"` // relative to the run dir
	CaptureComplete bool   `json:"capture_complete,omitempty"`

	Contended bool          `json:"contended,omitempty"`  // other lap runs held pool tokens during this attempt
	MaxRSSKB  int64         `json:"max_rss_kb,omitempty"` // largest single process (not tree total)
	UserCPU   time.Duration `json:"user_cpu_ns,omitempty"`
	SysCPU    time.Duration `json:"sys_cpu_ns,omitempty"`

	Machine   string `json:"machine,omitempty"`    // hostname
	ConfigKey string `json:"config_key,omitempty"` // engine-provided hash of tool/config identity
}

// Duration is End-Start, or 0 if the attempt never started or has no end.
func (a Attempt) Duration() time.Duration {
	if a.Start.IsZero() || a.End.IsZero() || a.End.Before(a.Start) {
		return 0
	}
	return a.End.Sub(a.Start)
}

// censored reports whether the attempt's duration is only a lower bound.
func (a Attempt) censored() bool { return a.Censored || a.Outcome == TimedOut }

// when is the attempt's most meaningful timestamp (end, else start, else queued).
func (a Attempt) when() time.Time {
	switch {
	case !a.End.IsZero():
		return a.End
	case !a.Start.IsZero():
		return a.Start
	}
	return a.Queued
}

// ScopeBucketFor maps a scope to a timing category:
// "all", "0", "1", "2-10", "11-100", "101+".
func ScopeBucketFor(all bool, files int) string {
	switch {
	case all:
		return "all"
	case files <= 0:
		return "0"
	case files == 1:
		return "1"
	case files <= 10:
		return "2-10"
	case files <= 100:
		return "11-100"
	}
	return "101+"
}
