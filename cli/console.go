package cli

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/store"
	"github.com/vivster7/lap/term"
)

type palette struct{ on bool }

func (p palette) c(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (p palette) dim(s string) string    { return p.c("2", s) }
func (p palette) bold(s string) string   { return p.c("1", s) }
func (p palette) red(s string) string    { return p.c("31", s) }
func (p palette) green(s string) string  { return p.c("32", s) }
func (p palette) yellow(s string) string { return p.c("33", s) }
func (p palette) cyan(s string) string   { return p.c("36", s) }

// console is a line-oriented Reporter: it works the same in a terminal, a
// CI log or an agent's tool output.
type console struct {
	mu      sync.Mutex
	w       io.Writer
	p       palette
	cfg     lap.Config
	verbose bool
	// TailLines of failing output to print at the end.
	TailLines int
}

func newConsole(cfg lap.Config, w io.Writer, color, verbose bool) *console {
	return &console{w: w, p: palette{on: color}, cfg: cfg, verbose: verbose, TailLines: 25}
}

func (c *console) Report(e lap.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.p
	switch e.Kind {
	case lap.PlanReady:
		pl := e.Plan
		counts := map[string]int{}
		for _, en := range pl.Entries {
			counts[en.Status]++
		}
		fmt.Fprintf(c.w, "%s %s · budget %s · %s mode · %d cpu · %s\n", p.bold(c.cfg.Name), p.dim("·"),
			pl.Budget, pl.Mode, pl.CPU, pl.ScopeDesc)
		if c.verbose {
			printPlan(c.w, pl, p)
		} else {
			fmt.Fprintf(c.w, "%s\n", p.dim(fmt.Sprintf("plan: %d to run, %d deferred, %d skipped (%s plan for details)",
				counts["run"], counts["deferred"], counts["skipped"], c.cfg.Name)))
		}
	case lap.Note:
		fmt.Fprintf(c.w, "  %s %s\n", p.yellow("!"), e.Text)
	case lap.TaskStarted:
		a := e.Attempt
		scope := fmt.Sprintf("%d files", a.ScopeFiles)
		if a.ScopeAll {
			scope = "all files"
		}
		fmt.Fprintf(c.w, "  %s %s %s\n", p.dim("▸"), a.Task, p.dim(fmt.Sprintf("[%s · %s · %d cpu]", a.Variant, scope, a.Workers)))
	case lap.TaskFinished:
		c.finished(e)
	case lap.RunFinished:
		c.summary(e.Report)
	}
}

func (c *console) finished(e lap.Event) {
	p := c.p
	a := e.Attempt
	d := a.Duration().Round(100 * time.Millisecond)
	extra := ""
	if a.Reason != "" {
		extra = " " + p.dim(a.Reason)
	}
	switch a.Outcome {
	case store.Passed:
		fmt.Fprintf(c.w, "  %s %s %s%s\n", p.green("✓"), a.Task, p.dim(d.String()), extra)
	case store.Fixed:
		fmt.Fprintf(c.w, "  %s %s %s %s\n", p.green("✎"), a.Task, p.dim(d.String()),
			p.cyan(fmt.Sprintf("fixed %d file(s)", len(a.ChangedFiles))))
	case store.Findings:
		fmt.Fprintf(c.w, "  %s %s %s %s%s\n", p.red("✗"), a.Task, p.dim(d.String()), p.red(plural(a.Findings, "finding")), extra)
	case store.ToolError:
		fmt.Fprintf(c.w, "  %s %s %s %s%s\n", p.red("!"), a.Task, p.dim(d.String()), p.red("error"), extra)
	case store.TimedOut, store.Interrupted:
		fmt.Fprintf(c.w, "  %s %s %s %s%s\n", p.yellow("⏱"), a.Task, p.dim(d.String()), p.yellow(string(a.Outcome)), extra)
	case store.Deferred, store.Blocked:
		fmt.Fprintf(c.w, "  %s %s %s%s\n", p.yellow("–"), a.Task, p.yellow(string(a.Outcome)), extra)
	case store.Stale:
		fmt.Fprintf(c.w, "  %s %s %s%s\n", p.yellow("~"), a.Task, p.yellow("stale"), extra)
	case store.Skipped:
		if c.verbose {
			fmt.Fprintf(c.w, "  %s %s %s%s\n", p.dim("·"), p.dim(a.Task), p.dim("skipped"), extra)
		}
	}
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func (c *console) summary(r *lap.Report) {
	p := c.p
	latest := r.Latest()
	// Output tails of failing tasks.
	for _, a := range latest {
		if a.Outcome != store.Findings && a.Outcome != store.ToolError {
			continue
		}
		tail := c.tail(r.RunDir, a)
		if tail == "" {
			continue
		}
		fmt.Fprintf(c.w, "\n%s\n", p.bold(fmt.Sprintf("── %s (%s, exit %d) ──", a.Task, a.Outcome, a.ExitCode)))
		fmt.Fprint(c.w, tail)
	}

	counts := map[store.Outcome]int{}
	for _, a := range latest {
		counts[a.Outcome]++
	}
	var parts []string
	add := func(o store.Outcome, color func(string) string) {
		if n := counts[o]; n > 0 {
			parts = append(parts, color(fmt.Sprintf("%d %s", n, o)))
		}
	}
	add(store.Passed, p.green)
	add(store.Fixed, p.cyan)
	add(store.Findings, p.red)
	add(store.ToolError, p.red)
	add(store.TimedOut, p.yellow)
	add(store.Interrupted, p.yellow)
	add(store.Stale, p.yellow)
	add(store.Deferred, p.yellow)
	add(store.Blocked, p.yellow)
	fmt.Fprintf(c.w, "\n%s in %s: %s\n", p.bold(c.cfg.Name), r.Elapsed.Round(100*time.Millisecond), strings.Join(parts, ", "))

	var unverified []string
	for _, a := range latest {
		switch a.Outcome {
		case store.Deferred, store.Blocked, store.TimedOut, store.Interrupted, store.Stale:
			unverified = append(unverified, a.Task)
		default:
			if strings.HasPrefix(a.Reason, "narrowed") {
				unverified = append(unverified, a.Task+" (narrowed to "+a.Variant+")")
			}
		}
	}
	if len(unverified) > 0 {
		fmt.Fprintf(c.w, "%s %s\n", p.yellow("not verified locally:"), strings.Join(unverified, ", "))
	}
	for _, s := range r.Suggestions {
		fmt.Fprintf(c.w, "%s %s\n", p.cyan("hint:"), s)
	}
	if r.RunID != "" {
		fmt.Fprintf(c.w, "%s\n", p.dim(fmt.Sprintf("run %s · %s logs <task> · %s why", r.RunID, c.cfg.Name, c.cfg.Name)))
	}
}

func (c *console) tail(runDir string, a store.Attempt) string {
	if runDir == "" || a.CaptureDir == "" {
		return ""
	}
	capt, err := term.OpenCapture(filepath.Join(runDir, a.CaptureDir))
	if err != nil {
		return ""
	}
	var b bytes.Buffer
	if err := capt.Plain(&b, term.PlainOptions{}); err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	skipped := 0
	if len(lines) > c.TailLines {
		skipped = len(lines) - c.TailLines
		lines = lines[skipped:]
	}
	var out strings.Builder
	if skipped > 0 {
		fmt.Fprintf(&out, "%s\n", c.p.dim(fmt.Sprintf("… %d earlier lines (%s logs %s)", skipped, c.cfg.Name, a.Task)))
	}
	for _, l := range lines {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return out.String()
}

func printPlan(w io.Writer, pl *lap.Plan, p palette) {
	for _, en := range pl.Entries {
		est := ""
		if en.Status == "run" {
			if en.Estimate > 0 {
				est = fmt.Sprintf("~%s (%s)", en.Estimate.Round(100*time.Millisecond), en.Source)
			} else {
				est = "no estimate"
			}
		}
		variant := en.Variant
		if en.Narrowed {
			variant += " (narrowed)"
		}
		status := en.Status
		switch status {
		case "run":
			status = p.green(fmt.Sprintf("%-8s", status))
		case "deferred", "blocked":
			status = p.yellow(fmt.Sprintf("%-8s", status))
		default:
			status = p.dim(fmt.Sprintf("%-8s", status))
		}
		line := fmt.Sprintf("  %s %-24s %-20s %s", status, en.Task, variant, est)
		if en.Reason != "" {
			line += "  " + p.dim(en.Reason)
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	for _, n := range pl.Notes {
		fmt.Fprintf(w, "  %s\n", n)
	}
}
