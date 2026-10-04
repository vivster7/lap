//go:build unix

package pool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// tryFlock attempts flock(LOCK_EX|LOCK_NB) on f. It reports false (and no
// error) when the lock is held through another open file description.
func tryFlock(f *os.File) (bool, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

// ensurePrivateDir creates dir (mode 0700) and checks that it is a real
// directory owned by the current user, tightening its mode if needed. When
// dir is in the default temp-dir location, the lap-<uid> parent is checked
// too, since it lives in a shared directory.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	check := []string{dir}
	if parent := filepath.Dir(dir); filepath.Base(parent) == fmt.Sprintf("lap-%d", os.Getuid()) {
		check = append(check, parent)
	}
	for _, d := range check {
		st, err := os.Lstat(d)
		if err != nil {
			return fmt.Errorf("pool: %w", err)
		}
		if !st.IsDir() {
			return fmt.Errorf("pool: %s is not a directory", d)
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
			return fmt.Errorf("pool: %s is owned by uid %d, not %d", d, sys.Uid, os.Getuid())
		}
		if st.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(d, 0o700); err != nil {
				return fmt.Errorf("pool: %w", err)
			}
		}
	}
	return nil
}
