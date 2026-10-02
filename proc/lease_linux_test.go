//go:build linux

package proc

// Q6: lease ordering. Not a resource pool: just shows the order a
// supervisor must follow with a flock token and why it releases by close.

import (
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func lockToken(t *testing.T, path string) *os.File {
	t.Helper()
	// os.OpenFile sets O_CLOEXEC, so launched children never inherit it.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return f
}

// tryLock attempts the token through a separate open file description,
// which conflicts with other descriptions even within one process.
func tryLock(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil
}

func fdTargets(pid int) []string {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd/"
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if l, err := os.Readlink(dir + e.Name()); err == nil {
			out = append(out, l)
		}
	}
	return out
}

// Normal cancellation: the token becomes available only after Terminate has
// verified that no owned process is alive.
func TestLeaseReleasedOnlyAfterVerifiedCleanup(t *testing.T) {
	token := filepath.Join(t.TempDir(), "cpu-0")
	lease := lockToken(t, token)

	tr := startPipe(t, helperSpec("spawn", "{", "ignore=TERM", "report=gc", "sleep", "}", "report=child", "wait"), "child", "gc")
	gc := tr.pids["gc"]
	for _, pid := range []int{tr.p.Pid(), gc} {
		for _, l := range fdTargets(pid) {
			if l == token {
				t.Fatalf("pid %d inherited the lease fd", pid)
			}
		}
	}

	var acquiredAt atomic.Int64
	var gcAliveAtAcquire atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // contender: another run waiting for the token
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if tryLock(token) {
				gcAliveAtAcquire.Store(alive(gc))
				acquiredAt.Store(time.Now().UnixNano())
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(20 * time.Millisecond)
	r := terminate(t, tr.p, 50*time.Millisecond)
	if !r.Verified {
		t.Fatal("cleanup not verified; a real supervisor must keep the lease (or report it) here")
	}
	if acquiredAt.Load() != 0 {
		t.Fatal("token acquired before release")
	}
	lease.Close() // release = close, after verification
	released := time.Now()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatal("contender never acquired the token")
	}
	at := time.Unix(0, acquiredAt.Load())
	if at.Before(r.Done) || gcAliveAtAcquire.Load() {
		t.Fatalf("token acquired at %v before verified cleanup at %v (gc alive=%v)", at, r.Done, gcAliveAtAcquire.Load())
	}
	t.Logf("verified %v after TERM; contender acquired %v after close", r.Done.Sub(r.TermAt), at.Sub(released))
}

// Why close and not LOCK_UN: if a lock descriptor is duplicated into a
// child (it should never be; Go opens with O_CLOEXEC), close keeps the lock
// held until the last duplicate goes away, while LOCK_UN drops it for every
// duplicate at once although the child still runs.
func TestLeaseDuplicateCloseVersusUnlock(t *testing.T) {
	for _, unlock := range []bool{false, true} {
		name := "close"
		if unlock {
			name = "LOCK_UN"
		}
		t.Run(name, func(t *testing.T) {
			token := filepath.Join(t.TempDir(), "cpu-0")
			lease := lockToken(t, token)
			spec := helperSpec("report=child", "sleep")
			spec.Stdin = lease // deliberately leak the lock fd into the child
			tr := startPipe(t, spec, "child")
			if unlock {
				unix.Flock(int(lease.Fd()), unix.LOCK_UN)
			}
			lease.Close()
			got := tryLock(token)
			t.Logf("%s while child %d holds a duplicate: token free=%v", name, tr.p.Pid(), got)
			if got != unlock {
				t.Fatalf("token free=%v, want %v", got, unlock)
			}
			terminate(t, tr.p, time.Second)
			if !tryLock(token) {
				t.Fatal("token still held after the child exited")
			}
		})
	}
}
