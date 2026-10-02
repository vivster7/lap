//go:build linux

package proc

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

func TestParseStatHostileComm(t *testing.T) {
	line := "4242 (a) b (c)) S 1 4242 4242 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 987654 1000 10 18446744073709551615\n"
	m, err := parseStat([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if m.Pid != 4242 || m.Comm != "a) b (c)" || m.State != 'S' || m.PPid != 1 || m.Pgid != 4242 || m.Sid != 4242 || m.Start != 987654 {
		t.Fatalf("got %+v", m)
	}
}

// PID-reuse guard: a member whose start time does not match is not signalled.
func TestSignalMemberRejectsStaleIdentity(t *testing.T) {
	tr := startPipe(t, helperSpec("report=child", "sleep"), "child")
	m, err := readStat(tr.p.Pid())
	if err != nil {
		t.Fatal(err)
	}
	stale := m
	stale.Start++
	if err := signalMember(stale, syscall.SIGKILL); !errors.Is(err, errStale) {
		t.Fatalf("want errStale, got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !alive(m.Pid) {
		t.Fatal("stale identity was signalled")
	}
	if err := signalMember(m, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	r := terminate(t, tr.p, time.Second)
	if r.Exit.Signal != syscall.SIGTERM {
		t.Fatalf("exit %v", r.Exit)
	}
}

// Spec.Env's PATH is used, not the supervisor's (exec.Command's pitfall).
func TestLookPathUsesSpecEnv(t *testing.T) {
	dir := t.TempDir()
	exe, _ := os.Executable()
	if err := os.Symlink(exe, filepath.Join(dir, "lap-helper")); err != nil {
		t.Fatal(err)
	}
	spec := helperSpec("report=child", "exit")
	spec.Argv[0] = "lap-helper"
	spec.Env = append(spec.Env, "PATH="+dir)
	tr := startPipe(t, spec, "child")
	terminate(t, tr.p, time.Second)

	spec.Env = []string{"PATH=/nonexistent"}
	if _, err := Start(spec); err == nil {
		t.Fatal("expected lookup failure")
	}
}

func TestStartRejectsBadSpec(t *testing.T) {
	for _, s := range []Spec{
		{},
		{Argv: []string{"/bin/true"}, Session: 7},
		{Argv: []string{"/bin/true"}, Session: NewSessionWithCtty}, // no tty
	} {
		if _, err := Start(s); !errors.Is(err, ErrBadSpec) {
			t.Errorf("%+v: want ErrBadSpec, got %v", s, err)
		}
	}
	if _, err := Start(Spec{Argv: []string{"/nonexistent/x"}}); err == nil {
		t.Error("want exec error")
	}
}

func stats(ds []time.Duration) (p50, max time.Duration) {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2], s[len(s)-1]
}

// Q5: cleanup latency. Logged, loosely asserted; see SPIKE.md for numbers
// (measure without -race for representative values).
func TestCleanupLatency(t *testing.T) {
	const n = 10
	var scan, holder []time.Duration
	var nprocs int
	for i := 0; i < n; i++ {
		t0 := time.Now()
		procs, _ := listProcs()
		scan = append(scan, time.Since(t0))
		nprocs = len(procs)
		t0 = time.Now()
		findHolders(procs, []fileID{"pipe:[1]"})
		holder = append(holder, time.Since(t0))
	}
	a, b := stats(scan)
	c, d := stats(holder)
	t.Logf("scan of %d procs: p50=%v max=%v; stdio-holder scan: p50=%v max=%v", nprocs, a, b, c, d)

	var termTotal, verifyOnly, killPhase, killTotal []time.Duration
	for i := 0; i < n; i++ {
		tr := startPipe(t, helperSpec("spawn", "{", "report=gc", "sleep", "}", "report=child", "wait"), "child", "gc")
		r := terminate(t, tr.p, time.Second)
		if !r.Verified || r.SentKill {
			t.Fatal("unexpected")
		}
		termTotal = append(termTotal, r.Elapsed())

		tr = startPipe(t, helperSpec("spawn", "{", "ignore=TERM", "report=gc", "sleep", "}", "report=child", "wait"), "child", "gc")
		r = terminate(t, tr.p, 20*time.Millisecond)
		if !r.Verified || !r.SentKill {
			t.Fatal("unexpected")
		}
		killPhase = append(killPhase, r.Done.Sub(r.KillAt))
		killTotal = append(killTotal, r.Elapsed())

		tr = startPipe(t, helperSpec("report=child", "exit"), "child")
		<-tr.p.Exited()
		r = terminate(t, tr.p, time.Second)
		verifyOnly = append(verifyOnly, r.Elapsed())
	}
	for _, x := range []struct {
		name string
		ds   []time.Duration
	}{
		{"TERM-responsive tree: Terminate total", termTotal},
		{"TERM-ignoring gc (grace 20ms): KILL->done", killPhase},
		{"TERM-ignoring gc (grace 20ms): Terminate total", killTotal},
		{"already exited, verify+reap only", verifyOnly},
	} {
		p50, max := stats(x.ds)
		t.Logf("%-48s p50=%v max=%v", x.name, p50, max)
		if max > 3*time.Second {
			t.Errorf("%s: max %v", x.name, max)
		}
	}
}
