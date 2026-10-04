// Package store keeps lap's local record of runs: per-run metadata, plans and
// task-attempt metadata (with capture directories written by package term),
// plus a history index of attempts that the scheduler queries for duration
// estimates and that inspection commands query for statistics.
//
// # Layout
//
// In a Git repository the default layout is
//
//	<git-common-dir>/lap/history.jsonl          shared by every worktree
//	<git-common-dir>/lap/history.lock           flock serializing appends
//	<git-dir>/lap/runs/<run-id>/run.json        per worktree
//	<git-dir>/lap/runs/<run-id>/.active         flock held while the run is live
//	<git-dir>/lap/runs/<run-id>/plan.json
//	<git-dir>/lap/runs/<run-id>/tasks/<attempt-id>/meta.json
//	<git-dir>/lap/runs/<run-id>/tasks/<attempt-id>/capture/   (written by term)
//
// Options.Dir replaces both roots (history at Dir/history.jsonl, runs at
// Dir/runs) for non-Git callers and tests.
//
// # Durability
//
// run.json, meta.json and plan.json are written atomically (temp file +
// rename) without fsync: a crash leaves either the old or the new version,
// but a power loss may lose recent records. Recording stays inside the
// run's deadline, so no unbounded fsync is performed. Each history record is
// one write(2) of one line under an exclusive flock; readers stop at the
// last newline, so a record torn by a crash is skipped (and a later append
// starts on a fresh line). Per-run records (meta.json) are sufficient to
// rebuild the history index.
//
// # History reading
//
// The history file is read once per Store and cached; later queries read
// only the bytes appended since (or reload if the file was replaced or
// shrank). The first read is limited to the last Options.HistoryTailBytes
// (default 16 MiB, on the order of 30k attempts), so startup cost stays
// bounded however long the file grows. Older lines are invisible to
// History, Stats and Estimate; estimates only use the 20 most recent
// comparable samples anyway.
package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned (possibly wrapped) when a run or attempt does not exist.
var ErrNotFound = errors.New("store: not found")

// Options configures Open.
type Options struct {
	// Dir overrides both locations (non-git use / tests): history at
	// Dir/history.jsonl, runs at Dir/runs.
	Dir string
	// Retention bounds this worktree's runs; zero fields take defaults.
	Retention Retention
	// HistoryTailBytes limits the initial history read to the file's last
	// N bytes. 0 means DefaultHistoryTailBytes; negative reads the whole file.
	HistoryTailBytes int64
}

// DefaultHistoryTailBytes is the default initial history read window.
const DefaultHistoryTailBytes = 16 << 20

// Retention bounds the runs kept for one worktree. A zero field takes its
// default; a negative field disables that limit.
type Retention struct {
	MaxRuns  int           // default 200
	MaxAge   time.Duration // default 30 days
	MaxBytes int64         // default 5 GiB across this worktree's runs
	MinFree  int64         // default 2 GiB: prune oldest until filesystem free space >= MinFree (best effort)
}

// Retention defaults.
const (
	DefaultMaxRuns  = 200
	DefaultMaxAge   = 30 * 24 * time.Hour
	DefaultMaxBytes = 5 << 30
	DefaultMinFree  = 2 << 30
)

func (r Retention) withDefaults() Retention {
	if r.MaxRuns == 0 {
		r.MaxRuns = DefaultMaxRuns
	}
	if r.MaxAge == 0 {
		r.MaxAge = DefaultMaxAge
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = DefaultMaxBytes
	}
	if r.MinFree == 0 {
		r.MinFree = DefaultMinFree
	}
	return r
}

// Store is a handle on one worktree's runs and the shared history. It is
// safe for concurrent use by multiple goroutines; multiple Stores (in one or
// several processes) may share the same files.
type Store struct {
	historyPath string
	lockPath    string
	runsDir     string
	worktree    string
	hostname    string
	retention   Retention
	tailBytes   int64

	mu   sync.Mutex // guards hist
	hist histCache
}

// Open resolves the store locations for the repository containing repoRoot
// and creates the directories. With opt.Dir set, Git is not consulted.
func Open(repoRoot string, opt Options) (*Store, error) {
	s := &Store{retention: opt.Retention.withDefaults(), tailBytes: opt.HistoryTailBytes}
	if s.tailBytes == 0 {
		s.tailBytes = DefaultHistoryTailBytes
	}
	s.hostname, _ = os.Hostname()
	if opt.Dir != "" {
		dir, err := filepath.Abs(opt.Dir)
		if err != nil {
			return nil, err
		}
		s.historyPath = filepath.Join(dir, "history.jsonl")
		s.runsDir = filepath.Join(dir, "runs")
		if repoRoot != "" {
			if s.worktree, err = filepath.Abs(repoRoot); err != nil {
				return nil, err
			}
		}
	} else {
		top, gitDir, commonDir, err := gitDirs(repoRoot)
		if err != nil {
			return nil, err
		}
		s.worktree = top
		s.historyPath = filepath.Join(commonDir, "lap", "history.jsonl")
		s.runsDir = filepath.Join(gitDir, "lap", "runs")
	}
	s.lockPath = strings.TrimSuffix(s.historyPath, ".jsonl") + ".lock"
	if err := os.MkdirAll(filepath.Dir(s.historyPath), 0o755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := os.MkdirAll(s.runsDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return s, nil
}

// gitDirs returns the worktree toplevel, its git dir and the common git dir.
func gitDirs(repoRoot string) (top, gitDir, commonDir string, err error) {
	if repoRoot == "" {
		repoRoot = "."
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", "", "", err
	}
	cmd := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("store: resolving git dirs in %s: %w: %s", root, err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 {
		return "", "", "", fmt.Errorf("store: unexpected git rev-parse output %q", out)
	}
	top, gitDir, commonDir = lines[0], lines[1], lines[2]
	if !filepath.IsAbs(commonDir) {
		// Relative output is relative to the directory git ran in.
		commonDir = filepath.Join(root, commonDir)
	}
	return filepath.Clean(top), filepath.Clean(gitDir), filepath.Clean(commonDir), nil
}

// HistoryPath is the shared history index file.
func (s *Store) HistoryPath() string { return s.historyPath }

// RunsDir is this worktree's run directory root.
func (s *Store) RunsDir() string { return s.runsDir }

// Worktree is the worktree toplevel this Store was opened for ("" in Dir
// mode without a repoRoot).
func (s *Store) Worktree() string { return s.worktree }
