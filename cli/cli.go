// Package cli is a starter command-line front end for a lap.Config. A
// company's main program can be as small as:
//
//	func main() { cli.Main(lap.Config{Name: "dev", Tasks: tasks}) }
//
// It is meant to be copied and edited as much as used: everything here is
// built on the public lap, store and term APIs.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/vivster7/lap"
	"github.com/vivster7/lap/pool"
	"github.com/vivster7/lap/scope"
	"github.com/vivster7/lap/store"
	"github.com/vivster7/lap/term"
)

// Main runs the CLI with os.Args and exits.
func Main(cfg lap.Config) {
	os.Exit(Run(context.Background(), cfg, os.Args[1:], os.Stdout, os.Stderr))
}

// Run executes the CLI and returns the exit code.
func Run(ctx context.Context, cfg lap.Config, args []string, stdout, stderr io.Writer) int {
	if cfg.Name == "" {
		cfg.Name = "dev"
	}
	if len(args) > 0 && !isSelection(cfg, args[0]) {
		switch args[0] {
		case "plan":
			return runCmd(ctx, cfg, append([]string{"--plan"}, args[1:]...), stdout, stderr)
		case "logs":
			return logsCmd(cfg, args[1:], stdout, stderr)
		case "why":
			return whyCmd(cfg, args[1:], stdout, stderr)
		case "stats":
			return statsCmd(cfg, args[1:], stdout, stderr)
		case "tasks":
			return tasksCmd(cfg, stdout)
		case "pool":
			return poolCmd(cfg, stdout, stderr)
		case "help", "-h", "--help":
			usage(cfg, stdout)
			return 0
		}
	}
	return runCmd(ctx, cfg, args, stdout, stderr)
}

func isSelection(cfg lap.Config, s string) bool {
	for _, t := range cfg.Tasks {
		if t.Name == s {
			return true
		}
		for _, g := range t.Groups {
			if g == s {
				return true
			}
		}
	}
	return false
}

func usage(cfg lap.Config, w io.Writer) {
	n := cfg.Name
	fmt.Fprintf(w, `%[1]s: run what can be verified locally within a time budget.

usage:
  %[1]s [group|task ...] [flags]   run (default: everything applicable)
  %[1]s plan [group|task ...]      show what would run, and why
  %[1]s logs [task] [--raw]        output of the last attempt of a task
  %[1]s why [run-id]               decisions and outcomes of a run
  %[1]s stats [task]               timing and outcome history
  %[1]s tasks                      list tasks and groups
  %[1]s pool                       machine-wide resource holders

run flags:
`, n)
	fs, _ := runFlags(cfg)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fmt.Fprintf(w, "\nexit: 0 no findings (coverage may be partial), 1 findings, 3 tool/config error, 130 interrupted\n")
}

type runOpts struct {
	timeout time.Duration
	check   bool
	all     bool
	base    string
	plan    bool
	json    bool
	autofix bool
	jobs    int
	verbose bool
	color   string
	root    string
}

func runFlags(cfg lap.Config) (*flag.FlagSet, *runOpts) {
	o := &runOpts{}
	budget := cfg.Budget
	if budget == 0 {
		budget = 60 * time.Second
	}
	fs := flag.NewFlagSet(cfg.Name, flag.ContinueOnError)
	fs.DurationVar(&o.timeout, "timeout", budget, "total time budget")
	fs.BoolVar(&o.check, "check", false, "read-only: writers report instead of rewriting files")
	fs.BoolVar(&o.all, "all", false, "full scope instead of changed files")
	fs.StringVar(&o.base, "base", "", "base revision for change selection (default: merge-base with the default branch)")
	fs.BoolVar(&o.plan, "plan", false, "print the plan and exit")
	fs.BoolVar(&o.json, "json", false, "print the report as JSON")
	fs.BoolVar(&o.autofix, "autofix", false, "apply rule fixes in one remediation pass")
	fs.IntVar(&o.jobs, "jobs", 0, "CPU slots for this run (default: machine pool size)")
	fs.BoolVar(&o.verbose, "v", false, "verbose: show the plan and skipped tasks")
	fs.StringVar(&o.color, "color", "auto", "auto, always or never")
	fs.StringVar(&o.root, "C", ".", "run as if started in this directory")
	return fs, o
}

func runCmd(ctx context.Context, cfg lap.Config, args []string, stdout, stderr io.Writer) int {
	fs, o := runFlags(cfg)
	fs.SetOutput(stderr)
	sel, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(cfg, stdout)
			return 0
		}
		return 3
	}
	for _, s := range sel {
		if !isSelection(cfg, s) {
			fmt.Fprintf(stderr, "%s: unknown task or group %q (see `%s tasks`)\n", cfg.Name, s, cfg.Name)
			return 3
		}
	}
	if o.jobs > 0 {
		cfg.CPU = o.jobs
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	opt := lap.RunOptions{
		Root:     o.root,
		Base:     o.base,
		All:      o.all,
		Select:   sel,
		Budget:   o.timeout,
		PlanOnly: o.plan,
		Autofix:  o.autofix,
		Argv:     append([]string{cfg.Name}, args...),
	}
	if o.check {
		opt.Mode = lap.CheckOnly
	}
	var con *console
	if !o.json {
		con = newConsole(cfg, stdout, colorEnabled(o.color, stdout), o.verbose || o.plan)
		opt.Reporter = con
	}
	rep, err := lap.Run(ctx, cfg, opt)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		if errors.Is(err, lap.ErrBusy) {
			return 3
		}
		return 3
	}
	if o.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(jsonReport(rep))
	}
	if o.plan {
		return 0
	}
	return rep.ExitCode
}

// parseInterleaved allows flags after positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func colorEnabled(mode string, w io.Writer) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTerminal(w)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

type jsonAttempt struct {
	store.Attempt
	DurationMS int64         `json:"duration_ms"`
	Findings   []lap.Finding `json:"findings_list,omitempty"`
}

func jsonReport(r *lap.Report) any {
	var atts []jsonAttempt
	for _, a := range r.Attempts {
		atts = append(atts, jsonAttempt{Attempt: a, DurationMS: a.Duration().Milliseconds(), Findings: r.Findings[a.Task]})
	}
	return map[string]any{
		"run_id":      r.RunID,
		"run_dir":     r.RunDir,
		"root":        r.Root,
		"mode":        r.Mode.String(),
		"budget_ms":   r.Budget.Milliseconds(),
		"elapsed_ms":  r.Elapsed.Milliseconds(),
		"complete":    r.Complete(),
		"exit_code":   r.ExitCode,
		"scope":       r.Scope,
		"plan":        r.Plan,
		"attempts":    atts,
		"suggestions": r.Suggestions,
		"notes":       r.Notes,
	}
}

func openStore(cfg lap.Config, stderr io.Writer) (*store.Store, bool) {
	root, err := scope.Toplevel(".")
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		return nil, false
	}
	st, err := store.Open(root, cfg.Store)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		return nil, false
	}
	return st, true
}

func logsCmd(cfg lap.Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	raw := fs.Bool("raw", false, "replay the raw bytes (colors, as the tool wrote them)")
	rendered := fs.Bool("rendered", false, "final terminal screen with scrollback")
	runID := fs.String("run", "", "run id (default: latest attempt of the task)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return 3
	}
	st, ok := openStore(cfg, stderr)
	if !ok {
		return 3
	}
	if len(pos) == 0 {
		rec, atts, err := st.LoadRun(*runID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: no runs recorded yet\n", cfg.Name)
			return 3
		}
		fmt.Fprintf(stdout, "run %s (%s)\n", rec.ID, rec.Status)
		for _, a := range atts {
			if a.Start.IsZero() {
				continue
			}
			fmt.Fprintf(stdout, "  %-24s %-10s %6s\n", a.Task, a.Outcome, a.Duration().Round(100*time.Millisecond))
		}
		fmt.Fprintf(stdout, "\n%s logs <task> to see output\n", cfg.Name)
		return 0
	}
	task := pos[0]
	var a store.Attempt
	var runDir string
	if *runID != "" {
		rec, atts, err := st.LoadRun(*runID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
			return 3
		}
		runDir = rec.Dir
		for _, x := range atts {
			if x.Task == task && !x.Start.IsZero() {
				a = x
			}
		}
	} else {
		a, runDir, err = st.LastAttempt(task)
	}
	if err != nil || a.CaptureDir == "" {
		fmt.Fprintf(stderr, "%s: no captured output for %q\n", cfg.Name, task)
		return 3
	}
	c, err := term.OpenCapture(joinPath(runDir, a.CaptureDir))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		return 3
	}
	fmt.Fprintf(stderr, "# %s %s · %s · exit %d · %s · run %s\n", a.Task, a.Variant, a.Outcome, a.ExitCode,
		a.Duration().Round(100*time.Millisecond), a.RunID)
	switch {
	case *raw:
		f, err := c.Raw()
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
			return 3
		}
		defer f.Close()
		_, _ = io.Copy(stdout, f)
	case *rendered:
		if _, err := c.Rendered(stdout, term.RenderOptions{}); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
			return 3
		}
	default:
		if err := c.Plain(stdout, term.PlainOptions{}); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
			return 3
		}
	}
	if s, ok := c.Summary(); ok && !s.Complete {
		fmt.Fprintf(stderr, "# capture incomplete: %v\n", s.Problems)
	}
	return 0
}

func whyCmd(cfg lap.Config, args []string, stdout, stderr io.Writer) int {
	st, ok := openStore(cfg, stderr)
	if !ok {
		return 3
	}
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	rec, atts, err := st.LoadRun(id)
	if err != nil {
		fmt.Fprintf(stderr, "%s: no runs recorded yet\n", cfg.Name)
		return 3
	}
	fmt.Fprintf(stdout, "run %s · %s · mode %s · budget %s", rec.ID, rec.Status, rec.Mode, rec.Budget)
	if rec.Summary != nil {
		fmt.Fprintf(stdout, " · took %s · exit %d", rec.Summary.Elapsed.Round(100*time.Millisecond), rec.Summary.ExitCode)
	}
	fmt.Fprintln(stdout)
	if rec.ScopeAll {
		fmt.Fprintf(stdout, "scope: full (%d files)\n", rec.ScopeFiles)
	} else {
		fmt.Fprintf(stdout, "scope: %d changed files vs %s\n", rec.ScopeFiles, rec.BaseRef)
	}
	var plan lap.Plan
	if b, err := os.ReadFile(joinPath(rec.Dir, "plan.json")); err == nil && json.Unmarshal(b, &plan) == nil {
		fmt.Fprintln(stdout, "\nplan:")
		printPlan(stdout, &plan, palette{})
	}
	fmt.Fprintln(stdout, "\noutcomes:")
	sort.SliceStable(atts, func(i, j int) bool { return atts[i].AttemptID < atts[j].AttemptID })
	for _, a := range atts {
		d := ""
		if !a.Start.IsZero() {
			d = a.Duration().Round(100 * time.Millisecond).String()
		}
		fmt.Fprintf(stdout, "  %-24s %-12s %-10s %8s  %s\n", a.Task, a.Variant, a.Outcome, d, a.Reason)
	}
	if rec.Summary != nil && len(rec.Summary.Notes) > 0 {
		fmt.Fprintln(stdout, "\nnotes:")
		for _, n := range rec.Summary.Notes {
			fmt.Fprintf(stdout, "  %s\n", n)
		}
	}
	return 0
}

func statsCmd(cfg lap.Config, args []string, stdout, stderr io.Writer) int {
	st, ok := openStore(cfg, stderr)
	if !ok {
		return 3
	}
	q := store.Query{}
	if len(args) > 0 {
		q.Task = args[0]
	}
	stats, err := st.Stats(q)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		return 3
	}
	if len(stats) == 0 {
		fmt.Fprintf(stdout, "no history yet\n")
		return 0
	}
	fmt.Fprintf(stdout, "%-24s %-10s %5s %8s %8s  %s\n", "task", "variant", "runs", "p50", "p90", "outcomes")
	dur := func(d time.Duration) string {
		if d == 0 {
			return "-"
		}
		return d.Round(100 * time.Millisecond).String()
	}
	for _, s := range stats {
		if s.Counts[store.Skipped] == s.Attempts {
			continue // never applicable so far
		}
		var parts []string
		for _, o := range []store.Outcome{store.Passed, store.Fixed, store.Findings, store.ToolError, store.TimedOut, store.Deferred, store.Blocked, store.Stale} {
			if c := s.Counts[o]; c > 0 {
				parts = append(parts, fmt.Sprintf("%s %d%%", o, c*100/max(1, s.Attempts)))
			}
		}
		fmt.Fprintf(stdout, "%-24s %-10s %5d %8s %8s  %s\n", s.Task, s.Variant, s.Attempts,
			dur(s.P50), dur(s.P90), strings.Join(parts, ", "))
	}
	return 0
}

func tasksCmd(cfg lap.Config, stdout io.Writer) int {
	fmt.Fprintf(stdout, "%-24s %-8s %-24s %s\n", "task", "phase", "groups", "variants")
	for _, t := range cfg.Tasks {
		var vs []string
		for _, v := range t.Variants {
			name := v.Name
			if name == "" {
				name = "default"
			}
			vs = append(vs, name)
		}
		fmt.Fprintf(stdout, "%-24s %-8s %-24s %s\n", t.Name, t.Phase, strings.Join(t.Groups, ","), strings.Join(vs, " > "))
	}
	return 0
}

func poolCmd(cfg lap.Config, stdout, stderr io.Writer) int {
	p, err := pool.Open(cfg.Pool)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		return 3
	}
	cpu, mem := p.Size()
	fcpu, fmem := p.Free()
	fmt.Fprintf(stdout, "pool: %d/%d cpu free, %s/%s memory free\n", fcpu, cpu, bytesStr(fmem), bytesStr(mem))
	hs, err := p.Holders()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cfg.Name, err)
		return 3
	}
	for _, h := range hs {
		fmt.Fprintf(stdout, "  %-8s pid %-7d %-20s %s (since %s)\n", h.Token, h.Holder.PID, h.Holder.Task, h.Holder.Worktree,
			time.Since(h.Since).Round(time.Second))
	}
	return 0
}

func bytesStr(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	}
	return fmt.Sprintf("%dB", n)
}

func joinPath(dir, rel string) string {
	if strings.HasPrefix(rel, "/") {
		return rel
	}
	return dir + "/" + rel
}
