package lap

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/vivster7/lap/store"
)

// Option is one admissible way to run a ready task: a variant with its
// estimate and resource range.
type Option struct {
	Variant  int // index into Task.Variants
	Name     string
	Estimate store.Estimate
	// MinCPU..MaxCPU slots; Memory bytes.
	MinCPU, MaxCPU int
	Memory         int64
}

// Candidate is a ready task (prerequisites satisfied, not yet started).
type Candidate struct {
	Task    *Task
	Options []Option // broadest first
	// Waited is how long the task has been ready.
	Waited time.Duration
}

// Snapshot is the scheduler state handed to a Policy.
type Snapshot struct {
	Budget    time.Duration // whole-run budget
	Remaining time.Duration // until the work cutoff
	FreeCPU   int           // slots free in this run's allowance
	FreeMem   int64         // bytes free in this run's allowance
	Allowance int           // total CPU slots for this run
	Running   int           // tasks currently running
	// RunningUnknown counts running tasks with no duration estimate.
	RunningUnknown int
	Ready          []Candidate
}

// Admission starts a task with a variant and a CPU grant.
type Admission struct {
	Task    string
	Variant int
	CPU     int
}

// Deferral removes a ready task from this run with a reason.
type Deferral struct {
	Task   string
	Reason string
}

// Decision is what a Policy returns. Tasks neither admitted nor deferred stay
// ready and are offered again when something changes.
type Decision struct {
	Admit []Admission
	Defer []Deferral
}

// Policy decides which ready tasks to start. Policies are pure: they never
// launch processes. The engine validates every admission against the
// dependency graph, the deadline and the free resources, and may still fail
// to start a task when the machine-wide pool is busy.
type Policy interface {
	Decide(ctx context.Context, s Snapshot) Decision
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, s Snapshot) Decision

func (f PolicyFunc) Decide(ctx context.Context, s Snapshot) Decision { return f(ctx, s) }

// DefaultPolicy is a greedy heuristic: by priority, then shortest expected
// work first; for each task, the broadest variant whose estimate fits the
// remaining time and whose resources are free. It is not an optimal packer.
type DefaultPolicy struct {
	// UnknownFraction: a task with no estimate is admitted only while at
	// least this fraction of the budget remains (default 0.25), and at most
	// MaxUnknown such tasks run at once (default 2).
	UnknownFraction float64
	MaxUnknown      int
}

func (p DefaultPolicy) Decide(_ context.Context, s Snapshot) Decision {
	frac := p.UnknownFraction
	if frac == 0 {
		frac = 0.25
	}
	maxUnknown := p.MaxUnknown
	if maxUnknown == 0 {
		maxUnknown = 2
	}

	ready := append([]Candidate(nil), s.Ready...)
	sort.SliceStable(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		if a.Task.Priority != b.Task.Priority {
			return a.Task.Priority > b.Task.Priority
		}
		da, db := shortest(a), shortest(b)
		if (da == 0) != (db == 0) {
			return da != 0 // known estimates before unknown
		}
		if da != db {
			return da < db
		}
		return a.Task.Name < b.Task.Name
	})

	var d Decision
	freeCPU, freeMem := s.FreeCPU, s.FreeMem
	// Tasks that can use many cores get a fair share of what is free, so one
	// AllCPU task does not starve the others: free / (ready tasks), at least
	// the task's minimum.
	share := freeCPU
	if len(ready) > 1 {
		share = (freeCPU + len(ready) - 1) / len(ready)
	}
	unknown := s.RunningUnknown
	for _, c := range ready {
		fitsTime := false
		admitted := false
		var waitReason string
		for _, o := range c.Options {
			est := o.Estimate.Duration
			if est == 0 {
				if s.Remaining < time.Duration(float64(s.Budget)*frac) {
					continue
				}
			} else if est > s.Remaining {
				continue
			}
			fitsTime = true
			if est == 0 && unknown >= maxUnknown {
				waitReason = "unknown"
				continue
			}
			if o.MinCPU > freeCPU || o.Memory > freeMem {
				waitReason = "resources"
				continue
			}
			cpu := o.MaxCPU
			if cpu > share && share >= o.MinCPU {
				cpu = share
			}
			if cpu < o.MinCPU {
				cpu = o.MinCPU
			}
			if cpu > freeCPU {
				cpu = freeCPU
			}
			d.Admit = append(d.Admit, Admission{Task: c.Task.Name, Variant: o.Variant, CPU: cpu})
			freeCPU -= cpu
			freeMem -= o.Memory
			if est == 0 {
				unknown++
			}
			admitted = true
			break
		}
		if admitted || waitReason != "" {
			continue
		}
		if !fitsTime {
			d.Defer = append(d.Defer, Deferral{Task: c.Task.Name, Reason: deferReason(c, s)})
		}
	}
	return d
}

func shortest(c Candidate) time.Duration {
	var best time.Duration
	for _, o := range c.Options {
		if d := o.Estimate.Duration; d > 0 && (best == 0 || d < best) {
			best = d
		}
	}
	return best
}

func deferReason(c Candidate, s Snapshot) string {
	if len(c.Options) == 0 {
		return "no runnable variant"
	}
	o := c.Options[len(c.Options)-1]
	if o.Estimate.Duration == 0 {
		return fmt.Sprintf("no estimate and only %s left", s.Remaining.Round(time.Second))
	}
	return fmt.Sprintf("%s needs ~%s (%s), %s left", o.Name, o.Estimate.Duration.Round(100*time.Millisecond),
		o.Estimate.Source, s.Remaining.Round(100*time.Millisecond))
}
