package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrNoMeta reports a lock file that holds no metadata.
var ErrNoMeta = errors.New("pool: lock has no metadata")

// Lock is a simple exclusive flock on one file, used for the per-worktree run
// lock and for serialising history appends. Like leases it is released by
// closing its descriptor, never LOCK_UN, and the descriptor is close-on-exec.
// The lock file itself is never removed.
type Lock struct {
	mu     sync.Mutex
	f      *os.File
	closed bool
}

// TryLock takes an exclusive lock on path without blocking, creating the file
// (and its parent directories, mode 0700) if needed. It returns ErrBusy if the
// lock is held. If meta is non-nil it is written into the file as JSON for
// diagnostics (see ReadLockMeta); otherwise the file is truncated.
func TryLock(path string, meta any) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	locked, err := tryFlock(f)
	if err != nil {
		closeFile(f)
		return nil, fmt.Errorf("pool: lock %s: %w", path, err)
	}
	if !locked {
		closeFile(f)
		return nil, fmt.Errorf("%w: %s is locked", ErrBusy, path)
	}
	l := &Lock{f: f}
	if err := l.SetMeta(meta); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// LockWait takes an exclusive lock on path, polling until it is acquired or
// ctx is done (returning ctx.Err()). Any previous metadata is cleared.
func LockWait(ctx context.Context, path string) (*Lock, error) {
	f, err := lockPoll(ctx, path, 20*time.Millisecond)
	if err != nil {
		return nil, err
	}
	l := &Lock{f: f}
	if err := l.SetMeta(nil); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// SetMeta replaces the lock file's diagnostic metadata with meta as JSON
// (nil clears it).
func (l *Lock) SetMeta(meta any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("pool: lock closed")
	}
	var b []byte
	if meta != nil {
		var err error
		if b, err = json.Marshal(meta); err != nil {
			return fmt.Errorf("pool: lock metadata: %w", err)
		}
	}
	if err := writeMeta(l.f, b); err != nil {
		return fmt.Errorf("pool: write lock metadata: %w", err)
	}
	return nil
}

// Path returns the lock file path.
func (l *Lock) Path() string { return l.f.Name() }

// Close releases the lock by closing its descriptor. It is idempotent.
func (l *Lock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.f.Close()
}

// ReadLockMeta decodes the JSON metadata stored in the lock file at path into
// v. It does not take the lock; metadata may belong to a previous holder if
// the lock is free (or be mid-write). It returns ErrNoMeta for an empty file.
func ReadLockMeta(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	if len(b) == 0 {
		return fmt.Errorf("%w: %s", ErrNoMeta, path)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("pool: lock metadata %s: %w", path, err)
	}
	return nil
}

func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	return f, nil
}

// lockPoll opens path and polls a nonblocking flock until it succeeds or ctx
// is done, backing off exponentially up to maxDelay. Polling (rather than a
// blocking flock) keeps it cancellable.
func lockPoll(ctx context.Context, path string, maxDelay time.Duration) (*os.File, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	delay := min(50*time.Microsecond, maxDelay)
	for {
		locked, err := tryFlock(f)
		if err != nil {
			closeFile(f)
			return nil, fmt.Errorf("pool: lock %s: %w", path, err)
		}
		if locked {
			return f, nil
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			closeFile(f)
			return nil, ctx.Err()
		case <-t.C:
		}
		delay = min(delay*2, maxDelay)
	}
}
