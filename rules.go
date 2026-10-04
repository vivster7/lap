package lap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vivster7/lap/store"
)

type ruleMatch struct {
	rule *Rule
	node *node
}

// remediate evaluates rules against failed tasks. Matches always produce
// suggestions; with Autofix, rule fixes run in one remediation pass (writers,
// sequentially) and each matched task is re-run once if time remains. Old
// runs never trigger fixes: only findings from this run in this worktree.
func (e *engine) remediate(ctx context.Context, nodes []*node) {
	var matches []ruleMatch
	seenSuggest := map[string]bool{}
	suggest := func(s string) {
		if s != "" && !seenSuggest[s] {
			seenSuggest[s] = true
			e.report.Suggestions = append(e.report.Suggestions, s)
		}
	}
	for _, n := range nodes {
		if n.attempt.Outcome != store.Findings {
			continue
		}
		if n.task.Suggest != "" {
			suggest(n.task.Name + ": " + n.task.Suggest)
		}
		if !n.attempt.CaptureComplete {
			continue // incomplete diagnostics never trigger rules
		}
		for i := range e.cfg.Rules {
			r := &e.cfg.Rules[i]
			if r.Task != "" && r.Task != n.task.Name {
				continue
			}
			if e.ruleMatches(r, n) {
				matches = append(matches, ruleMatch{rule: r, node: n})
				if r.Suggest != "" {
					suggest(fmt.Sprintf("%s: %s", n.task.Name, r.Suggest))
				}
			}
		}
	}
	if !e.opt.Autofix || e.opt.Mode == CheckOnly || len(matches) == 0 {
		return
	}
	if time.Until(e.cutoff) <= 0 {
		e.note("autofix: no time left for a remediation pass")
		return
	}

	var fixes, reruns []*node
	fixByName := map[string]*node{}
	rerunByName := map[string]bool{}
	for _, m := range matches {
		if m.rule.Fix == nil {
			continue
		}
		f := fixByName[m.rule.Fix.Name]
		if f == nil {
			f = e.newNode(m.rule.Fix, Prepare, true)
			for _, prev := range fixes { // fixes run one at a time, in rule order
				f.deps = append(f.deps, prev)
			}
			fixByName[m.rule.Fix.Name] = f
			fixes = append(fixes, f)
		}
		if !rerunByName[m.node.task.Name] {
			rerunByName[m.node.task.Name] = true
			r := e.newNode(m.node.task, Check, false)
			r.inv = m.node.inv
			r.scopeN = m.node.scopeN
			r.options = e.options(r)
			reruns = append(reruns, r)
		}
	}
	if len(fixes) == 0 {
		return
	}
	for _, r := range reruns {
		r.deps = append(r.deps, fixes...)
	}
	e.note("autofix: running %d fix(es), then re-checking %s", len(fixes), taskNames(reruns))
	all := append(fixes, reruns...)
	e.execute(ctx, all)
	for _, r := range reruns {
		if r.attempt.Outcome != store.Passed {
			if r.attempt.Outcome == store.Findings {
				continue
			}
			e.note("%s: fix applied; verification incomplete (%s)", r.task.Name, r.attempt.Outcome)
		}
	}
}

func (e *engine) newNode(t *Task, phase Phase, writer bool) *node {
	n := &node{task: t, phase: phase, writer: writer, label: "remediation"}
	n.inv = Invocation{Root: e.root, Mode: e.opt.Mode, Scope: e.scope}
	_, all, files, reason := e.applicability(t)
	n.inv.All, n.inv.Files, n.reason = all, files, reason
	n.scopeN = len(files)
	n.options = e.options(n)
	return n
}

func (e *engine) ruleMatches(r *Rule, n *node) bool {
	if r.Match == nil && r.Code == "" {
		return true
	}
	if r.Code != "" {
		for _, f := range n.finds {
			if f.Code == r.Code {
				return true
			}
		}
	}
	if r.Match != nil && e.run != nil {
		c := &Classification{captureDir: e.run.Dir() + "/" + n.attempt.CaptureDir}
		for _, line := range strings.Split(c.Plain(), "\n") {
			if r.Match.MatchString(line) {
				return true
			}
		}
	}
	return false
}

func taskNames(ns []*node) string {
	var s []string
	for _, n := range ns {
		s = append(s, n.task.Name)
	}
	return strings.Join(s, ", ")
}
