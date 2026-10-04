package lap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vivster7/lap/pool"
	"github.com/vivster7/lap/store"
)

// testRepo creates a repository with an origin whose main has a.txt and
// b.py, and a checked-out feature branch where the given files changed.
func testRepo(t *testing.T, changes map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	repo := filepath.Join(dir, "repo")
	git := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(dir, "init", "-q", "--bare", "-b", "main", origin)
	git(dir, "init", "-q", "-b", "main", repo)
	write(t, repo, "a.txt", "a\n")
	write(t, repo, "b.py", "print('b')\n")
	git(repo, "add", ".")
	git(repo, "commit", "-qm", "init")
	git(repo, "remote", "add", "origin", origin)
	git(repo, "push", "-q", "origin", "main")
	git(repo, "remote", "set-head", "origin", "main")
	git(repo, "checkout", "-qb", "feature")
	for f, c := range changes {
		write(t, repo, f, c)
	}
	return repo
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func shTask(name string, phase Phase, script string) Task {
	return Task{Name: name, Phase: phase, Groups: []string{phase.String()},
		Variants: []Variant{{Name: "default", Cmd: Shell(script), Estimate: 500 * time.Millisecond}}}
}

func testConfig(tasks ...Task) Config {
	return Config{Name: "dev", Tasks: tasks, NoPool: true, CPU: 4, Cleanup: 500 * time.Millisecond, Nice: -1}
}

func byTask(r *Report) map[string]store.Attempt {
	m := map[string]store.Attempt{}
	for _, a := range r.Latest() {
		m[a.Task] = a
	}
	return m
}

func mustRun(t *testing.T, cfg Config, opt RunOptions) *Report {
	t.Helper()
	r, err := Run(context.Background(), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPassFindingsAndSkip(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "changed\n"})
	py := shTask("py-only", Check, "true")
	py.Files = []string{"*.py"}
	cfg := testConfig(
		shTask("ok", Check, "echo fine"),
		shTask("bad", Check, "echo 'a.txt:1:2: something is wrong'; exit 1"),
		py,
	)
	cfg.Tasks[1].Classify = ClassifyFileLine()
	r := mustRun(t, cfg, RunOptions{Root: repo})
	got := byTask(r)
	if got["ok"].Outcome != store.Passed {
		t.Errorf("ok: %v", got["ok"].Outcome)
	}
	if got["bad"].Outcome != store.Findings || got["bad"].Findings != 1 {
		t.Errorf("bad: %v %d", got["bad"].Outcome, got["bad"].Findings)
	}
	if f := r.Findings["bad"]; len(f) != 1 || f[0].File != "a.txt" || f[0].Line != 1 || f[0].Col != 2 {
		t.Errorf("findings: %+v", f)
	}
	if got["py-only"].Outcome != store.Skipped {
		t.Errorf("py-only should be skipped (no .py changes): %v", got["py-only"].Outcome)
	}
	if r.ExitCode != 1 {
		t.Errorf("exit %d, want 1", r.ExitCode)
	}
	if r.RunID == "" || r.Scope.BaseRef != "origin/main" {
		t.Errorf("run id %q base %q", r.RunID, r.Scope.BaseRef)
	}
}

func TestDeferWhenEstimateExceedsBudget(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	slow := shTask("slow", Check, "sleep 5")
	slow.Variants[0].Estimate = 30 * time.Second
	cfg := testConfig(shTask("fast", Check, "true"), slow)
	start := time.Now()
	r := mustRun(t, cfg, RunOptions{Root: repo, Budget: 3 * time.Second})
	got := byTask(r)
	if got["slow"].Outcome != store.Deferred || !strings.Contains(got["slow"].Reason, "needs") {
		t.Errorf("slow: %v %q", got["slow"].Outcome, got["slow"].Reason)
	}
	if got["fast"].Outcome != store.Passed {
		t.Errorf("fast: %v", got["fast"].Outcome)
	}
	if r.ExitCode != 0 {
		t.Errorf("partial coverage without findings must exit 0, got %d", r.ExitCode)
	}
	if r.Complete() {
		t.Errorf("report should not be complete")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("deferral should be immediate, took %v", time.Since(start))
	}
}

func TestNarrowsToVariantThatFits(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	task := Task{Name: "tests", Phase: Check, Variants: []Variant{
		{Name: "full", Cmd: Shell("sleep 5"), Estimate: 20 * time.Second},
		{Name: "changed", Cmd: Shell("echo changed-only"), Estimate: 200 * time.Millisecond},
	}}
	r := mustRun(t, testConfig(task), RunOptions{Root: repo, Budget: 4 * time.Second})
	a := byTask(r)["tests"]
	if a.Variant != "changed" || a.Outcome != store.Passed || !strings.HasPrefix(a.Reason, narrowedReason) {
		t.Errorf("got variant %q outcome %v reason %q", a.Variant, a.Outcome, a.Reason)
	}
	if r.Complete() {
		t.Errorf("narrowed coverage is not complete")
	}
}

func TestDeadlineKillsUnknownTask(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	hang := Task{Name: "hang", Phase: Check, Variants: []Variant{{Name: "default", Cmd: Shell("echo started; sleep 30")}}}
	start := time.Now()
	r := mustRun(t, testConfig(hang), RunOptions{Root: repo, Budget: 2 * time.Second})
	el := time.Since(start)
	a := byTask(r)["hang"]
	if a.Outcome != store.TimedOut || !a.Censored {
		t.Errorf("hang: %v censored=%v", a.Outcome, a.Censored)
	}
	if el > 3*time.Second {
		t.Errorf("run took %v for a 2s budget", el)
	}
	if r.ExitCode != 0 {
		t.Errorf("timeout without findings exits 0, got %d", r.ExitCode)
	}
}

func TestWriterFixesBeforeChecks(t *testing.T) {
	repo := testRepo(t, map[string]string{"b.py": "print('UGLY')\n"})
	fmtTask := Task{Name: "fmt", Phase: Prepare, Files: []string{"*.py"}, PerFile: true, Variants: []Variant{{
		Name: "default", Estimate: 300 * time.Millisecond,
		Cmd: func(inv Invocation) ([]string, error) {
			if inv.Mode == CheckOnly {
				return append([]string{"sh", "-c", `! grep -l UGLY "$@"`, "sh"}, inv.Files...), nil
			}
			// Portable in-place edit (BSD sed's -i differs from GNU's).
			return append([]string{"sh", "-c", `for f; do sed s/UGLY/pretty/ "$f" > "$f.tmp" && mv "$f.tmp" "$f"; done`, "sh"}, inv.Files...), nil
		},
	}}}
	check := shTask("lint", Check, "! grep -q UGLY b.py")
	cfg := testConfig(fmtTask, check)

	r := mustRun(t, cfg, RunOptions{Root: repo, Mode: CheckOnly})
	if got := byTask(r); got["fmt"].Outcome != store.Findings || got["lint"].Outcome != store.Findings {
		t.Fatalf("check-only: fmt %v lint %v", got["fmt"].Outcome, got["lint"].Outcome)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "b.py")); !strings.Contains(string(b), "UGLY") {
		t.Fatalf("check-only mode modified the file")
	}

	r = mustRun(t, cfg, RunOptions{Root: repo})
	got := byTask(r)
	if got["fmt"].Outcome != store.Fixed || len(got["fmt"].ChangedFiles) != 1 || got["fmt"].ChangedFiles[0] != "b.py" {
		t.Errorf("fmt: %v %v", got["fmt"].Outcome, got["fmt"].ChangedFiles)
	}
	if got["lint"].Outcome != store.Passed {
		t.Errorf("lint must see the fixed file (barrier): %v", got["lint"].Outcome)
	}
	if got["lint"].Start.Before(got["fmt"].End) {
		t.Errorf("lint started before fmt ended")
	}
	if r.ExitCode != 0 {
		t.Errorf("exit %d", r.ExitCode)
	}
}

func TestBlockedWhenPreparationIncomplete(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	gen := Task{Name: "gen", Phase: Prepare, Variants: []Variant{{Name: "default", Cmd: Shell("sleep 30")}}}
	cfg := testConfig(gen, shTask("check", Check, "true"))
	r := mustRun(t, cfg, RunOptions{Root: repo, Budget: 2 * time.Second})
	got := byTask(r)
	if got["gen"].Outcome != store.Interrupted {
		t.Errorf("gen: %v", got["gen"].Outcome)
	}
	if got["check"].Outcome != store.Blocked {
		t.Errorf("check: %v %q", got["check"].Outcome, got["check"].Reason)
	}
}

func TestHistoryEstimatesAndPlan(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	task := Task{Name: "t", Phase: Check, Variants: []Variant{{Name: "default", Cmd: Shell("sleep 0.3")}}}
	cfg := testConfig(task)
	for i := 0; i < 3; i++ {
		mustRun(t, cfg, RunOptions{Root: repo})
	}
	r := mustRun(t, cfg, RunOptions{Root: repo, PlanOnly: true})
	e := r.Plan.Entries[0]
	if e.Status != "run" || e.Confidence != "ok" || e.Estimate < 300*time.Millisecond || e.Estimate > 2*time.Second {
		t.Errorf("plan entry %+v", e)
	}
	if len(r.Attempts) != 0 {
		t.Errorf("plan-only must not run tasks")
	}
}

func TestAutofixRemediation(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	check := shTask("lockfile", Check, `test -f lock.txt || { echo "error: lock.txt is out of date"; exit 1; }`)
	fix := shTask("relock", Prepare, "touch lock.txt")
	cfg := testConfig(check)
	cfg.Rules = []Rule{{Name: "relock", Task: "lockfile", Match: regexp.MustCompile(`lock.txt is out of date`),
		Suggest: "run relock", Fix: &fix}}

	r := mustRun(t, cfg, RunOptions{Root: repo})
	if len(r.Suggestions) != 1 || r.ExitCode != 1 {
		t.Fatalf("suggestions %v exit %d", r.Suggestions, r.ExitCode)
	}
	r = mustRun(t, cfg, RunOptions{Root: repo, Autofix: true})
	got := byTask(r)
	if got["relock"].Outcome != store.Fixed || got["lockfile"].Outcome != store.Passed {
		t.Fatalf("relock %v lockfile %v (%q)", got["relock"].Outcome, got["lockfile"].Outcome, got["lockfile"].Reason)
	}
	if got["lockfile"].Phase != "remediation" || r.ExitCode != 0 {
		t.Errorf("phase %q exit %d", got["lockfile"].Phase, r.ExitCode)
	}
}

func TestWorktreeLockAndPool(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	poolDir := t.TempDir()
	cfg := testConfig(shTask("a", Check, "sleep 1"), shTask("b", Check, "sleep 1"))
	cfg.NoPool = false
	cfg.Pool = pool.Config{Dir: poolDir, CPU: 1, MemoryBytes: 4 << 30}

	done := make(chan *Report)
	go func() { done <- mustRun(t, cfg, RunOptions{Root: repo}) }()
	time.Sleep(300 * time.Millisecond)
	if _, err := Run(context.Background(), cfg, RunOptions{Root: repo}); err == nil || !strings.Contains(err.Error(), "another run") {
		t.Errorf("second run in the same worktree: %v", err)
	}
	r := <-done
	got := byTask(r)
	// One CPU token: the two tasks must not overlap.
	a, b := got["a"], got["b"]
	if a.Outcome != store.Passed || b.Outcome != store.Passed {
		t.Fatalf("a %v b %v", a.Outcome, b.Outcome)
	}
	if a.Start.Before(b.End) && b.Start.Before(a.End) {
		t.Errorf("tasks overlapped with a 1-cpu pool")
	}
}

func TestRetryAfterRepeatedTimeoutDeferrals(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "x\n"})
	marker := filepath.Join(t.TempDir(), "warm")
	// Slow until the marker exists (a cold cache), then fast.
	task := Task{Name: "build", Phase: Check, Variants: []Variant{{Name: "all",
		Cmd: Shell("if [ -f " + marker + " ]; then exit 0; fi; sleep 30")}}}
	cfg := testConfig(task)
	opt := RunOptions{Root: repo, Budget: 2 * time.Second}

	// A longer first budget makes the recorded timeout (a lower bound of
	// ~2.5s) clearly exceed what later 2s runs have left.
	if a := byTask(mustRun(t, cfg, RunOptions{Root: repo, Budget: 3 * time.Second}))["build"]; a.Outcome != store.TimedOut {
		t.Fatalf("first run: %v", a.Outcome)
	}
	os.WriteFile(marker, nil, 0o644)
	for i := 0; i < retryAfterDeferrals; i++ {
		if a := byTask(mustRun(t, cfg, opt))["build"]; a.Outcome != store.Deferred {
			t.Fatalf("run %d: %v %q", i+2, a.Outcome, a.Reason)
		}
	}
	if a := byTask(mustRun(t, cfg, opt))["build"]; a.Outcome != store.Passed {
		t.Fatalf("after %d deferrals the variant should be retried: %v %q", retryAfterDeferrals, a.Outcome, a.Reason)
	}
}
