package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// noPrune disables every retention limit.
var noPrune = Retention{MaxRuns: -1, MaxAge: -1, MaxBytes: -1, MinFree: -1}

func openDir(t *testing.T, dir string, opt Options) *Store {
	t.Helper()
	opt.Dir = dir
	if opt.Retention == (Retention{}) {
		opt.Retention = noPrune
	}
	s, err := Open("", opt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newRun(t *testing.T, s *Store, meta RunMeta) *Run {
	t.Helper()
	if meta.Machine.Hostname == "" {
		meta.Machine = Machine{Hostname: "testhost", OS: "linux"}
	}
	r, err := s.NewRun(meta)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var attemptSeq int

// att builds a started attempt of the given duration ending now-ago.
func att(task string, d time.Duration, o Outcome) Attempt {
	attemptSeq++
	end := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(attemptSeq) * time.Minute)
	return Attempt{
		AttemptID: fmt.Sprintf("%s-%d", task, attemptSeq), Task: task, Variant: "full",
		Workers: 4, ScopeAll: true, Machine: "testhost", ConfigKey: "cfg1",
		Queued: end.Add(-d - time.Second), Start: end.Add(-d), End: end, Outcome: o,
	}
}

func record(t *testing.T, r *Run, as ...Attempt) {
	t.Helper()
	for _, a := range as {
		if err := r.RecordAttempt(a); err != nil {
			t.Fatal(err)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitLayoutAndWorktrees(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	base, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(base, "main")
	wt := filepath.Join(base, "wt")
	os.Mkdir(main, 0o755)
	git(t, main, "init", "-q", "-b", "main")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, main, "worktree", "add", "-q", "-b", "other", wt)
	os.Mkdir(filepath.Join(main, "sub"), 0o755)

	s1, err := Open(filepath.Join(main, "sub"), Options{Retention: noPrune})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(wt, Options{Retention: noPrune})
	if err != nil {
		t.Fatal(err)
	}
	wantHist := filepath.Join(main, ".git", "lap", "history.jsonl")
	if s1.HistoryPath() != wantHist || s2.HistoryPath() != wantHist {
		t.Fatalf("history paths %q %q, want %q", s1.HistoryPath(), s2.HistoryPath(), wantHist)
	}
	if want := filepath.Join(main, ".git", "lap", "runs"); s1.RunsDir() != want {
		t.Fatalf("runs dir %q want %q", s1.RunsDir(), want)
	}
	if want := filepath.Join(main, ".git", "worktrees", "wt", "lap", "runs"); s2.RunsDir() != want {
		t.Fatalf("worktree runs dir %q want %q", s2.RunsDir(), want)
	}
	if s1.Worktree() != main || s2.Worktree() != wt {
		t.Fatalf("worktrees %q %q", s1.Worktree(), s2.Worktree())
	}

	r := newRun(t, s2, RunMeta{})
	if r.Meta().Worktree != wt {
		t.Fatalf("run worktree %q", r.Meta().Worktree)
	}
	record(t, r, att("lint", time.Second, Passed))
	if err := r.Finish(RunSummary{}); err != nil {
		t.Fatal(err)
	}
	h, err := s1.History(Query{})
	if err != nil || len(h) != 1 || h[0].Worktree != wt {
		t.Fatalf("shared history from main: %v %+v", err, h)
	}
	if runs, _ := s1.Runs(0); len(runs) != 0 {
		t.Fatalf("main sees worktree's runs: %+v", runs)
	}
	if runs, _ := s2.Runs(0); len(runs) != 1 {
		t.Fatalf("worktree runs: %+v", runs)
	}
	// History is shared but LastAttempt is per worktree.
	if _, _, err := s1.LastAttempt("lint"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("main LastAttempt: %v", err)
	}
	if _, dir, err := s2.LastAttempt("lint"); err != nil || dir != r.Dir() {
		t.Fatalf("worktree LastAttempt: %q %v", dir, err)
	}
}

func TestRunLifecycle(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	r := newRun(t, s, RunMeta{Budget: time.Nanosecond, Mode: "check", Groups: []string{"py"}})
	if !strings.HasPrefix(filepath.Base(r.Dir()), time.Now().UTC().Format("20060102T")) || len(r.ID()) != len("20261003T142233Z-ab12cd") {
		t.Fatalf("run id %q", r.ID())
	}
	runs, err := s.Runs(0)
	if err != nil || len(runs) != 1 || runs[0].Status != StatusStarted {
		t.Fatalf("runs: %v %+v", err, runs)
	}
	ad := r.AttemptDir("ruff-1")
	if st, err := os.Stat(ad); err != nil || !st.IsDir() {
		t.Fatalf("attempt dir: %v", err)
	}
	cd := r.CaptureDir("ruff-1")
	if _, err := os.Stat(cd); !os.IsNotExist(err) || filepath.Dir(cd) != ad {
		t.Fatalf("capture dir %q must not exist: %v", cd, err)
	}
	if err := r.SavePlan(map[string]any{"tasks": []string{"ruff"}}); err != nil {
		t.Fatal(err)
	}
	a := att("ruff", 2*time.Second, Findings)
	a.AttemptID = "ruff-1"
	a.CaptureDir = cd
	a.ScopeFiles = 5
	a.ScopeAll = false
	record(t, r, a, Attempt{AttemptID: "mypy-1", Task: "mypy", Outcome: Deferred, Reason: "budget"})
	if err := r.Finish(RunSummary{ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(RunSummary{}); err == nil {
		t.Fatal("second Finish succeeded")
	}
	rec, atts, err := s.LoadRun(r.ID())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusCompleted || rec.Summary == nil || rec.Summary.ExitCode != 1 ||
		rec.Summary.Counts[Findings] != 1 || rec.Summary.Counts[Deferred] != 1 || rec.Summary.Overrun <= 0 ||
		rec.Mode != "check" || rec.Dir != r.Dir() {
		t.Fatalf("record %+v summary %+v", rec, rec.Summary)
	}
	if len(atts) != 2 {
		t.Fatalf("attempts %+v", atts)
	}
	got := atts[1] // the deferred attempt has zero Queued and sorts first
	if got.RunID != r.ID() || got.CaptureDir != "tasks/ruff-1/capture" || got.ScopeBucket != "2-10" {
		t.Fatalf("attempt defaults: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(r.Dir(), "plan.json")); err != nil {
		t.Fatal(err)
	}

	r2 := newRun(t, s, RunMeta{})
	if err := r2.Abandon("busy"); err != nil {
		t.Fatal(err)
	}
	rec2, _, err := s.LoadRun("last")
	if err != nil || rec2.ID != r2.ID() || rec2.Status != StatusAbandoned || rec2.Reason != "busy" {
		t.Fatalf("last: %v %+v", err, rec2)
	}

	// A run whose process died is reported incomplete.
	r3 := newRun(t, s, RunMeta{})
	r3.active.Close()
	rec3, _, err := s.LoadRun("")
	if err != nil || rec3.ID != r3.ID() || rec3.Status != StatusIncomplete {
		t.Fatalf("crashed run: %v %+v", err, rec3)
	}
	if _, _, err := s.LoadRun("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadRun missing: %v", err)
	}
	if runs, _ := s.Runs(2); len(runs) != 2 || runs[0].ID != r3.ID() || runs[1].ID != r2.ID() {
		t.Fatalf("Runs(2) order: %+v", runs)
	}
}

func TestLoadRunEmpty(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	if _, _, err := s.LoadRun("last"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestTruncatedHistoryTail(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir, Options{})
	r := newRun(t, s, RunMeta{})
	record(t, r, att("a", time.Second, Passed), att("b", time.Second, Passed))
	f, _ := os.OpenFile(s.HistoryPath(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.WriteString(`{"v":1,"run_id":"x","task":"torn","outc`)
	f.Close()

	for _, st := range []*Store{s, openDir(t, dir, Options{})} {
		h, err := st.History(Query{})
		if err != nil || len(h) != 2 {
			t.Fatalf("history with torn tail: %v %d", err, len(h))
		}
	}
	record(t, r, att("c", time.Second, Passed))
	for _, st := range []*Store{s, openDir(t, dir, Options{})} {
		h, err := st.History(Query{})
		if err != nil || len(h) != 3 || h[2].Task != "c" {
			t.Fatalf("history after append: %v %+v", err, h)
		}
	}
	data, _ := os.ReadFile(s.HistoryPath())
	if !strings.Contains(string(data), "\"outc\n{") {
		t.Fatalf("append did not start a fresh line:\n%s", data)
	}
}

func TestHistoryCacheAndQuery(t *testing.T) {
	dir := t.TempDir()
	reader := openDir(t, dir, Options{})
	writer := openDir(t, dir, Options{})
	if h, err := reader.History(Query{}); err != nil || len(h) != 0 {
		t.Fatalf("empty: %v %v", err, h)
	}
	r := newRun(t, writer, RunMeta{})
	record(t, r, att("a", time.Second, Passed))
	if h, _ := reader.History(Query{}); len(h) != 1 {
		t.Fatalf("incremental read: %d", len(h))
	}
	b := att("b", time.Second, Findings)
	b.Variant = "fast"
	record(t, r, b, att("a", time.Second, TimedOut), att("a", time.Second, Passed))
	if h, _ := reader.History(Query{Task: "a"}); len(h) != 3 {
		t.Fatalf("task filter: %d", len(h))
	}
	if h, _ := reader.History(Query{Variant: "fast"}); len(h) != 1 || h[0].Task != "b" {
		t.Fatalf("variant filter: %+v", h)
	}
	if h, _ := reader.History(Query{Outcomes: []Outcome{TimedOut, Findings}}); len(h) != 2 {
		t.Fatalf("outcome filter: %d", len(h))
	}
	if h, _ := reader.History(Query{Task: "a", Limit: 2}); len(h) != 2 || h[1].Outcome != Passed || h[0].Outcome != TimedOut {
		t.Fatalf("limit keeps newest: %+v", h)
	}
	if h, _ := reader.History(Query{Since: b.End}); len(h) != 3 {
		t.Fatalf("since: %d", len(h))
	}
	if h, _ := reader.History(Query{Worktree: "/elsewhere"}); len(h) != 0 {
		t.Fatalf("worktree: %d", len(h))
	}
	// A replaced (rotated) file is reloaded.
	os.Rename(writer.HistoryPath(), writer.HistoryPath()+".old")
	record(t, r, att("z", time.Second, Passed))
	if h, _ := reader.History(Query{}); len(h) != 1 || h[0].Task != "z" {
		t.Fatalf("after replace: %+v", h)
	}
}

func TestHistoryTailWindow(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir, Options{})
	r := newRun(t, s, RunMeta{})
	for i := 0; i < 100; i++ {
		record(t, r, att("t"+strconv.Itoa(i), time.Second, Passed))
	}
	info, _ := os.Stat(s.HistoryPath())
	small := openDir(t, dir, Options{HistoryTailBytes: info.Size() / 10})
	h, err := small.History(Query{})
	if err != nil || len(h) < 5 || len(h) > 11 || h[len(h)-1].Task != "t99" {
		t.Fatalf("tail window: %v %d", err, len(h))
	}
	all := openDir(t, dir, Options{HistoryTailBytes: -1})
	if h, _ := all.History(Query{}); len(h) != 100 {
		t.Fatalf("whole file: %d", len(h))
	}
}

func TestHistoryParallelParse(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir, Options{})
	f, _ := os.Create(s.HistoryPath())
	const n = 3000
	for i := 0; i < n; i++ {
		line, _ := json.Marshal(attemptJSON{Version: 1, Attempt: Attempt{RunID: "r", AttemptID: strconv.Itoa(i), Task: "t",
			Outcome: Passed, Reason: strings.Repeat("r", i%700)}})
		f.Write(append(line, '\n'))
		if i%1000 == 0 {
			f.WriteString("garbage\n\n")
		}
	}
	f.Close()
	if info, _ := os.Stat(s.HistoryPath()); info.Size() < parallelParseMin {
		t.Fatalf("history too small to exercise parallel parse: %d", info.Size())
	}
	h, err := s.History(Query{})
	if err != nil || len(h) != n {
		t.Fatalf("%v %d", err, len(h))
	}
	for i, a := range h {
		if a.AttemptID != strconv.Itoa(i) {
			t.Fatalf("order broken at %d: %s", i, a.AttemptID)
		}
	}
}

func TestLastAttempt(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	if _, _, err := s.LastAttempt("lint"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty: %v", err)
	}
	r1 := newRun(t, s, RunMeta{Started: time.Now().Add(-time.Hour)})
	first := att("lint", time.Second, Findings)
	record(t, r1, first)
	r1.Finish(RunSummary{})
	r2 := newRun(t, s, RunMeta{})
	record(t, r2, Attempt{AttemptID: "lint-deferred", Task: "lint", Outcome: Deferred})
	r2.Finish(RunSummary{})

	a, dir, err := s.LastAttempt("lint")
	if err != nil || a.AttemptID != first.AttemptID || dir != r1.Dir() {
		t.Fatalf("got %+v %q %v", a, dir, err)
	}
	// Fallback scan when history lacks the line.
	os.Remove(s.HistoryPath())
	if a, _, err := openDir(t, filepath.Dir(s.RunsDir()), Options{}).LastAttempt("lint"); err != nil || a.AttemptID != first.AttemptID {
		t.Fatalf("fallback: %+v %v", a, err)
	}
	// Pruned run dirs are skipped.
	record(t, r2, first) // re-add to history (run r2 finished; RecordAttempt still writes)
	os.RemoveAll(r1.Dir())
	os.RemoveAll(r2.Dir())
	if _, _, err := s.LastAttempt("lint"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned: %v", err)
	}
}

func TestEstimate(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	r := newRun(t, s, RunMeta{})
	q := EstimateQuery{Task: "lint", Variant: "full", ScopeBucket: "all", ConfigKey: "cfg1", Machine: "testhost", Workers: 4}

	e := s.Estimate(q)
	if e.Duration != 0 || e.Confidence != "none" || e.Samples != 0 {
		t.Fatalf("none: %+v", e)
	}

	record(t, r, att("lint", 10*time.Second, Passed), att("lint", 20*time.Second, Findings))
	e = s.Estimate(q)
	if e.Duration != 25*time.Second || e.Confidence != "low" || e.Samples != 2 || strings.Contains(e.Source, "p90") {
		t.Fatalf("low: %+v", e)
	}

	// Censored sample raises the estimate to its lower bound.
	to := att("lint", 30*time.Second, TimedOut)
	to.Censored = true
	record(t, r, to)
	e = s.Estimate(q)
	if e.Duration != 30*time.Second || e.LowerBound != 30*time.Second || e.Confidence != "low" || e.Samples != 2 {
		t.Fatalf("censored: %+v", e)
	}

	for i := 1; i <= 8; i++ {
		record(t, r, att("lint", time.Duration(i)*time.Second, Passed))
	}
	// 10 samples: 1..8, 10, 20 -> p90 (nearest rank 9) = 10s -> 11s. The
	// timeout is older than these completions, so it no longer bounds the
	// estimate (caches warm, code changes).
	e = s.Estimate(q)
	if e.Confidence != "ok" || e.Samples != 10 || e.Duration != 11*time.Second || e.LowerBound != 0 || !strings.Contains(e.Source, "p90 of 10 samples (+10%)") {
		t.Fatalf("ok, superseded timeout: %+v", e)
	}
	// A timeout newer than every completion raises the estimate again.
	to2 := att("lint", 40*time.Second, TimedOut)
	to2.Censored = true
	record(t, r, to2)
	e = s.Estimate(q)
	if e.Duration != 40*time.Second || e.LowerBound != 40*time.Second {
		t.Fatalf("ok+recent censored: %+v", e)
	}

	// Only censored samples.
	s2 := openDir(t, t.TempDir(), Options{})
	r2 := newRun(t, s2, RunMeta{})
	record(t, r2, to)
	e = s2.Estimate(q)
	if e.Duration != 30*time.Second || e.Confidence != "low" || e.Samples != 0 {
		t.Fatalf("only censored: %+v", e)
	}
}

func TestEstimateOK(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	r := newRun(t, s, RunMeta{})
	for i := 1; i <= 10; i++ {
		record(t, r, att("lint", time.Duration(i)*time.Second, Passed))
	}
	// Non-comparable noise: other machine, variant, config, failures.
	other := att("lint", time.Hour, Passed)
	other.Machine = "elsewhere"
	variant := att("lint", time.Hour, Passed)
	variant.Variant = "fast"
	cfg := att("lint", time.Hour, Passed)
	cfg.ConfigKey = "cfg2"
	failed := att("lint", time.Hour, ToolError)
	record(t, r, other, variant, cfg, failed)

	q := EstimateQuery{Task: "lint", Variant: "full", ScopeBucket: "all", ConfigKey: "cfg1", Machine: "testhost", Workers: 4}
	e := s.Estimate(q)
	if e.Duration != 9900*time.Millisecond || e.Confidence != "ok" || e.Samples != 10 || e.LowerBound != 0 {
		t.Fatalf("ok: %+v", e)
	}
	// Empty ConfigKey matches any config.
	q.ConfigKey = ""
	if e := s.Estimate(q); e.Samples != 11 {
		t.Fatalf("any config: %+v", e)
	}
	// Machine defaults to this host, which has no samples.
	q.Machine = ""
	if host, _ := os.Hostname(); host != "testhost" {
		if e := s.Estimate(q); e.Confidence != "none" {
			t.Fatalf("this host: %+v", e)
		}
	}

	// Only the newest 20 samples count.
	s2 := openDir(t, t.TempDir(), Options{})
	r2 := newRun(t, s2, RunMeta{})
	for i := 0; i < 5; i++ {
		record(t, r2, att("lint", time.Hour, Passed))
	}
	for i := 0; i < 20; i++ {
		record(t, r2, att("lint", time.Second, Passed))
	}
	q.Machine = "testhost"
	if e := s2.Estimate(q); e.Samples != 20 || e.Duration != 1100*time.Millisecond {
		t.Fatalf("window: %+v", e)
	}
}

func TestEstimateRelaxation(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	r := newRun(t, s, RunMeta{})
	q := EstimateQuery{Task: "lint", Variant: "full", ScopeBucket: "all", Machine: "testhost", Workers: 4}

	exact := att("lint", 10*time.Second, Passed)
	record(t, r, exact)
	if e := s.Estimate(q); e.Confidence != "low" || e.Samples != 1 || strings.Contains(e.Source, "relaxed") {
		t.Fatalf("exact only: %+v", e)
	}
	// Samples with other worker counts: relax workers once exact < 3.
	for i := 0; i < 2; i++ {
		a := att("lint", 20*time.Second, Passed)
		a.Workers = 8
		record(t, r, a)
	}
	e := s.Estimate(q)
	if e.Confidence != "ok" || e.Samples != 3 || !strings.Contains(e.Source, "workers relaxed") || e.Duration != 22*time.Second {
		t.Fatalf("workers relaxed: %+v", e)
	}
	// A different scope bucket with no samples at that bucket relaxes scope too.
	q.ScopeBucket = "1"
	e = s.Estimate(q)
	if e.Confidence != "ok" || !strings.Contains(e.Source, "workers and scope relaxed") {
		t.Fatalf("scope relaxed: %+v", e)
	}
	// Once enough exact samples exist, no relaxation.
	q.ScopeBucket = "all"
	record(t, r, att("lint", 10*time.Second, Passed), att("lint", 10*time.Second, Passed))
	e = s.Estimate(q)
	if e.Samples != 3 || e.Duration != 11*time.Second || strings.Contains(e.Source, "relaxed") {
		t.Fatalf("exact: %+v", e)
	}
	// Task is never relaxed.
	q.Task = "other"
	if e := s.Estimate(q); e.Confidence != "none" {
		t.Fatalf("other task: %+v", e)
	}
}

func TestEstimateContended(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	r := newRun(t, s, RunMeta{})
	q := EstimateQuery{Task: "lint", Variant: "full", ScopeBucket: "all", Machine: "testhost", Workers: 4}
	contended := func(d time.Duration) Attempt {
		a := att("lint", d, Passed)
		a.Contended = true
		return a
	}
	record(t, r, att("lint", 10*time.Second, Passed), att("lint", 10*time.Second, Passed))
	for i := 0; i < 5; i++ {
		record(t, r, contended(100*time.Second))
	}
	// Fewer than 3 uncontended: use everything.
	e := s.Estimate(q)
	if e.Samples != 7 || e.Duration != 110*time.Second || !strings.Contains(e.Source, "contended") {
		t.Fatalf("mixed: %+v", e)
	}
	record(t, r, att("lint", 10*time.Second, Passed))
	e = s.Estimate(q)
	if e.Samples != 3 || e.Duration != 11*time.Second || strings.Contains(e.Source, "contended") {
		t.Fatalf("prefer uncontended: %+v", e)
	}
}

func TestStats(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	r := newRun(t, s, RunMeta{})
	for i := 1; i <= 10; i++ {
		record(t, r, att("lint", time.Duration(i)*time.Second, Passed))
	}
	to := att("lint", time.Hour, TimedOut)
	to.Censored = true
	record(t, r, to, Attempt{AttemptID: "d", Task: "lint", Variant: "full", Outcome: Deferred},
		att("test", 3*time.Second, Findings))
	st, err := s.Stats(Query{})
	if err != nil || len(st) != 2 {
		t.Fatalf("stats: %v %+v", err, st)
	}
	l := st[0]
	if l.Task != "lint" || l.Attempts != 12 || l.Counts[Passed] != 10 || l.Counts[TimedOut] != 1 || l.Counts[Deferred] != 1 ||
		l.P50 != 5*time.Second || l.P90 != 9*time.Second || !l.LastRun.Equal(to.End) {
		t.Fatalf("lint stats: %+v", l)
	}
	if st[1].Task != "test" || st[1].P50 != 3*time.Second || st[1].P90 != 0 {
		t.Fatalf("test stats (no p90 under 3 samples): %+v", st[1])
	}
	if st, _ := s.Stats(Query{Task: "test"}); len(st) != 1 {
		t.Fatalf("filtered: %+v", st)
	}
}

func finishedRun(t *testing.T, s *Store, started time.Time, payload int) *Run {
	t.Helper()
	r := newRun(t, s, RunMeta{Started: started})
	if payload > 0 {
		if err := os.WriteFile(filepath.Join(r.AttemptDir("x"), "output.bytes"), make([]byte, payload), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Finish(RunSummary{}); err != nil {
		t.Fatal(err)
	}
	return r
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneCountAndActive(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir, Options{Retention: Retention{MaxRuns: 3, MaxAge: -1, MaxBytes: -1, MinFree: -1}})
	other := openDir(t, dir, Options{})
	now := time.Now()
	// The oldest run is live, held by another Store instance.
	active := newRun(t, other, RunMeta{Started: now.Add(-10 * time.Hour)})
	var runs []*Run
	for i := 5; i >= 1; i-- {
		runs = append(runs, finishedRun(t, s, now.Add(-time.Duration(i)*time.Hour), 0))
	}
	n, err := s.Prune()
	if err != nil || n != 3 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if !exists(active.Dir()) {
		t.Fatal("active run removed")
	}
	for i, r := range runs {
		if want := i >= 3; exists(r.Dir()) != want {
			t.Fatalf("run %d exists=%v", i, !want)
		}
	}
	// Once finished, the formerly active run is prunable.
	active.Finish(RunSummary{})
	if n, err := s.Prune(); err != nil || n != 0 {
		t.Fatalf("prune 2: %d %v", n, err) // 3 runs remain == MaxRuns
	}
	finishedRun(t, s, now, 0)
	if n, err := s.Prune(); err != nil || n != 1 || exists(active.Dir()) {
		t.Fatalf("prune 3: %d %v", n, err)
	}
	if ents, _ := os.ReadDir(s.RunsDir()); len(ents) != 3 {
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestPruneAgeBytesFree(t *testing.T) {
	now := time.Now()
	t.Run("age", func(t *testing.T) {
		s := openDir(t, t.TempDir(), Options{Retention: Retention{MaxRuns: -1, MaxAge: 24 * time.Hour, MaxBytes: -1, MinFree: -1}})
		old := finishedRun(t, s, now.Add(-48*time.Hour), 0)
		oldActive := newRun(t, s, RunMeta{Started: now.Add(-72 * time.Hour)})
		fresh := finishedRun(t, s, now.Add(-time.Hour), 0)
		broken := filepath.Join(s.RunsDir(), "20200101T000000Z-broken")
		os.Mkdir(broken, 0o755)
		os.Chtimes(broken, now.Add(-49*time.Hour), now.Add(-49*time.Hour))
		if n, err := s.Prune(); err != nil || n != 2 || exists(old.Dir()) || exists(broken) || !exists(fresh.Dir()) || !exists(oldActive.Dir()) {
			t.Fatalf("age prune: %d %v", n, err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		s := openDir(t, t.TempDir(), Options{Retention: Retention{MaxRuns: -1, MaxAge: -1, MaxBytes: 2_500_000, MinFree: -1}})
		var runs []*Run
		for i := 4; i >= 1; i-- {
			runs = append(runs, finishedRun(t, s, now.Add(-time.Duration(i)*time.Hour), 1_000_000))
		}
		if n, err := s.Prune(); err != nil || n != 2 || exists(runs[1].Dir()) || !exists(runs[2].Dir()) {
			t.Fatalf("bytes prune: %d %v", n, err)
		}
	})
	t.Run("minfree", func(t *testing.T) {
		s := openDir(t, t.TempDir(), Options{Retention: Retention{MaxRuns: -1, MaxAge: -1, MaxBytes: -1, MinFree: 1 << 62}})
		finishedRun(t, s, now.Add(-2*time.Hour), 10)
		finishedRun(t, s, now.Add(-time.Hour), 10)
		live := newRun(t, s, RunMeta{})
		if n, err := s.Prune(); err != nil || n != 2 || !exists(live.Dir()) {
			t.Fatalf("minfree prune: %d %v", n, err)
		}
	})
	t.Run("defaults keep recent runs", func(t *testing.T) {
		s, err := Open("", Options{Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		r := finishedRun(t, s, now, 10)
		free, _ := freeBytes(s.RunsDir())
		if n, err := s.Prune(); err != nil || (free > DefaultMinFree && (n != 0 || !exists(r.Dir()))) {
			t.Fatalf("default prune: %d %v", n, err)
		}
	})
}

func TestConcurrentRecordGoroutines(t *testing.T) {
	s := openDir(t, t.TempDir(), Options{})
	r := newRun(t, s, RunMeta{})
	const workers, each = 32, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				a := Attempt{AttemptID: fmt.Sprintf("w%d-%d", w, i), Task: "t", Outcome: Passed,
					ChangedFiles: []string{strings.Repeat("x", 3000)}}
				if err := r.RecordAttempt(a); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	checkHistory(t, s.HistoryPath(), workers*each)
	_, atts, err := s.LoadRun(r.ID())
	if err != nil || len(atts) != workers*each {
		t.Fatalf("meta files: %v %d", err, len(atts))
	}
}

// checkHistory verifies every line is intact JSON and IDs are unique.
func checkHistory(t *testing.T, path string, want int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	seen := map[string]bool{}
	for sc.Scan() {
		var aj attemptJSON
		if err := json.Unmarshal(sc.Bytes(), &aj); err != nil {
			t.Fatalf("corrupt line %q: %v", sc.Text(), err)
		}
		key := aj.RunID + "/" + aj.AttemptID
		if seen[key] {
			t.Fatalf("duplicate %s", key)
		}
		seen[key] = true
	}
	if len(seen) != want {
		t.Fatalf("%d lines, want %d", len(seen), want)
	}
}

const helperEnv = "LAP_STORE_TEST_HELPER_DIR"

func TestHelperProcessAppend(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	s, err := Open("", Options{Dir: dir, Retention: noPrune})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.NewRun(RunMeta{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		a := Attempt{AttemptID: fmt.Sprint(i), Task: "t", Outcome: Passed, ChangedFiles: []string{strings.Repeat("y", 9000)}}
		if err := r.RecordAttempt(a); err != nil {
			t.Fatal(err)
		}
	}
	r.Finish(RunSummary{})
}

func TestConcurrentRecordProcesses(t *testing.T) {
	dir := t.TempDir()
	var cmds []*exec.Cmd
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessAppend$", "-test.count=1")
		cmd.Env = append(os.Environ(), helperEnv+"="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	s := openDir(t, dir, Options{})
	checkHistory(t, s.HistoryPath(), 600)
	if runs, _ := s.Runs(0); len(runs) != 2 || runs[0].Status != StatusCompleted {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestScopeBucketFor(t *testing.T) {
	cases := map[string]string{}
	for _, c := range []struct {
		all   bool
		files int
		want  string
	}{{true, 5, "all"}, {false, 0, "0"}, {false, 1, "1"}, {false, 2, "2-10"}, {false, 10, "2-10"},
		{false, 11, "11-100"}, {false, 100, "11-100"}, {false, 101, "101+"}} {
		if got := ScopeBucketFor(c.all, c.files); got != c.want {
			cases[fmt.Sprint(c.all, c.files)] = got
		}
	}
	if len(cases) > 0 {
		t.Fatal(cases)
	}
}
