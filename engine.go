package lap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vivster7/lap/pool"
	"github.com/vivster7/lap/run"
	"github.com/vivster7/lap/scope"
	"github.com/vivster7/lap/store"
	"github.com/vivster7/lap/term"
)

// Config is the company-owned definition of a verification command.
type Config struct {
	Name  string // command name for messages, e.g. "dev"
	Tasks []Task
	Rules []Rule

	// Budget is the default total time budget (default 60s).
	Budget time.Duration
	// Cleanup is reserved at the end of the budget for terminating work,
	// draining output and reporting (default 3s).
	Cleanup time.Duration
	// Policy chooses what to start (default DefaultPolicy{}).
	Policy Policy

	// CPU and Memory cap this run's allowance (0: the machine-wide pool size).
	CPU    int
	Memory int64
	// Nice lowers task priority with nice(1) (default 10; negative disables).
	Nice int

	Pool  pool.Config
	Store store.Options
	// NoPool disables machine-wide coordination (tests, CI containers).
	NoPool bool
	// Env is added to every task's environment.
	Env []string
	// Cols and Rows size task terminals (default 160x50).
	Cols, Rows uint16
}

// RunOptions are per-invocation choices, typically from flags.
type RunOptions struct {
	Root   string // any directory inside the repository (default ".")
	Base   string // base revision for change selection ("" = auto)
	All    bool   // full scope
	Mode   Mode
	Select []string // group or task names (empty = everything)
	Budget time.Duration
	// PlanOnly computes the plan without running anything.
	PlanOnly bool
	// Autofix applies matching rule fixes in one remediation pass.
	Autofix  bool
	Reporter Reporter
	Argv     []string
}

// ErrBusy is returned when another run holds this worktree's lock.
var ErrBusy = errors.New("lap: another run is active in this worktree")

// Rule maps a task's failure to a suggestion or a fix.
type Rule struct {
	Name string
	// Task whose findings this rule inspects ("" = any task).
	Task string
	// Match is applied to each output line (plain text); Code matches a
	// Finding code. Either may be set; both empty matches any failure.
	Match *regexp.Regexp
	Code  string
	// Suggest is shown when the rule matches.
	Suggest string
	// Fix, when set, is run in the remediation pass under --autofix, after
	// which the matched task is re-run once if time remains.
	Fix *Task
}

type node struct {
	task    *Task
	phase   Phase
	inv     Invocation
	deps    []*node
	writer  bool // may modify the workspace
	options []Option
	scopeN  int
	reason  string // applicability note
	label   string // phase label override ("remediation")

	state   nodeState
	attempt store.Attempt
	readyAt time.Time
	lease   *pool.Lease
	before  map[string]string // dirty snapshot before a writer starts
	unknown bool
	cpu     int
	mem     int64
	finds   []Finding
}

type nodeState int

const (
	pending nodeState = iota
	running
	done
)

func (n *node) succeeded() bool {
	switch n.attempt.Outcome {
	case store.Passed, store.Fixed, store.Findings, store.Skipped:
		return true
	}
	return false
}

type engine struct {
	cfg      Config
	opt      RunOptions
	rep      Reporter
	start    time.Time
	deadline time.Time
	cutoff   time.Time
	budget   time.Duration
	root     string
	scope    *scope.Scope
	store    *store.Store
	run      *store.Run
	pool     *pool.Pool
	cpu      int
	mem      int64
	freeCPU  int
	freeMem  int64
	host     string
	seq      int
	parent   context.Context
	report   *Report
	gitDir   string

	lockCloser *pool.Lock
	snap       map[string]string // cached dirty snapshot; nil when invalid
}

// Run executes cfg under opt and returns the report. A non-nil error means
// the run could not happen (bad configuration, busy worktree, no repository);
// task failures are in the Report.
func Run(ctx context.Context, cfg Config, opt RunOptions) (*Report, error) {
	e := &engine{cfg: cfg, opt: opt, parent: ctx, start: time.Now()}
	e.defaults()
	e.deadline = e.start.Add(e.budget)
	e.cutoff = e.deadline.Add(-e.cfg.Cleanup)

	nodes, err := e.setup(ctx)
	if err != nil {
		return nil, err
	}
	if e.run != nil {
		defer e.run.Abandon("engine exited without finishing") //nolint:errcheck // no-op after Finish
	}
	plan := e.plan(nodes)
	e.report.Plan = plan
	e.emit(Event{Kind: PlanReady, Plan: plan})
	if opt.PlanOnly {
		return e.report, nil
	}
	if e.run != nil {
		_ = e.run.SavePlan(plan)
	}
	for _, n := range nodes {
		if n.state == done { // not applicable
			e.record(n)
		}
	}

	e.execute(ctx, nodes)
	e.checkFreshness(nodes)
	e.remediate(ctx, nodes)
	return e.finish(), nil
}

func (e *engine) defaults() {
	if e.cfg.Name == "" {
		e.cfg.Name = "dev"
	}
	if e.cfg.Budget == 0 {
		e.cfg.Budget = 60 * time.Second
	}
	if e.cfg.Cleanup == 0 {
		e.cfg.Cleanup = 3 * time.Second
	}
	if e.cfg.Policy == nil {
		e.cfg.Policy = DefaultPolicy{}
	}
	if e.cfg.Nice == 0 {
		e.cfg.Nice = 10
	}
	if e.cfg.Cols == 0 {
		e.cfg.Cols = 160
	}
	if e.cfg.Rows == 0 {
		e.cfg.Rows = 50
	}
	e.budget = e.opt.Budget
	if e.budget == 0 {
		e.budget = e.cfg.Budget
	}
	if e.cfg.Cleanup > e.budget/2 {
		e.cfg.Cleanup = e.budget / 2
	}
	e.rep = e.opt.Reporter
	if e.rep == nil {
		e.rep = ReporterFunc(func(Event) {})
	}
	e.host, _ = os.Hostname()
}

func (e *engine) emit(ev Event) {
	ev.Time = time.Now()
	ev.Elapsed = ev.Time.Sub(e.start)
	e.rep.Report(ev)
}

func (e *engine) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e.report.Notes = append(e.report.Notes, msg)
	e.emit(Event{Kind: Note, Text: msg})
}

// setup validates the configuration, resolves scope, opens the store and
// pool, and builds the task graph.
func (e *engine) setup(ctx context.Context) ([]*node, error) {
	byName := map[string]*Task{}
	for i := range e.cfg.Tasks {
		t := &e.cfg.Tasks[i]
		if err := t.validate(); err != nil {
			return nil, err
		}
		if byName[t.Name] != nil {
			return nil, fmt.Errorf("lap: duplicate task %q", t.Name)
		}
		byName[t.Name] = t
	}
	for _, t := range byName {
		for _, d := range t.After {
			if byName[d] == nil {
				return nil, fmt.Errorf("lap: task %q depends on unknown task %q", t.Name, d)
			}
		}
	}
	if err := checkCycles(e.cfg.Tasks); err != nil {
		return nil, err
	}

	rootDir := e.opt.Root
	if rootDir == "" {
		rootDir = "."
	}
	root, err := scope.Toplevel(rootDir)
	if err != nil {
		return nil, fmt.Errorf("lap: %w", err)
	}
	e.root = root
	e.report = &Report{Root: root, Mode: e.opt.Mode, Budget: e.budget, Findings: map[string][]Finding{}}
	e.emit(Event{Kind: RunStarted})

	sc, err := scope.Resolve(ctx, scope.Options{Root: root, Base: e.opt.Base, All: e.opt.All})
	if errors.Is(err, scope.ErrNoBase) {
		e.note("no base revision found; using full scope (pass --base to choose one)")
		sc, err = scope.Resolve(ctx, scope.Options{Root: root, All: true})
	}
	if err != nil {
		return nil, fmt.Errorf("lap: resolve scope: %w", err)
	}
	e.scope = sc
	e.report.Scope = sc

	gitDir, _, err := scope.GitDirs(root)
	if err != nil {
		return nil, fmt.Errorf("lap: %w", err)
	}
	e.gitDir = gitDir

	// Resource allowance.
	poolCPU, poolMem := 0, int64(0)
	if !e.cfg.NoPool {
		p, err := pool.Open(e.cfg.Pool)
		if err != nil {
			return nil, fmt.Errorf("lap: open resource pool: %w", err)
		}
		e.pool = p
		poolCPU, poolMem = p.Size()
	} else {
		poolCPU, poolMem = defaultAllowance()
	}
	e.cpu, e.mem = poolCPU, poolMem
	if e.cfg.CPU > 0 && e.cfg.CPU < e.cpu {
		e.cpu = e.cfg.CPU
	}
	if e.cfg.Memory > 0 && e.cfg.Memory < e.mem {
		e.mem = e.cfg.Memory
	}
	e.freeCPU, e.freeMem = e.cpu, e.mem

	st, err := store.Open(root, e.cfg.Store)
	if err != nil {
		return nil, fmt.Errorf("lap: open store: %w", err)
	}
	e.store = st

	nodes := e.buildGraph(byName)

	if e.opt.PlanOnly {
		return nodes, nil
	}

	lock, err := pool.TryLock(filepath.Join(gitDir, "lap", "run.lock"), map[string]any{"pid": os.Getpid(), "started": e.start})
	if errors.Is(err, pool.ErrBusy) {
		var meta map[string]any
		_ = pool.ReadLockMeta(filepath.Join(gitDir, "lap", "run.lock"), &meta)
		return nil, fmt.Errorf("%w (holder: %v)", ErrBusy, meta)
	}
	if err != nil {
		return nil, fmt.Errorf("lap: worktree lock: %w", err)
	}
	e.lockCloser = lock

	commit, _ := gitOutput(root, "rev-parse", "HEAD")
	r, err := st.NewRun(store.RunMeta{
		Started:    e.start,
		Worktree:   root,
		Commit:     commit,
		Base:       sc.Base,
		BaseRef:    sc.BaseRef,
		ScopeAll:   sc.All,
		ScopeFiles: len(sc.Files),
		Budget:     e.budget,
		Mode:       e.opt.Mode.String(),
		Groups:     e.opt.Select,
		Machine:    store.DetectMachine(),
		Argv:       e.opt.Argv,
	})
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("lap: new run: %w", err)
	}
	e.run = r
	e.report.RunID = r.ID()
	e.report.RunDir = r.Dir()
	return nodes, nil
}

func defaultAllowance() (int, int64) {
	n := numCPU()
	cpu := n - n/4
	if cpu < 1 {
		cpu = 1
	}
	return cpu, 4 << 30
}

func checkCycles(tasks []Task) error {
	deps := map[string][]string{}
	for _, t := range tasks {
		deps[t.Name] = t.After
	}
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var visit func(string, []string) error
	visit = func(n string, path []string) error {
		switch color[n] {
		case grey:
			return fmt.Errorf("lap: dependency cycle: %s", strings.Join(append(path, n), " -> "))
		case black:
			return nil
		}
		color[n] = grey
		for _, d := range deps[n] {
			if err := visit(d, append(path, n)); err != nil {
				return err
			}
		}
		color[n] = black
		return nil
	}
	for _, t := range tasks {
		if err := visit(t.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

// selected returns the tasks named by opt.Select (groups or task names) plus
// their transitive prerequisites, in declaration order.
func (e *engine) selected(byName map[string]*Task) []*Task {
	want := map[string]bool{}
	if len(e.opt.Select) == 0 {
		for _, t := range e.cfg.Tasks {
			want[t.Name] = true
		}
	} else {
		for _, s := range e.opt.Select {
			for _, t := range e.cfg.Tasks {
				if t.Name == s || t.hasGroup(s) {
					want[t.Name] = true
				}
			}
		}
		var add func(string)
		add = func(n string) {
			for _, d := range byName[n].After {
				if !want[d] {
					want[d] = true
					add(d)
				}
			}
		}
		for n := range want {
			add(n)
		}
	}
	var out []*Task
	for i := range e.cfg.Tasks {
		if want[e.cfg.Tasks[i].Name] {
			out = append(out, &e.cfg.Tasks[i])
		}
	}
	return out
}

func (e *engine) buildGraph(byName map[string]*Task) []*node {
	tasks := e.selected(byName)
	nodes := make([]*node, 0, len(tasks))
	index := map[string]*node{}
	for _, t := range tasks {
		n := &node{task: t, phase: t.Phase}
		if e.opt.Mode == CheckOnly {
			n.phase = Check
		}
		n.writer = t.Phase == Prepare && e.opt.Mode == Fix
		n.inv = Invocation{Root: e.root, Mode: e.opt.Mode, Scope: e.scope}
		applies, all, files, reason := e.applicability(t)
		n.reason = reason
		if !applies {
			n.state = done
			n.attempt = e.baseAttempt(n, "")
			n.attempt.Outcome = store.Skipped
			n.attempt.Reason = reason
		}
		n.inv.All = all
		n.inv.Files = files
		n.scopeN = len(files)
		if all {
			n.scopeN = len(e.scope.Files)
		}
		nodes = append(nodes, n)
		index[t.Name] = n
	}

	// Explicit dependencies.
	for _, n := range nodes {
		for _, d := range n.task.After {
			if dn := index[d]; dn != nil {
				n.deps = append(n.deps, dn)
			}
		}
	}
	// Writers that may overlap run in declaration order.
	var writers []*node
	for _, n := range nodes {
		if n.writer && n.state != done {
			for _, w := range writers {
				if overlaps(w, n) {
					n.deps = append(n.deps, w)
				}
			}
			writers = append(writers, n)
		}
	}
	// Preparation barrier: checks wait for every applicable writer.
	for _, n := range nodes {
		if n.phase == Check {
			for _, w := range writers {
				if w != n {
					n.deps = append(n.deps, w)
				}
			}
		}
	}

	for _, n := range nodes {
		if n.state != done {
			n.options = e.options(n)
		}
	}
	return nodes
}

func overlaps(a, b *node) bool {
	if a.inv.All || b.inv.All || !a.task.PerFile || !b.task.PerFile {
		return true
	}
	set := map[string]bool{}
	for _, f := range a.inv.Files {
		set[f] = true
	}
	for _, f := range b.inv.Files {
		if set[f] {
			return true
		}
	}
	return false
}

// applicability decides whether a task runs and on what scope.
func (e *engine) applicability(t *Task) (applies, all bool, files []string, reason string) {
	maxFiles := t.MaxFiles
	if maxFiles == 0 {
		maxFiles = 1000
	}
	sc := e.scope
	switch {
	case sc.All:
		all = true
		reason = "full scope"
	case len(t.Config) > 0 && sc.Touches(t.Config):
		all = true
		reason = "config changed"
	case t.Files == nil:
		all = !t.PerFile
		if t.PerFile {
			files = sc.Files
		}
		reason = "always"
	default:
		files = sc.Match(t.Files)
		if len(files) == 0 {
			if !sc.Touches(t.Files) {
				return false, false, nil, "no matching changes"
			}
			if t.PerFile {
				return false, false, nil, "only deletions matched"
			}
		}
		reason = fmt.Sprintf("%d changed files", len(files))
	}
	if t.PerFile && all {
		files = nil
	}
	if !t.PerFile {
		all = true
		if len(files) > 0 {
			reason = fmt.Sprintf("%d changed files (project-wide)", len(files))
		}
		files = nil
	} else if len(files) > maxFiles {
		all = true
		reason = fmt.Sprintf("%d files > %d: project-wide", len(files), maxFiles)
		files = nil
	}
	return true, all, files, reason
}

func (e *engine) options(n *node) []Option {
	bucket := store.ScopeBucketFor(n.inv.All, len(n.inv.Files))
	var out []Option
	for i, v := range n.task.Variants {
		min, max := v.cpuRange(e.cpu)
		est := e.store.Estimate(store.EstimateQuery{
			Task: n.task.Name, Variant: v.name(), ScopeBucket: bucket, Machine: e.host, Workers: max,
		})
		if est.Samples == 0 && est.LowerBound > 0 && e.deferredStreak(n.task.Name, v.name()) >= retryAfterDeferrals {
			// Only a timeout is known, and it has kept this variant out of
			// the last runs. Caches may have warmed since: try again rather
			// than defer forever on one censored sample.
			est = store.Estimate{Confidence: "none", Source: fmt.Sprintf("retrying: last timeout %s is old evidence", est.LowerBound.Round(time.Second))}
		}
		if est.Duration == 0 && v.Estimate > 0 && est.LowerBound == 0 {
			est = store.Estimate{Duration: v.Estimate, Confidence: "config", Source: "configured"}
		}
		mem := v.memory()
		if mem > e.mem {
			mem = e.mem
		}
		out = append(out, Option{Variant: i, Name: v.name(), Estimate: est, MinCPU: min, MaxCPU: max, Memory: mem})
	}
	return out
}

func (e *engine) baseAttempt(n *node, variant string) store.Attempt {
	return store.Attempt{
		RunID:       e.report.RunID,
		Task:        n.task.Name,
		Variant:     variant,
		Phase:       phaseLabel(n),
		Groups:      n.task.Groups,
		Mode:        e.opt.Mode.String(),
		Worktree:    e.root,
		ScopeAll:    n.inv.All,
		ScopeFiles:  n.scopeN,
		ScopeBucket: store.ScopeBucketFor(n.inv.All, len(n.inv.Files)),
		Machine:     e.host,
	}
}

type finished struct {
	n   *node
	res run.Result
	err error
}

// execute runs nodes until all are done or the work cutoff passes.
func (e *engine) execute(ctx context.Context, nodes []*node) {
	workCtx, cancel := context.WithDeadline(ctx, e.cutoff)
	defer cancel()
	doneCh := make(chan finished, len(nodes))
	runningN := 0
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	byName := map[string]*node{}
	for _, n := range nodes {
		byName[n.task.Name] = n
	}

	for {
		// Propagate blocked prerequisites.
		for changed := true; changed; {
			changed = false
			for _, n := range nodes {
				if n.state != pending {
					continue
				}
				for _, d := range n.deps {
					if d.state == done && !d.succeeded() {
						e.complete(n, store.Blocked, fmt.Sprintf("prerequisite %s %s", d.task.Name, d.attempt.Outcome))
						changed = true
						break
					}
				}
			}
		}

		pendingN := 0
		var ready []*node
		now := time.Now()
		for _, n := range nodes {
			if n.state != pending {
				continue
			}
			pendingN++
			ok := true
			for _, d := range n.deps {
				if d.state != done {
					ok = false
					break
				}
			}
			if ok {
				if n.readyAt.IsZero() {
					n.readyAt = now
				}
				ready = append(ready, n)
			}
		}
		if pendingN == 0 && runningN == 0 {
			return
		}

		if workCtx.Err() != nil {
			reason := "budget exhausted"
			if ctx.Err() != nil {
				reason = "interrupted"
			}
			for _, n := range nodes {
				if n.state != pending {
					continue
				}
				o, why := store.Deferred, reason
				if ctx.Err() != nil {
					o = store.Interrupted
				} else {
					for _, d := range n.deps {
						if d.state != done || !d.succeeded() {
							o, why = store.Blocked, "prerequisite "+d.task.Name+" did not finish"
							break
						}
					}
				}
				e.complete(n, o, why)
			}
			if runningN == 0 {
				return
			}
		} else if len(ready) > 0 {
			e.admit(workCtx, ready, byName, doneCh, &runningN)
		}

		select {
		case f := <-doneCh:
			runningN--
			e.collect(ctx, f)
		case <-tick.C:
		}
	}
}

func (e *engine) snapshot(ready []*node, runningUnknown, runningN int) Snapshot {
	s := Snapshot{
		Budget:         e.budget,
		Remaining:      time.Until(e.cutoff),
		FreeCPU:        e.freeCPU,
		FreeMem:        e.freeMem,
		Allowance:      e.cpu,
		Running:        runningN,
		RunningUnknown: runningUnknown,
	}
	now := time.Now()
	for _, n := range ready {
		s.Ready = append(s.Ready, Candidate{Task: n.task, Options: n.options, Waited: now.Sub(n.readyAt)})
	}
	return s
}

func (e *engine) admit(ctx context.Context, ready []*node, byName map[string]*node, doneCh chan finished, runningN *int) {
	unknown := 0
	for _, n := range byName {
		if n.state == running && n.unknown {
			unknown++
		}
	}
	d := e.cfg.Policy.Decide(ctx, e.snapshot(ready, unknown, *runningN))
	isReady := map[string]bool{}
	for _, n := range ready {
		isReady[n.task.Name] = true
	}
	for _, df := range d.Defer {
		if n := byName[df.Task]; n != nil && isReady[df.Task] && n.state == pending {
			e.complete(n, store.Deferred, df.Reason)
		}
	}
	for _, a := range d.Admit {
		n := byName[a.Task]
		if n == nil || !isReady[a.Task] || n.state != pending || a.Variant < 0 || a.Variant >= len(n.task.Variants) {
			continue // invalid admission: ignore
		}
		o := n.options[a.Variant]
		cpu := a.CPU
		if cpu < o.MinCPU {
			cpu = o.MinCPU
		}
		if cpu > o.MaxCPU {
			cpu = o.MaxCPU
		}
		if cpu > e.freeCPU || o.Memory > e.freeMem {
			continue
		}
		if o.Estimate.Duration > 0 && o.Estimate.Duration > time.Until(e.cutoff) {
			continue
		}
		if e.start1(ctx, n, a.Variant, cpu, o, doneCh) {
			*runningN++
		}
	}
}

// start1 launches one node. It returns false if the node did not start (the
// machine-wide pool is busy, or the node completed immediately).
func (e *engine) start1(ctx context.Context, n *node, vi, cpu int, o Option, doneCh chan finished) bool {
	v := n.task.Variants[vi]
	inv := n.inv
	inv.Workers = cpu
	argv, err := v.Cmd(inv)
	if err != nil {
		e.complete(n, store.ToolError, "building command: "+err.Error())
		return false
	}
	if argv == nil {
		e.complete(n, store.Skipped, "nothing to do")
		return false
	}

	contended := false
	if e.pool != nil {
		freeCPU, _ := e.pool.Free()
		poolCPU, _ := e.pool.Size()
		ours := e.cpu - e.freeCPU
		contended = poolCPU-freeCPU-ours > 0
		lease, err := e.pool.TryAcquire(pool.Request{CPU: cpu, Memory: o.Memory, Holder: pool.Holder{
			PID: os.Getpid(), Worktree: e.root, Task: n.task.Name, RunID: e.report.RunID,
		}})
		if errors.Is(err, pool.ErrBusy) {
			if n.attempt.Reason == "" {
				n.attempt.Reason = "waiting: machine busy (other lap runs hold the pool)"
			}
			return false
		}
		if err != nil {
			e.complete(n, store.ToolError, "resource pool: "+err.Error())
			return false
		}
		n.lease = lease
	}

	e.seq++
	n.attempt = e.baseAttempt(n, v.name())
	n.attempt.AttemptID = fmt.Sprintf("%02d-%s", e.seq, sanitize(n.task.Name))
	n.attempt.Workers = cpu
	n.attempt.CPU = cpu
	n.attempt.Memory = o.Memory
	n.attempt.Queued = n.readyAt
	n.attempt.Start = time.Now()
	n.attempt.Contended = contended
	n.attempt.CaptureMode = v.Capture.String()
	if vi > 0 {
		n.attempt.Reason = narrowedReason
	}
	n.unknown = o.Estimate.Duration == 0
	n.cpu, n.mem = cpu, o.Memory
	e.freeCPU -= cpu
	e.freeMem -= o.Memory
	n.state = running

	n.before = e.snapshotDirty()

	capDir := e.run.CaptureDir(n.attempt.AttemptID)
	n.attempt.CaptureDir, _ = filepath.Rel(e.run.Dir(), capDir)

	if e.cfg.Nice > 0 {
		if nice, err := exec.LookPath("nice"); err == nil {
			argv = append([]string{nice, "-n", fmt.Sprint(e.cfg.Nice)}, argv...)
		}
	}
	env := append(os.Environ(), e.cfg.Env...)
	env = append(env, v.Env...)
	env = append(env, fmt.Sprintf("LAP_WORKERS=%d", cpu))
	cmd := run.Command{
		Argv: argv,
		Dir:  filepath.Join(e.root, v.Dir),
		Env:  env,
		Capture: term.Spec{
			Mode: v.Capture,
			Dir:  capDir,
			Cols: e.cfg.Cols,
			Rows: e.cfg.Rows,
		},
		Cleanup: e.cfg.Cleanup,
	}
	e.emit(Event{Kind: TaskStarted, Task: n.task.Name, Attempt: &n.attempt})
	go func() {
		res, err := run.Run(ctx, cmd)
		doneCh <- finished{n: n, res: res, err: err}
	}()
	return true
}

func (e *engine) collect(parent context.Context, f finished) {
	n := f.n
	if n.lease != nil {
		// Release only after run.Run returned, i.e. after verified cleanup.
		if f.res.Cleanup.Verified || f.err != nil {
			n.lease.Close()
		} else {
			// Unverified cleanup: keep the tokens until this process exits
			// rather than claim capacity that may still be in use.
			e.note("%s: cleanup not verified; keeping its resource tokens", n.task.Name)
		}
		n.lease = nil
	}
	e.freeCPU += n.cpu
	e.freeMem += n.mem
	a := &n.attempt
	a.End = time.Now()
	res := f.res
	if f.err != nil {
		e.complete(n, store.ToolError, f.err.Error())
		return
	}
	a.ExitCode = res.Exit.Code
	if res.Exit.Signal != 0 {
		a.Signal = res.Exit.Signal.String()
	}
	if ru := res.Exit.Rusage; ru != nil {
		a.MaxRSSKB = ru.MaxRSSBytes / 1024
		a.UserCPU = ru.User
		a.SysCPU = ru.System
	}
	a.CaptureComplete = res.CaptureErr == nil

	var changed []string
	if n.writer {
		e.snap = nil // the workspace may have changed
		changed = changedSince(n.before, dirtySnapshot(e.root), n.task, n.inv)
		a.ChangedFiles = changed
	}

	switch {
	case res.TimedOut && parent.Err() != nil:
		a.Censored = true
		e.complete(n, store.Interrupted, "interrupted")
		return
	case res.TimedOut:
		a.Censored = true
		if n.writer {
			e.complete(n, store.Interrupted, fmt.Sprintf("writer interrupted by deadline; %d files changed", len(changed)))
		} else {
			e.complete(n, store.TimedOut, "killed at the work cutoff")
		}
		return
	}

	c := &Classification{Task: n.task, Variant: n.task.Variants[variantIndex(n)], Mode: e.opt.Mode,
		ExitCode: res.Exit.Code, captureDir: filepath.Join(e.run.Dir(), a.CaptureDir)}
	defaultClassify(c)
	if res.Exit.Signal != 0 {
		c.Outcome = store.ToolError
		c.Reason = "killed by " + res.Exit.Signal.String()
	} else if n.task.Classify != nil {
		n.task.Classify(c)
	}
	if n.writer && c.Outcome == store.Passed && len(changed) > 0 {
		c.Outcome = store.Fixed
	}
	if !res.Cleanup.Verified {
		c.Reason = joinReason(c.Reason, "cleanup not verified")
	}
	if res.CaptureErr != nil {
		c.Reason = joinReason(c.Reason, "capture incomplete")
	}
	n.finds = c.Findings
	a.Findings = len(c.Findings)
	if c.Outcome == store.Findings && a.Findings == 0 {
		a.Findings = 1
	}
	reason := c.Reason
	if a.Reason == narrowedReason {
		reason = joinReason(narrowedReason, reason)
	}
	e.complete(n, c.Outcome, reason)
}

func variantIndex(n *node) int {
	for i, v := range n.task.Variants {
		if v.name() == n.attempt.Variant {
			return i
		}
	}
	return 0
}

func joinReason(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// complete marks n done with outcome o, records and reports it.
func (e *engine) complete(n *node, o store.Outcome, reason string) {
	if n.attempt.Task == "" {
		n.attempt = e.baseAttempt(n, "")
	}
	if n.attempt.Queued.IsZero() && !n.readyAt.IsZero() {
		n.attempt.Queued = n.readyAt
	}
	n.attempt.Outcome = o
	if reason != "" || o != store.Passed {
		n.attempt.Reason = reason
	}
	n.state = done
	e.record(n)
}

func (e *engine) record(n *node) {
	n.attempt.RunID = e.report.RunID
	if e.run != nil {
		if n.attempt.AttemptID == "" {
			e.seq++
			n.attempt.AttemptID = fmt.Sprintf("%02d-%s", e.seq, sanitize(n.task.Name))
		}
		if err := e.run.RecordAttempt(n.attempt); err != nil {
			e.report.Notes = append(e.report.Notes, "store: "+err.Error())
		}
	}
	if len(n.finds) > 0 {
		e.report.Findings[n.task.Name] = n.finds
	}
	e.report.Attempts = append(e.report.Attempts, n.attempt)
	a := n.attempt
	e.emit(Event{Kind: TaskFinished, Task: n.task.Name, Attempt: &a})
}

// checkFreshness marks passed checks stale when files they read changed
// while they ran or afterwards in this run (best effort; not snapshot
// isolation).
func (e *engine) checkFreshness(nodes []*node) {
	now := dirtySnapshot(e.root)
	for _, n := range nodes {
		if n.writer || n.before == nil || n.attempt.Outcome != store.Passed {
			continue
		}
		if ch := changedSince(n.before, now, n.task, n.inv); len(ch) > 0 {
			n.attempt.Outcome = store.Stale
			n.attempt.Reason = fmt.Sprintf("inputs changed during the run: %s", summarizePaths(ch))
			for i := range e.report.Attempts {
				if e.report.Attempts[i].AttemptID == n.attempt.AttemptID {
					e.report.Attempts[i] = n.attempt
				}
			}
			if e.run != nil {
				_ = e.run.RecordAttempt(n.attempt)
			}
		}
	}
}

// snapshotDirty returns the dirty-file snapshot, reusing it until a writer
// finishes.
func (e *engine) snapshotDirty() map[string]string {
	if e.snap == nil {
		e.snap = dirtySnapshot(e.root)
	}
	return e.snap
}

func (e *engine) finish() *Report {
	r := e.report
	r.Elapsed = time.Since(e.start)
	switch {
	case e.parent.Err() != nil:
		r.ExitCode = 130
	case r.LatestCount(store.Findings) > 0:
		r.ExitCode = 1
	case r.LatestCount(store.ToolError) > 0:
		r.ExitCode = 3
	default:
		r.ExitCode = 0
	}
	if e.run != nil {
		counts := map[store.Outcome]int{}
		for _, a := range r.Attempts {
			counts[a.Outcome]++
		}
		sum := store.RunSummary{Ended: time.Now(), Elapsed: r.Elapsed, Counts: counts, ExitCode: r.ExitCode, Notes: r.Notes}
		if over := r.Elapsed - e.budget; over > 0 {
			sum.Overrun = over
		}
		_ = e.run.Finish(sum)
	}
	if e.lockCloser != nil {
		e.lockCloser.Close()
	}
	e.emit(Event{Kind: RunFinished, Report: r})
	go e.store.Prune() //nolint:errcheck // best effort, after reporting
	return r
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func phaseLabel(n *node) string {
	if n.label != "" {
		return n.label
	}
	return n.phase.String()
}

// retryAfterDeferrals is how many consecutive deferrals based only on a
// timeout make the engine try a variant again.
const retryAfterDeferrals = 3

// deferredStreak counts the most recent consecutive attempts of task that
// were deferred while their broadest variant was variant.
func (e *engine) deferredStreak(task, variant string) int {
	hist, err := e.store.History(store.Query{Task: task, Worktree: e.root, Limit: 10})
	if err != nil {
		return 0
	}
	n := 0
	for i := len(hist) - 1; i >= 0; i-- {
		a := hist[i]
		if a.Outcome != store.Deferred || !strings.HasPrefix(a.Reason, variant+" needs") {
			break
		}
		n++
	}
	return n
}
