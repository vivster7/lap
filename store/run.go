package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run status values stored in run.json.
const (
	StatusStarted   = "started"
	StatusCompleted = "completed"
	StatusAbandoned = "abandoned"
	// StatusIncomplete is reported (never stored) for a run whose run.json
	// still says "started" but whose active lock is no longer held: the
	// process died without finishing it.
	StatusIncomplete = "incomplete"
)

// Record format versions.
const (
	runVersion     = 1
	attemptVersion = 1
)

const (
	activeFile = ".active"
	runFile    = "run.json"
	planFile   = "plan.json"
	metaFile   = "meta.json"
	tasksDir   = "tasks"
	tmpPrefix  = ".tmp-"
)

// RunMeta describes a run at its start.
type RunMeta struct {
	ID         string        `json:"id"` // "" => generated: time-sortable, e.g. 20261003T142233Z-ab12cd
	Started    time.Time     `json:"started"`
	Worktree   string        `json:"worktree,omitempty"` // repo toplevel
	Commit     string        `json:"commit,omitempty"`
	Base       string        `json:"base,omitempty"`
	BaseRef    string        `json:"base_ref,omitempty"`
	ScopeAll   bool          `json:"scope_all,omitempty"`
	ScopeFiles int           `json:"scope_files,omitempty"`
	Budget     time.Duration `json:"budget_ns,omitempty"`
	Mode       string        `json:"mode,omitempty"` // "fix" | "check"
	Groups     []string      `json:"groups,omitempty"`
	Machine    Machine       `json:"machine"`
	Argv       []string      `json:"argv,omitempty"`
}

// RunSummary is recorded when a run finishes.
type RunSummary struct {
	Ended    time.Time       `json:"ended"`
	Elapsed  time.Duration   `json:"elapsed_ns"`
	Counts   map[Outcome]int `json:"counts,omitempty"`
	ExitCode int             `json:"exit_code"`
	Overrun  time.Duration   `json:"overrun_ns,omitempty"` // elapsed beyond budget, if any
	Notes    []string        `json:"notes,omitempty"`
}

// RunRecord is a run as read back from disk.
type RunRecord struct {
	RunMeta
	Status  string      // StatusStarted, StatusCompleted, StatusAbandoned or StatusIncomplete
	Summary *RunSummary // nil unless finished or abandoned
	Dir     string      // absolute run directory
	Reason  string      // Abandon reason
}

// runJSON is the on-disk run.json.
type runJSON struct {
	Version int `json:"version"`
	RunMeta
	Status  string      `json:"status"`
	Reason  string      `json:"reason,omitempty"`
	Summary *RunSummary `json:"summary,omitempty"`
}

// attemptJSON is the on-disk meta.json and history line.
type attemptJSON struct {
	Version int `json:"v"`
	Attempt
}

// Run is a live run being recorded. Its methods are safe for concurrent use.
type Run struct {
	s      *Store
	dir    string
	mu     sync.Mutex
	meta   RunMeta
	active *os.File // .active lock; nil once finished
	counts map[Outcome]int
}

// NewRun creates runs/<id>/run.json with status "started" and holds the
// run's active lock until Finish or Abandon, so Prune never deletes it.
// The directory is populated and locked under a temporary name and renamed
// into place, so it is never visible unlocked.
func (s *Store) NewRun(meta RunMeta) (*Run, error) {
	if meta.Started.IsZero() {
		meta.Started = time.Now()
	}
	if meta.Worktree == "" {
		meta.Worktree = s.worktree
	}
	if meta.Machine.Hostname == "" && meta.Machine.OS == "" {
		meta.Machine = DetectMachine()
	}
	generated := meta.ID == ""
	for try := 0; ; try++ {
		if generated {
			meta.ID = newRunID(meta.Started)
		}
		meta.ID = safeName(meta.ID)
		r, err := s.createRun(meta)
		if err == nil {
			return r, nil
		}
		if !generated || !errors.Is(err, fs.ErrExist) || try >= 5 {
			return nil, err
		}
	}
}

func (s *Store) createRun(meta RunMeta) (*Run, error) {
	final := filepath.Join(s.runsDir, meta.ID)
	if _, err := os.Lstat(final); err == nil {
		return nil, fmt.Errorf("store: run %s: %w", meta.ID, fs.ErrExist)
	}
	tmp, err := os.MkdirTemp(s.runsDir, tmpPrefix+meta.ID+"-")
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	fail := func(err error, lock *os.File) (*Run, error) {
		os.RemoveAll(tmp)
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	lock, err := lockFile(filepath.Join(tmp, activeFile), true, 0)
	if err != nil {
		return fail(fmt.Errorf("store: locking new run: %w", err), nil)
	}
	if err := writeJSONAtomic(filepath.Join(tmp, runFile), runJSON{Version: runVersion, RunMeta: meta, Status: StatusStarted}); err != nil {
		return fail(err, lock)
	}
	// rename(2) onto an existing non-empty directory fails, so a concurrent
	// creator of the same ID cannot be clobbered.
	if err := os.Rename(tmp, final); err != nil {
		if _, serr := os.Lstat(final); serr == nil {
			err = fmt.Errorf("store: run %s: %w", meta.ID, fs.ErrExist)
		}
		return fail(err, lock)
	}
	return &Run{s: s, dir: final, meta: meta, active: lock, counts: map[Outcome]int{}}, nil
}

func newRunID(t time.Time) string {
	var b [3]byte
	rand.Read(b[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// ID is the run ID.
func (r *Run) ID() string { return r.meta.ID }

// Dir is the run directory.
func (r *Run) Dir() string { return r.dir }

// Meta returns the run's metadata (with defaults filled in).
func (r *Run) Meta() RunMeta { return r.meta }

// AttemptDir returns runs/<id>/tasks/<attemptID>/, creating it.
func (r *Run) AttemptDir(attemptID string) string {
	d := filepath.Join(r.dir, tasksDir, safeName(attemptID))
	os.MkdirAll(d, 0o755)
	return d
}

// CaptureDir returns AttemptDir/capture. It is not created: term.Open
// creates it and requires it to be absent. The attempt dir is created.
func (r *Run) CaptureDir(attemptID string) string {
	return filepath.Join(r.AttemptDir(attemptID), "capture")
}

// SavePlan writes plan as runs/<id>/plan.json.
func (r *Run) SavePlan(plan any) error {
	return writeJSONAtomic(filepath.Join(r.dir, planFile), plan)
}

// RecordAttempt writes AttemptDir/meta.json and appends one line to the
// history index under the history lock. Empty RunID, Worktree, Machine and
// ScopeBucket are filled from the run; an absolute CaptureDir inside the run
// directory is made relative to it.
func (r *Run) RecordAttempt(a Attempt) error {
	if a.AttemptID == "" {
		return errors.New("store: RecordAttempt: empty AttemptID")
	}
	if a.RunID == "" {
		a.RunID = r.meta.ID
	}
	if a.Worktree == "" {
		a.Worktree = r.meta.Worktree
	}
	if a.Machine == "" {
		a.Machine = r.meta.Machine.Hostname
	}
	if a.ScopeBucket == "" {
		a.ScopeBucket = ScopeBucketFor(a.ScopeAll, a.ScopeFiles)
	}
	if filepath.IsAbs(a.CaptureDir) {
		if rel, err := filepath.Rel(r.dir, a.CaptureDir); err == nil && !strings.HasPrefix(rel, "..") {
			a.CaptureDir = rel
		}
	}
	rec := attemptJSON{Version: attemptVersion, Attempt: a}
	metaErr := writeJSONAtomic(filepath.Join(r.AttemptDir(a.AttemptID), metaFile), rec)
	histErr := r.s.appendHistory(rec)
	r.mu.Lock()
	r.counts[a.Outcome]++
	r.mu.Unlock()
	return errors.Join(metaErr, histErr)
}

// Finish records run.json status "completed" with the summary and releases
// the active lock. Zero Ended/Elapsed/Overrun and nil Counts are derived
// from the run's start, budget and recorded attempts.
func (r *Run) Finish(sum RunSummary) error {
	return r.close(StatusCompleted, "", sum)
}

// Abandon records run.json status "abandoned" and releases the active lock.
func (r *Run) Abandon(reason string) error {
	sum := RunSummary{}
	if reason != "" {
		sum.Notes = []string{reason}
	}
	return r.close(StatusAbandoned, reason, sum)
}

func (r *Run) close(status, reason string, sum RunSummary) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return fmt.Errorf("store: run %s already finished", r.meta.ID)
	}
	if sum.Ended.IsZero() {
		sum.Ended = time.Now()
	}
	if sum.Elapsed == 0 {
		sum.Elapsed = sum.Ended.Sub(r.meta.Started)
	}
	if sum.Overrun == 0 && r.meta.Budget > 0 && sum.Elapsed > r.meta.Budget {
		sum.Overrun = sum.Elapsed - r.meta.Budget
	}
	if sum.Counts == nil && len(r.counts) > 0 {
		sum.Counts = make(map[Outcome]int, len(r.counts))
		for k, v := range r.counts {
			sum.Counts[k] = v
		}
	}
	err := writeJSONAtomic(filepath.Join(r.dir, runFile), runJSON{
		Version: runVersion, RunMeta: r.meta, Status: status, Reason: reason, Summary: &sum,
	})
	r.active.Close()
	r.active = nil
	return err
}

// readRun reads one run directory.
func readRun(dir string) (RunRecord, error) {
	var rj runJSON
	if err := readJSON(filepath.Join(dir, runFile), &rj); err != nil {
		return RunRecord{}, err
	}
	rec := RunRecord{RunMeta: rj.RunMeta, Status: rj.Status, Summary: rj.Summary, Dir: dir, Reason: rj.Reason}
	if rec.ID == "" {
		rec.ID = filepath.Base(dir)
	}
	if rec.Status == StatusStarted && !isActive(dir) {
		rec.Status = StatusIncomplete
	}
	return rec, nil
}

// listRuns returns every readable run in this worktree, newest first. With
// broken, directories without a readable run.json are included too (Status
// "", Started = directory mtime) so Prune can reclaim them.
func (s *Store) listRuns(broken bool) ([]RunRecord, error) {
	ents, err := os.ReadDir(s.runsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	var runs []RunRecord
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rec, err := readRun(filepath.Join(s.runsDir, e.Name()))
		if err != nil {
			info, ierr := e.Info()
			if !broken || ierr != nil {
				continue // unreadable or being deleted
			}
			rec = RunRecord{RunMeta: RunMeta{ID: e.Name(), Started: info.ModTime()}, Dir: filepath.Join(s.runsDir, e.Name())}
		}
		runs = append(runs, rec)
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].Started.Equal(runs[j].Started) {
			return runs[i].Started.After(runs[j].Started)
		}
		return runs[i].ID > runs[j].ID
	})
	return runs, nil
}

// Runs lists this worktree's runs, newest first. limit <= 0 means all.
func (s *Store) Runs(limit int) ([]RunRecord, error) {
	runs, err := s.listRuns(false)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// LoadRun reads a run and its attempts (from each AttemptDir/meta.json,
// ordered by queue/start time). id "" or "last" means the newest run.
func (s *Store) LoadRun(id string) (RunRecord, []Attempt, error) {
	var rec RunRecord
	if id == "" || id == "last" {
		runs, err := s.Runs(1)
		if err != nil {
			return RunRecord{}, nil, err
		}
		if len(runs) == 0 {
			return RunRecord{}, nil, fmt.Errorf("store: no runs: %w", ErrNotFound)
		}
		rec = runs[0]
	} else {
		var err error
		rec, err = readRun(filepath.Join(s.runsDir, safeName(id)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return RunRecord{}, nil, fmt.Errorf("store: run %s: %w", id, ErrNotFound)
			}
			return RunRecord{}, nil, fmt.Errorf("store: run %s: %w", id, err)
		}
	}
	atts, err := readAttempts(rec.Dir)
	return rec, atts, err
}

func readAttempts(runDir string) ([]Attempt, error) {
	ents, err := os.ReadDir(filepath.Join(runDir, tasksDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	var atts []Attempt
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		var aj attemptJSON
		if err := readJSON(filepath.Join(runDir, tasksDir, e.Name(), metaFile), &aj); err != nil {
			continue // not recorded (yet)
		}
		atts = append(atts, aj.Attempt)
	}
	sort.SliceStable(atts, func(i, j int) bool {
		a, b := atts[i], atts[j]
		if !a.Queued.Equal(b.Queued) {
			return a.Queued.Before(b.Queued)
		}
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		return a.AttemptID < b.AttemptID
	})
	return atts, nil
}

// LastAttempt returns the newest attempt of task in this worktree that
// actually started, and its run directory. Attempts whose run directory has
// been pruned are skipped. The history index is consulted first; if it has
// no live match (e.g. the line is outside the tail window), run directories
// are scanned. Returns ErrNotFound if there is none.
func (s *Store) LastAttempt(task string) (Attempt, string, error) {
	atts, err := s.History(Query{Task: task})
	if err != nil {
		return Attempt{}, "", err
	}
	sort.SliceStable(atts, func(i, j int) bool { return atts[i].Start.After(atts[j].Start) })
	for _, a := range atts {
		if a.Start.IsZero() || a.RunID == "" {
			continue
		}
		if s.worktree != "" && a.Worktree != "" && a.Worktree != s.worktree {
			continue
		}
		runDir := filepath.Join(s.runsDir, safeName(a.RunID))
		var aj attemptJSON
		if err := readJSON(filepath.Join(runDir, tasksDir, safeName(a.AttemptID), metaFile), &aj); err != nil {
			if _, serr := os.Stat(runDir); serr != nil {
				continue // pruned
			}
			return a, runDir, nil
		}
		return aj.Attempt, runDir, nil
	}
	// Fallback: scan run directories, newest first.
	runs, err := s.listRuns(false)
	if err != nil {
		return Attempt{}, "", err
	}
	for _, rec := range runs {
		atts, _ := readAttempts(rec.Dir)
		var best *Attempt
		for i := range atts {
			a := &atts[i]
			if a.Task == task && !a.Start.IsZero() && (best == nil || a.Start.After(best.Start)) {
				best = a
			}
		}
		if best != nil {
			return *best, rec.Dir, nil
		}
	}
	return Attempt{}, "", fmt.Errorf("store: no started attempt of %q: %w", task, ErrNotFound)
}
