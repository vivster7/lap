package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleTmpAge is how old an unrenamed NewRun temp directory must be before
// Prune removes it.
const staleTmpAge = time.Hour

// Prune applies Retention to this worktree's runs, oldest first: a run is
// removed while there are more than MaxRuns runs, while it is older than
// MaxAge, while the runs total more than MaxBytes, or while the filesystem
// has less than MinFree available. Runs whose .active lock is held are never
// removed (they still count toward the limits). Removing a run makes its
// captures unavailable; its history lines remain. It returns the number of
// runs removed.
func (s *Store) Prune() (removed int, err error) {
	ret := s.retention
	runs, err := s.listRuns(true) // newest first
	if err != nil {
		return 0, err
	}
	s.removeStaleTemps()

	sizes := make([]int64, len(runs))
	var total int64
	for i, r := range runs {
		sizes[i] = dirSize(r.Dir)
		total += sizes[i]
	}
	free, freeErr := int64(-1), error(nil)
	if ret.MinFree > 0 {
		free, freeErr = freeBytes(s.runsDir)
	}
	count := len(runs)
	now := time.Now()
	var errs []error
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		over := (ret.MaxRuns > 0 && count > ret.MaxRuns) ||
			(ret.MaxAge > 0 && now.Sub(r.Started) > ret.MaxAge) ||
			(ret.MaxBytes > 0 && total > ret.MaxBytes) ||
			(ret.MinFree > 0 && freeErr == nil && free < ret.MinFree)
		if !over {
			continue
		}
		ok, err := removeRun(r.Dir)
		if err != nil {
			errs = append(errs, err)
		}
		if !ok {
			continue
		}
		removed++
		count--
		total -= sizes[i]
		if free >= 0 {
			free += sizes[i] // estimate; avoids a statfs per removal
		}
	}
	return removed, errors.Join(errs...)
}

// removeRun deletes a run directory unless its active lock is held. The
// exclusive lock is held during deletion so a reader probing the lock sees
// it busy rather than a half-deleted run.
func removeRun(dir string) (bool, error) {
	lock, err := lockFile(filepath.Join(dir, activeFile), true, 0)
	if err != nil {
		if errors.Is(err, errLocked) {
			return false, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // removed concurrently
		}
		return false, err
	}
	defer lock.Close()
	// Move aside first so the run disappears atomically from listings.
	trash := filepath.Join(filepath.Dir(dir), tmpPrefix+"rm-"+filepath.Base(dir))
	target := dir
	if os.Rename(dir, trash) == nil {
		target = trash
	}
	if err := os.RemoveAll(target); err != nil {
		return true, fmt.Errorf("store: removing run %s: %w", filepath.Base(dir), err)
	}
	return true, nil
}

// removeStaleTemps deletes leftovers from crashed NewRun or Prune calls.
func (s *Store) removeStaleTemps() {
	ents, err := os.ReadDir(s.runsDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), tmpPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleTmpAge {
			continue
		}
		dir := filepath.Join(s.runsDir, e.Name())
		if isActive(dir) {
			continue
		}
		os.RemoveAll(dir)
	}
}
