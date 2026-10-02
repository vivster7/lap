//go:build linux

package proc

// Probes of Go and kernel behaviour that motivated the design. They assert
// what was observed so a toolchain/kernel change shows up as a failure.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Q1: Setsid+Setpgid fails in the child; Setsid alone gives pgid==sid==pid.
func TestProbeSetsidPlusSetpgid(t *testing.T) {
	cmd := exec.Command(helperArgv("ids")[0], "ids")
	cmd.Env = helperEnvList()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setpgid: true}
	out, err := cmd.Output()
	t.Logf("Setsid+Setpgid: err=%v (%T) out=%q", err, err, out)
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("want EPERM from setpgid after setsid, got %v", err)
	}
	var pe *os.PathError
	if !errors.As(err, &pe) || pe.Op != "fork/exec" {
		t.Errorf("want *os.PathError{Op: fork/exec}, got %#v", err)
	}

	for _, c := range []struct {
		name string
		attr *syscall.SysProcAttr
	}{
		{"Setsid", &syscall.SysProcAttr{Setsid: true}},
		{"Setpgid", &syscall.SysProcAttr{Setpgid: true}},
	} {
		cmd := exec.Command(helperArgv("ids")[0], "ids")
		cmd.Env = helperEnvList()
		cmd.SysProcAttr = c.attr
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var pid, pgid, sid int
		fmt.Sscanf(string(out), "ids %d %d %d", &pid, &pgid, &sid)
		t.Logf("%s: pid=%d pgid=%d sid=%d (test sid=%d)", c.name, pid, pgid, sid, mustSid())
		if pgid != pid {
			t.Errorf("%s: pgid %d != pid %d", c.name, pgid, pid)
		}
		if c.name == "Setsid" && sid != pid {
			t.Errorf("Setsid: sid %d != pid %d", sid, pid)
		}
		if c.name == "Setpgid" && sid != mustSid() {
			t.Errorf("Setpgid: should stay in our session")
		}
	}
}

func mustSid() int { s, _ := unix.Getsid(0); return s }

// startCmdTree runs plan via exec.Cmd in a new pgroup with stdout on a pipe
// and returns the reader.
func startCmdTree(t *testing.T, cmd *exec.Cmd) *reader {
	t.Helper()
	rd, wr, _ := os.Pipe()
	cmd.Env = helperEnvList()
	cmd.Stdout = wr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wr.Close()
	pgid := cmd.Process.Pid
	t.Cleanup(func() { unix.Kill(-pgid, syscall.SIGKILL); rd.Close() })
	return newReader(rd)
}

// Q2: CommandContext's default Cancel kills only the direct child.
func TestProbeCommandContextKillsOnlyDirectChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	argv := helperArgv("spawn", "{", "report=gc", "sleep", "}", "report=child", "wait")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	r := startCmdTree(t, cmd)
	gc := r.pid(t, "gc")
	r.pid(t, "child")
	cancel()
	err := cmd.Wait()
	t.Logf("Wait after cancel: %v", err)
	time.Sleep(100 * time.Millisecond)
	if !alive(gc) {
		t.Fatal("grandchild died; default Cancel killed more than the direct child?")
	}
	m, _ := readStat(gc)
	t.Logf("grandchild %d survives cancel: ppid=%d pgid=%d", gc, m.PPid, m.Pgid)
}

// Q2: a group-signalling Cancel works for cooperative trees, but WaitDelay's
// escalation is Process.Kill (direct child only): a TERM-ignoring
// grandchild survives even after WaitDelay.
func TestProbeCommandContextGroupCancel(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("gcIgnoresTERM=%v", ignore), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			gcPlan := []string{"report=gc", "sleep"}
			if ignore {
				gcPlan = append([]string{"ignore=TERM"}, gcPlan...)
			}
			plan := append(append([]string{"spawn", "{"}, gcPlan...), "}", "report=child", "wait")
			argv := helperArgv(plan...)
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
			cmd.WaitDelay = 200 * time.Millisecond
			r := startCmdTree(t, cmd)
			gc := r.pid(t, "gc")
			r.pid(t, "child")
			t0 := time.Now()
			cancel()
			err := cmd.Wait()
			t.Logf("Wait %v after cancel: %v", time.Since(t0), err)
			time.Sleep(300 * time.Millisecond) // past WaitDelay
			if alive(gc) != ignore {
				t.Fatalf("grandchild alive=%v, want %v", alive(gc), ignore)
			}
		})
	}
}

// Q3c with exec.Cmd: a non-*os.File Stdout makes Wait wait for the copying
// goroutine, i.e. for every holder of the pipe. Without WaitDelay Wait
// hangs while the grandchild lives; with WaitDelay it returns ErrWaitDelay
// but leaves the grandchild running. With an *os.File Stdout (what proc
// uses) Wait returns as soon as the direct child exits.
func TestProbeWaitDelayAndGrandchildHoldingStdout(t *testing.T) {
	argv := helperArgv("spawn", "{", "report=gc", "sleep", "}", "report=child", "exit")
	run := func(t *testing.T, stdout func(*exec.Cmd), waitDelay time.Duration) (*exec.Cmd, chan error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = helperEnvList()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = waitDelay
		stdout(cmd)
		// The grandchild is found by scanning the group; stderr is a pipe too.
		rd, wr, _ := os.Pipe()
		cmd.Stderr = wr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		wr.Close()
		pgid := cmd.Process.Pid
		t.Cleanup(func() { unix.Kill(-pgid, syscall.SIGKILL); rd.Close() })
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd, done
	}
	gcOf := func(t *testing.T, pgid int) int {
		var gc int
		waitFor(5*time.Second, func() bool {
			procs, _ := listProcs()
			for _, m := range procs {
				if m.Pgid == pgid && m.Pid != pgid && m.live() {
					gc = m.Pid
					return true
				}
			}
			return false
		})
		return gc
	}

	t.Run("buffer-no-waitdelay-hangs", func(t *testing.T) {
		var buf bytes.Buffer
		cmd, done := run(t, func(c *exec.Cmd) { c.Stdout = &buf }, 0)
		gc := gcOf(t, cmd.Process.Pid)
		select {
		case err := <-done:
			t.Fatalf("Wait returned early: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		t.Logf("Wait still blocked 500ms after child exit while gc %d holds stdout", gc)
		unix.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err := <-done; err != nil {
			t.Logf("Wait after killing gc: %v", err)
		}
	})
	t.Run("buffer-waitdelay", func(t *testing.T) {
		var buf bytes.Buffer
		t0 := time.Now()
		cmd, done := run(t, func(c *exec.Cmd) { c.Stdout = &buf }, 150*time.Millisecond)
		gc := gcOf(t, cmd.Process.Pid)
		err := <-done
		t.Logf("Wait returned %v after start: %v", time.Since(t0), err)
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("want ErrWaitDelay, got %v", err)
		}
		if !alive(gc) {
			t.Fatal("WaitDelay is not supposed to kill the grandchild")
		}
	})
	t.Run("osfile", func(t *testing.T) {
		rd, wr, _ := os.Pipe()
		defer rd.Close()
		t0 := time.Now()
		cmd, done := run(t, func(c *exec.Cmd) { c.Stdout = wr }, 0)
		wr.Close()
		gc := gcOf(t, cmd.Process.Pid)
		select {
		case err := <-done:
			t.Logf("Wait returned %v after start: %v (gc %d alive=%v)", time.Since(t0), err, gc, alive(gc))
		case <-time.After(5 * time.Second):
			t.Fatal("Wait blocked with *os.File stdout")
		}
	})
}

// Q7: Pdeathsig fires when the *thread* that forked exits. A goroutine
// that exits while locked to its thread terminates that thread, killing a
// child started with Pdeathsig from it.
func TestProbePdeathsigThreadExit(t *testing.T) {
	for _, unlock := range []bool{false, true} {
		t.Run(fmt.Sprintf("unlock=%v", unlock), func(t *testing.T) {
			ch := make(chan *Process)
			var tid int
			go func() {
				runtime.LockOSThread()
				if unlock {
					defer runtime.UnlockOSThread()
				}
				tid = unix.Gettid()
				spec := helperSpec("sleep")
				spec.Pdeathsig = syscall.SIGKILL
				p, err := Start(spec)
				if err != nil {
					panic(err)
				}
				ch <- p
			}()
			p := <-ch
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			ex, err := p.Wait(ctx)
			mainThread := tid == os.Getpid()
			t.Logf("unlock=%v: forked on tid %d (main thread=%v): wait=%v err=%v", unlock, tid, mainThread, ex, err)
			if !unlock && ex.Signal != syscall.SIGKILL {
				if mainThread {
					// The runtime wedges rather than exits the main thread
					// when a locked goroutine exits, so nothing fires.
					t.Logf("locked goroutine ran on the main thread; thread not exited, no Pdeathsig")
				} else {
					t.Errorf("expected Pdeathsig SIGKILL when the locked thread exited")
				}
			}
			if unlock && err == nil {
				t.Errorf("child should still be running")
			}
			terminate(t, p, 100*time.Millisecond)
		})
	}
}

// Runner killed: Pdeathsig takes the direct child down but grandchildren
// survive, still in the old pgroup (whose leader is gone).
func TestProbeRunnerKilledLeavesGrandchild(t *testing.T) {
	argv := helperArgv("runner", "{", "spawn", "{", "report=gc", "sleep", "}", "report=inner", "wait", "}")
	cmd := exec.Command(argv[0], argv[1:]...)
	r := startCmdTree(t, cmd)
	runner := r.pid(t, "runner")
	child := r.pid(t, "child")
	gc := r.pid(t, "gc")
	r.pid(t, "inner")
	t.Cleanup(func() { unix.Kill(-child, syscall.SIGKILL) })
	unix.Kill(runner, syscall.SIGKILL)
	cmd.Wait()
	if !waitFor(2*time.Second, func() bool { return !alive(child) }) {
		t.Fatal("direct child survived runner death despite Pdeathsig")
	}
	time.Sleep(100 * time.Millisecond)
	m, err := readStat(gc)
	if err != nil || !m.live() {
		t.Fatal("expected the grandchild to survive the runner")
	}
	t.Logf("runner %d killed: child %d gone (pdeathsig), grandchild %d alive ppid=%d pgid=%d (leader gone; group id still pinned by members)",
		runner, child, gc, m.PPid, m.Pgid)
	if err := unix.Kill(-child, 0); err != nil {
		t.Errorf("orphaned group should still be addressable: %v", err)
	}
}

// Q3d: with PR_SET_CHILD_SUBREAPER on the supervisor, a setsid escapee
// whose parent died is reparented to the supervisor, so it remains
// discoverable by lineage. Run in a subprocess because the attribute is
// process-wide.
func TestProbeSubreaperKeepsLineage(t *testing.T) {
	argv := helperArgv("subreaper", "{", "spawn", "{", "setsid", "report=esc", "closeio", "sleep", "}", "sleepms=50", "exit", "}")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = helperEnvList()
	out, err := cmd.Output()
	t.Logf("%s", bytes.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "result ") {
			if !strings.Contains(sc.Text(), "reparented=true") {
				t.Fatal("escapee not reparented to the subreaper")
			}
			if !strings.Contains(sc.Text(), "terminate_found=false") {
				t.Fatal("Terminate lineage unexpectedly found it; update SPIKE.md")
			}
			return
		}
	}
	t.Fatal("no result line")
}

// Runner killed, PTY mode, no Pdeathsig: the kernel closes the master,
// hangs up the slave and SIGHUPs the session. Cooperative trees die; a
// HUP-ignoring grandchild survives.
func TestProbeRunnerKilledPTYHangup(t *testing.T) {
	for _, ignoreHUP := range []bool{false, true} {
		t.Run(fmt.Sprintf("gcIgnoresHUP=%v", ignoreHUP), func(t *testing.T) {
			gcPlan := []string{"report=gc", "sleep"}
			if ignoreHUP {
				gcPlan = append([]string{"ignore=HUP"}, gcPlan...)
			}
			plan := append([]string{"runnerpty", "{", "spawn", "{"}, gcPlan...)
			plan = append(plan, "}", "report=inner", "wait", "}")
			argv := helperArgv(plan...)
			cmd := exec.Command(argv[0], argv[1:]...)
			r := startCmdTree(t, cmd)
			runner := r.pid(t, "runner")
			child := r.pid(t, "child")
			gc := r.pid(t, "gc")
			r.pid(t, "inner")
			t.Cleanup(func() { unix.Kill(-child, syscall.SIGKILL) })
			unix.Kill(runner, syscall.SIGKILL)
			cmd.Wait()
			childDied := waitFor(2*time.Second, func() bool { return !alive(child) })
			time.Sleep(100 * time.Millisecond)
			t.Logf("after runner SIGKILL: child dead=%v gc alive=%v", childDied, alive(gc))
			if !childDied {
				t.Fatal("session leader survived master close")
			}
			if alive(gc) != ignoreHUP {
				t.Fatalf("gc alive=%v, want %v", alive(gc), ignoreHUP)
			}
		})
	}
}
