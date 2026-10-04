package lap

import (
	"context"
	"fmt"
	"time"
)

// simUnknown is the duration assumed for tasks without an estimate when
// simulating a plan.
const simUnknown = 10 * time.Second

// plan simulates the policy on a virtual clock using duration estimates.
func (e *engine) plan(nodes []*node) *Plan {
	cutoff := e.budget - e.cfg.Cleanup
	p := &Plan{
		Budget:    e.budget,
		Cutoff:    cutoff,
		CPU:       e.cpu,
		Memory:    e.mem,
		Mode:      e.opt.Mode.String(),
		ScopeDesc: describeScope(e),
	}
	type sim struct {
		n       *node
		state   nodeState
		ok      bool
		end     time.Duration
		cpu     int
		mem     int64
		unknown bool
		entry   PlanEntry
	}
	sims := make([]*sim, len(nodes))
	idx := map[*node]*sim{}
	for i, n := range nodes {
		s := &sim{n: n, entry: PlanEntry{Task: n.task.Name, Phase: n.phase.String(), All: n.inv.All, Files: len(n.inv.Files)}}
		if n.state == done {
			s.state = done
			s.ok = true
			s.entry.Status = "skipped"
			s.entry.Reason = n.attempt.Reason
		}
		sims[i] = s
		idx[n] = s
	}

	var t time.Duration
	freeCPU, freeMem := e.cpu, e.mem
	for guard := 0; guard < 10*len(nodes)+10; guard++ {
		// Blocked propagation.
		for changed := true; changed; {
			changed = false
			for _, s := range sims {
				if s.state != pending {
					continue
				}
				for _, d := range s.n.deps {
					ds := idx[d]
					if ds.state == done && !ds.ok {
						s.state, s.ok = done, false
						s.entry.Status = "blocked"
						s.entry.Reason = "prerequisite " + d.task.Name + " " + ds.entry.Status
						changed = true
						break
					}
				}
			}
		}
		var ready []*node
		pendingN, runningN, unknownN := 0, 0, 0
		for _, s := range sims {
			switch s.state {
			case pending:
				pendingN++
				ok := true
				for _, d := range s.n.deps {
					if idx[d].state != done {
						ok = false
					}
				}
				if ok {
					ready = append(ready, s.n)
				}
			case running:
				runningN++
				if s.unknown {
					unknownN++
				}
			}
		}
		if pendingN == 0 && runningN == 0 {
			break
		}
		admitted := 0
		if len(ready) > 0 {
			snap := Snapshot{Budget: e.budget, Remaining: cutoff - t, FreeCPU: freeCPU, FreeMem: freeMem,
				Allowance: e.cpu, Running: runningN, RunningUnknown: unknownN}
			for _, n := range ready {
				snap.Ready = append(snap.Ready, Candidate{Task: n.task, Options: n.options})
			}
			d := e.cfg.Policy.Decide(context.Background(), snap)
			for _, df := range d.Defer {
				for _, n := range ready {
					if n.task.Name == df.Task && idx[n].state == pending {
						s := idx[n]
						s.state, s.ok = done, false
						s.entry.Status = "deferred"
						s.entry.Reason = df.Reason
					}
				}
			}
			for _, a := range d.Admit {
				for _, n := range ready {
					s := idx[n]
					if n.task.Name != a.Task || s.state != pending || a.Variant < 0 || a.Variant >= len(n.options) {
						continue
					}
					o := n.options[a.Variant]
					if a.CPU > freeCPU || o.Memory > freeMem {
						continue
					}
					dur := o.Estimate.Duration
					s.unknown = dur == 0
					if dur == 0 {
						dur = simUnknown
					}
					s.state = running
					s.end = t + dur
					s.cpu, s.mem = a.CPU, o.Memory
					freeCPU -= a.CPU
					freeMem -= o.Memory
					s.entry.Status = "run"
					s.entry.Variant = o.Name
					s.entry.Coverage = n.task.Variants[a.Variant].Coverage
					s.entry.Narrowed = a.Variant > 0
					s.entry.Estimate = o.Estimate.Duration
					s.entry.Source = o.Estimate.Source
					s.entry.Confidence = o.Estimate.Confidence
					if s.entry.Confidence == "" {
						s.entry.Confidence = "none"
					}
					s.entry.Start = t
					s.entry.End = s.end
					s.entry.CPU = a.CPU
					admitted++
				}
			}
		}
		if admitted > 0 {
			continue
		}
		// Advance to the next completion.
		var next *sim
		for _, s := range sims {
			if s.state == running && (next == nil || s.end < next.end) {
				next = s
			}
		}
		if next == nil {
			// Ready tasks the policy neither admitted nor deferred while the
			// machine is idle: they cannot run in this plan.
			for _, n := range ready {
				s := idx[n]
				s.state, s.ok = done, false
				s.entry.Status = "deferred"
				s.entry.Reason = "policy did not admit it"
			}
			continue
		}
		t = next.end
		for _, s := range sims {
			if s.state == running && s.end <= t {
				s.state, s.ok = done, true
				freeCPU += s.cpu
				freeMem += s.mem
				if s.end > cutoff {
					s.entry.Reason = "may not finish before the cutoff"
				}
			}
		}
	}
	for _, s := range sims {
		if s.entry.Status == "" {
			s.entry.Status = "deferred"
			s.entry.Reason = "not reached"
		}
		p.Entries = append(p.Entries, s.entry)
	}
	return p
}

func describeScope(e *engine) string {
	sc := e.scope
	if sc == nil {
		return ""
	}
	if sc.All {
		return fmt.Sprintf("full scope (%d files)", len(sc.Files))
	}
	base := sc.Base
	if len(base) > 10 {
		base = base[:10]
	}
	return fmt.Sprintf("%d changed files vs %s (merge-base %s)", len(sc.Files), sc.BaseRef, base)
}
