package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// writeJSONAtomic writes v as indented JSON to path via a temp file and rename.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encoding %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// safeName makes an ID usable as a single path element.
func safeName(id string) string {
	if id == "" || id == "." || id == ".." {
		return "_" + id
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, id)
}

// lockFile opens (creating if needed) path and takes a flock on it. The lock
// is released by closing the returned file (never LOCK_UN), so it follows
// the open file description. Files are close-on-exec. With wait > 0 the
// attempt is retried until wait elapses; otherwise it is nonblocking and
// returns errLocked if held.
func lockFile(path string, exclusive bool, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	deadline := time.Now().Add(wait)
	sleep := time.Millisecond
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err == unix.EINTR {
			continue
		}
		if err != unix.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("store: flock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, errLocked
		}
		time.Sleep(sleep)
		if sleep < 20*time.Millisecond {
			sleep *= 2
		}
	}
}

var errLocked = errors.New("store: lock held")

// isActive reports whether the run directory's .active lock is held by
// someone (a live Run).
func isActive(runDir string) bool {
	f, err := os.OpenFile(filepath.Join(runDir, activeFile), os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		if err == unix.EINTR {
			continue
		}
		return err == unix.EWOULDBLOCK
	}
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
