package pool

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	mib       = int64(1) << 20
	gib       = int64(1) << 30
	helperEnv = "LAP_POOL_HELPER"
)

// The test binary doubles as a token holder in another process. With
// LAP_POOL_HELPER=1 and args "<dir> <cpu> <memory> <task>" it acquires the
// request, prints "held <pid>" and sleeps until killed.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runHelper(args []string) int {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "helper: want dir cpu memory task")
		return 2
	}
	cpu, _ := strconv.Atoi(args[1])
	mem, _ := strconv.ParseInt(args[2], 10, 64)
	p, err := Open(Config{Dir: args[0]})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	l, err := p.TryAcquire(Request{CPU: cpu, Memory: mem, Holder: Holder{Task: args[3], RunID: "helper-run"}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	fmt.Printf("held %d\n", os.Getpid())
	time.Sleep(time.Minute)
	l.Close()
	return 0
}

// startHolder runs a helper process holding the request and waits until it
// reports the lease is held.
func startHolder(t *testing.T, dir string, cpu int, mem int64, task string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, dir, strconv.Itoa(cpu), strconv.FormatInt(mem, 10), task)
	cmd.Env = append(os.Environ(), helperEnv+"=1", "GORACE=atexit_sleep_ms=0")
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "held ") {
		t.Fatalf("helper did not acquire: %q %v", line, err)
	}
	return cmd
}

func openTest(t *testing.T, dir string, cpu int, mem int64) *Pool {
	t.Helper()
	p, err := Open(Config{Dir: dir, CPU: cpu, MemoryBytes: mem, Quantum: 512 * mib})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustAcquire(t *testing.T, p *Pool, cpu int, mem int64) *Lease {
	t.Helper()
	l, err := p.TryAcquire(Request{CPU: cpu, Memory: mem, Holder: Holder{Task: t.Name()}})
	if err != nil {
		t.Fatalf("TryAcquire(cpu=%d, mem=%d): %v", cpu, mem, err)
	}
	return l
}

func wantFree(t *testing.T, p *Pool, cpu int, mem int64) {
	t.Helper()
	if c, m := p.Free(); c != cpu || m != mem {
		t.Fatalf("Free() = (%d, %d), want (%d, %d)", c, m, cpu, mem)
	}
}

func TestOpenCreatesPrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "pool-v1")
	p := openTest(t, dir, 2, gib)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", st.Mode().Perm())
	}
	if p.Dir() != dir {
		t.Fatalf("Dir() = %q", p.Dir())
	}
	if _, err := os.Stat(filepath.Join(dir, "pool.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	if got := DefaultDir(); got != "/run/user/1234/lap/pool-v1" {
		t.Fatalf("DefaultDir() = %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	want := filepath.Join(os.TempDir(), fmt.Sprintf("lap-%d", os.Getuid()), "pool-v1")
	if got := DefaultDir(); got != want {
		t.Fatalf("DefaultDir() = %q, want %q", got, want)
	}
}

func TestCanonicalSize(t *testing.T) {
	dir := t.TempDir()
	p := openTest(t, dir, 4, 2*gib)
	if c, m := p.Size(); c != 4 || m != 2*gib {
		t.Fatalf("Size() = (%d, %d)", c, m)
	}
	if p.Quantum() != 512*mib {
		t.Fatalf("Quantum() = %d", p.Quantum())
	}

	// A later Open asking for something else gets the canonical pool.
	p2, err := Open(Config{Dir: dir, CPU: 16, MemoryBytes: 64 * gib, Quantum: 256 * mib})
	if err != nil {
		t.Fatal(err)
	}
	if c, m := p2.Size(); c != 4 || m != 2*gib || p2.Quantum() != 512*mib {
		t.Fatalf("second Open: Size() = (%d, %d) quantum %d; want canonical (4, 2GiB, 512MiB)", c, m, p2.Quantum())
	}
	// Env overrides apply only at creation.
	t.Setenv("LAP_POOL_CPU", "7")
	if c, _ := openTest(t, dir, 0, 0).Size(); c != 4 {
		t.Fatalf("env override resized existing pool to %d", c)
	}

	// Reset refuses while a token is held, then recreates.
	l := mustAcquire(t, p, 1, 0)
	if err := Reset(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("Reset with held token: %v, want ErrBusy", err)
	}
	l.Close()
	if err := Reset(dir); err != nil {
		t.Fatal(err)
	}
	if c, _ := openTest(t, dir, 8, 2*gib).Size(); c != 7 {
		t.Fatalf("after Reset with LAP_POOL_CPU=7: cpu = %d", c)
	}
}

func TestConcurrentFirstOpenAgrees(t *testing.T) {
	dir := t.TempDir()
	const n = 24
	sizes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			p, err := Open(Config{Dir: dir, CPU: i + 1, MemoryBytes: int64(i+1) * gib})
			if err != nil {
				t.Error(err)
				return
			}
			sizes[i], _ = p.Size()
		})
	}
	wg.Wait()
	for i := range sizes {
		if sizes[i] != sizes[0] {
			t.Fatalf("concurrent opens disagree: %v", sizes)
		}
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".pool-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestEnvOverridesAtCreation(t *testing.T) {
	t.Setenv("LAP_POOL_CPU", "3")
	t.Setenv("LAP_POOL_MEMORY", "1536M")
	p := openTest(t, t.TempDir(), 10, 10*gib)
	if c, m := p.Size(); c != 3 || m != 1536*mib {
		t.Fatalf("Size() = (%d, %d), want (3, 1536MiB)", c, m)
	}
	t.Setenv("LAP_POOL_CPU", "zero")
	if _, err := Open(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("invalid LAP_POOL_CPU accepted")
	}
}

func TestMemoryRounding(t *testing.T) {
	// Pool memory rounds down to whole quanta (at least one).
	p := openTest(t, t.TempDir(), 2, 2*gib+100*mib)
	if _, m := p.Size(); m != 2*gib {
		t.Fatalf("pool memory = %d, want 2GiB", m)
	}
	if _, m := openTest(t, t.TempDir(), 1, 1).Size(); m != 512*mib {
		t.Fatalf("tiny pool memory = %d, want one quantum", m)
	}
	// Requests round up.
	for _, tc := range []struct{ req, want int64 }{
		{1, 512 * mib},
		{512 * mib, 512 * mib},
		{512*mib + 1, gib},
		{1536 * mib, 1536 * mib},
	} {
		l := mustAcquire(t, p, 0, tc.req)
		if l.Memory() != tc.want || len(l.Tokens()) != int(tc.want/(512*mib)) {
			t.Errorf("request %d: Memory() = %d tokens %v, want %d", tc.req, l.Memory(), l.Tokens(), tc.want)
		}
		l.Close()
	}
}

func TestEmptyAndInvalidRequests(t *testing.T) {
	p := openTest(t, t.TempDir(), 2, gib)
	l := mustAcquire(t, p, 0, 0)
	if l.CPU() != 0 || l.Memory() != 0 || len(l.Tokens()) != 0 {
		t.Fatalf("empty lease holds %v", l.Tokens())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.TryAcquire(Request{CPU: -1}); err == nil {
		t.Fatal("negative request accepted")
	}
	if _, err := p.TryAcquire(Request{CPU: 3}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("cpu too large: %v", err)
	}
	if _, err := p.TryAcquire(Request{Memory: gib + 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("memory too large: %v", err)
	}
	wantFree(t, p, 2, gib)
}

func TestAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	a := openTest(t, dir, 4, 2*gib)
	b := openTest(t, dir, 4, 2*gib) // second Pool, same directory

	la := mustAcquire(t, a, 3, 512*mib)
	if got := la.Tokens(); strings.Join(got, ",") != "cpu-0,cpu-1,cpu-2,mem-0" {
		t.Fatalf("lowest-index tokens not preferred: %v", got)
	}
	// CPU short: nothing may be held afterwards (memory would have fit).
	if _, err := b.TryAcquire(Request{CPU: 2, Memory: 512 * mib}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	// Memory short after CPU fit: the CPU token must be released.
	if _, err := b.TryAcquire(Request{CPU: 1, Memory: 2 * gib}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	wantFree(t, a, 1, 1536*mib)

	lb := mustAcquire(t, b, 1, 1536*mib)
	wantFree(t, a, 0, 0)
	if _, err := a.TryAcquire(Request{CPU: 1}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	la.Close()
	lb.Close()
	wantFree(t, b, 4, 2*gib)
}

func TestCloseReleasesAndIsIdempotent(t *testing.T) {
	p := openTest(t, t.TempDir(), 2, gib)
	l := mustAcquire(t, p, 2, gib)
	wantFree(t, p, 0, 0)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	wantFree(t, p, 2, gib)
	mustAcquire(t, p, 2, gib).Close()
}

// TestNoOversubscription hammers two Pool instances on one directory and
// checks that the total held never exceeds the pool size.
func TestNoOversubscription(t *testing.T) {
	dir := t.TempDir()
	pools := []*Pool{openTest(t, dir, 6, 3*gib), openTest(t, dir, 6, 3*gib)}
	var cpuInUse, memInUse atomic.Int64
	var wins, busies atomic.Int64
	deadline := time.Now().Add(400 * time.Millisecond)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 1))
			p := pools[w%2]
			for time.Now().Before(deadline) {
				cpu, mem := r.IntN(4), int64(r.IntN(4))*512*mib
				l, err := p.TryAcquire(Request{CPU: cpu, Memory: mem})
				if errors.Is(err, ErrBusy) {
					busies.Add(1)
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				wins.Add(1)
				if c := cpuInUse.Add(int64(l.CPU())); c > 6 {
					t.Errorf("cpu oversubscribed: %d > 6", c)
				}
				if m := memInUse.Add(l.Memory()); m > 3*gib {
					t.Errorf("memory oversubscribed: %d > 3GiB", m)
				}
				time.Sleep(time.Duration(r.IntN(2000)) * time.Microsecond)
				cpuInUse.Add(-int64(l.CPU()))
				memInUse.Add(-l.Memory())
				l.Close()
			}
		})
	}
	wg.Wait()
	t.Logf("%d acquisitions, %d busy", wins.Load(), busies.Load())
	if wins.Load() == 0 || busies.Load() == 0 {
		t.Fatalf("test did not exercise contention: wins=%d busy=%d", wins.Load(), busies.Load())
	}
	wantFree(t, pools[0], 6, 3*gib)
}

func TestHolders(t *testing.T) {
	p := openTest(t, t.TempDir(), 4, 2*gib)
	if hs, err := p.Holders(); err != nil || len(hs) != 0 {
		t.Fatalf("Holders() on idle pool = %v, %v", hs, err)
	}
	before := time.Now().Add(-time.Second)
	l, err := p.TryAcquire(Request{CPU: 1, Memory: 600 * mib, Holder: Holder{Worktree: "/w", Task: "lint", RunID: "r1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	hs, err := p.Holders()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range hs {
		names = append(names, h.Token)
		if h.Holder.PID != os.Getpid() || h.Holder.Task != "lint" || h.Holder.Worktree != "/w" || h.Holder.RunID != "r1" {
			t.Errorf("%s: holder %+v", h.Token, h.Holder)
		}
		if h.Since.Before(before) || h.Since.After(time.Now()) {
			t.Errorf("%s: since %v", h.Token, h.Since)
		}
		if h.PIDStart == 0 {
			t.Errorf("%s: no PIDStart", h.Token)
		}
	}
	if strings.Join(names, ",") != "cpu-0,mem-0,mem-1" {
		t.Fatalf("held tokens = %v", names)
	}
	// Probing must not disturb the holder or leave tokens locked.
	wantFree(t, p, 3, gib)
}

func TestSubprocessHolderAndSIGKILL(t *testing.T) {
	dir := t.TempDir()
	p := openTest(t, dir, 4, 2*gib)
	cmd := startHolder(t, dir, 3, gib, "sub")

	if _, err := p.TryAcquire(Request{CPU: 2}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy while subprocess holds 3/4 cpu, got %v", err)
	}
	if _, err := p.TryAcquire(Request{CPU: 1, Memory: 1536 * mib}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy for memory, got %v", err)
	}
	wantFree(t, p, 1, gib) // the failed attempts left nothing held

	hs, err := p.Holders()
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 5 {
		t.Fatalf("Holders() = %d tokens, want 5: %+v", len(hs), hs)
	}
	for _, h := range hs {
		if h.Holder.PID != cmd.Process.Pid || h.Holder.Task != "sub" {
			t.Errorf("%s: holder %+v, want pid %d", h.Token, h.Holder, cmd.Process.Pid)
		}
	}
	if err := Reset(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("Reset while subprocess holds: %v", err)
	}

	// SIGKILL: the kernel closes the holder's descriptors, so the tokens are
	// free at once. This is the documented crash gap: had the holder started
	// workloads, they could still be running now.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	mustAcquire(t, p, 4, 2*gib).Close()
}

func TestTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "run.lock")
	type meta struct {
		PID  int    `json:"pid"`
		Task string `json:"task"`
	}
	l, err := TryLock(path, meta{PID: 42, Task: "check"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Path() != path {
		t.Fatalf("Path() = %q", l.Path())
	}
	if _, err := TryLock(path, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second TryLock: %v, want ErrBusy", err)
	}
	var m meta
	if err := ReadLockMeta(path, &m); err != nil || m.PID != 42 || m.Task != "check" {
		t.Fatalf("ReadLockMeta = %+v, %v", m, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := LockWait(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LockWait while held: %v", err)
	}

	// LockWait acquires once the holder closes.
	got := make(chan *Lock, 1)
	go func() {
		l2, err := LockWait(context.Background(), path)
		if err != nil {
			t.Error(err)
		}
		got <- l2
	}()
	time.Sleep(20 * time.Millisecond)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	var l2 *Lock
	select {
	case l2 = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("LockWait did not acquire after release")
	}
	if l2 == nil {
		t.FailNow()
	}
	// LockWait clears stale metadata.
	if err := ReadLockMeta(path, &m); !errors.Is(err, ErrNoMeta) {
		t.Fatalf("ReadLockMeta after LockWait: %v, want ErrNoMeta", err)
	}
	if err := l2.SetMeta(meta{PID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := ReadLockMeta(path, &m); err != nil || m.PID != 7 {
		t.Fatalf("ReadLockMeta = %+v, %v", m, err)
	}
	l2.Close()
	l3, err := TryLock(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	l3.Close()
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"1024": 1024, "8G": 8 * gib, "8g": 8 * gib, "512M": 512 * mib, "512MiB": 512 * mib,
		"2GB": 2 * gib, "64K": 64 << 10, "1T": 1 << 40, " 3G ": 3 * gib,
	} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "G", "-1", "1.5G", "abc", "99999999999T"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) accepted", in)
		}
	}
}

func TestDefaults(t *testing.T) {
	n := usableCPUs()
	c := DefaultCPU()
	if c < 1 || c > n {
		t.Fatalf("DefaultCPU() = %d with %d usable CPUs", c, n)
	}
	if m := DefaultMemory(); m <= 0 {
		t.Fatalf("DefaultMemory() = %d", m)
	}
	t.Logf("usable CPUs %d, default cpu %d, total memory %d, default memory %d", n, c, totalMemory(), DefaultMemory())
	p, err := Open(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if pc, pm := p.Size(); pc != c || pm <= 0 || pm%DefaultQuantum != 0 {
		t.Fatalf("default Size() = (%d, %d)", pc, pm)
	}
}
