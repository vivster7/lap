//go:build linux

package proc

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// (a) normal grandchild: TERM to the group is enough.
func TestCleanupNormalGrandchild(t *testing.T) {
	for _, mode := range []SessionMode{NewGroup, NewSessionWithCtty} {
		t.Run(mode.String(), func(t *testing.T) {
			spec := helperSpec("spawn", "{", "report=gc", "sleep", "}", "report=child", "wait")
			var tr *tree
			if mode == NewGroup {
				tr = startPipe(t, spec, "child", "gc")
			} else {
				tr = startPTY(t, spec, "child", "gc")
			}
			gc := tr.pids["gc"]
			r := terminate(t, tr.p, time.Second)
			if !r.Verified || !r.SentTerm || r.SentKill {
				t.Fatalf("want verified TERM-only cleanup")
			}
			if _, ok := hasMember(r.Members, gc); !ok {
				t.Errorf("grandchild %d not listed as member", gc)
			}
			if alive(gc) {
				t.Errorf("grandchild %d alive after verified cleanup", gc)
			}
			if !r.Reaped || r.Exit.Signal != syscall.SIGTERM || r.Exit.Rusage == nil {
				t.Errorf("exit = %+v reaped=%v", r.Exit, r.Reaped)
			}
		})
	}
}

// (b) grandchild ignores SIGTERM: needs SIGKILL escalation after grace.
// In PTY mode it must also ignore SIGHUP, because the session leader's death
// hangs up the terminal and SIGHUPs the foreground pgroup (see
// TestPTYTermIgnorerDiesOfHangup).
func TestCleanupTermIgnoringGrandchild(t *testing.T) {
	for _, mode := range []SessionMode{NewGroup, NewSessionWithCtty} {
		t.Run(mode.String(), func(t *testing.T) {
			spec := helperSpec("spawn", "{", "ignore=TERM", "ignore=HUP", "report=gc", "sleep", "}", "report=child", "wait")
			var tr *tree
			if mode == NewGroup {
				tr = startPipe(t, spec, "child", "gc")
			} else {
				tr = startPTY(t, spec, "child", "gc")
			}
			gc := tr.pids["gc"]
			grace := 100 * time.Millisecond
			r := terminate(t, tr.p, grace)
			if !r.Verified || !r.SentKill {
				t.Fatalf("want verified cleanup with KILL")
			}
			if d := r.KillAt.Sub(r.TermAt); d < grace {
				t.Errorf("KILL after %v, before grace %v", d, grace)
			}
			if alive(gc) {
				t.Errorf("grandchild alive")
			}
			t.Logf("TERM->KILL %v, KILL->verified %v", r.KillAt.Sub(r.TermAt), r.Done.Sub(r.KillAt))
		})
	}
}

// (c) direct child exits at once; grandchild keeps the stdout pipe open.
// Wait must return promptly (proc never reads output, so nothing blocks on
// the pipe); the reader sees EOF only after Terminate kills the grandchild.
func TestCleanupLeaderExitsGrandchildHoldsPipe(t *testing.T) {
	spec := helperSpec("spawn", "{", "report=gc", "sleep", "}", "report=child", "exit=7")
	tr := startPipe(t, spec, "child", "gc")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 := time.Now()
	ex, err := tr.p.Wait(ctx)
	if err != nil || ex.Code != 7 {
		t.Fatalf("Wait = %+v, %v", ex, err)
	}
	t.Logf("Wait returned %v after child report; exit=%v", time.Since(t0), ex)
	gc := tr.pids["gc"]
	if !alive(gc) {
		t.Fatal("grandchild should still be alive")
	}
	// Exit observed without reaping: the leader is a zombie that pins the
	// pgid until Terminate.
	if m, err := readStat(tr.p.Pid()); err != nil || m.State != 'Z' {
		t.Fatalf("leader should be an unreaped zombie: %v %v", m, err)
	}
	select {
	case <-tr.r.eof:
		t.Fatal("reader saw EOF while grandchild holds the pipe")
	case <-time.After(100 * time.Millisecond):
	}
	r := terminate(t, tr.p, time.Second)
	if !r.Verified || r.SentKill || r.Exit.Code != 7 || !r.Reaped {
		t.Fatalf("unexpected report")
	}
	select {
	case <-tr.r.eof:
		t.Logf("reader EOF %v after Terminate returned", time.Since(r.Done))
	case <-time.After(2 * time.Second):
		t.Fatal("reader never saw EOF after cleanup")
	}
}

// (d) grandchild calls setsid(2) and leaves the group and session.
func TestCleanupSetsidEscape(t *testing.T) {
	// d1: direct child still alive at Terminate -> lineage finds it.
	t.Run("lineage", func(t *testing.T) {
		spec := helperSpec("spawn", "{", "setsid", "report=esc", "sleep", "}", "report=child", "wait")
		tr := startPipe(t, spec, "child", "esc")
		esc := tr.pids["esc"]
		r := terminate(t, tr.p, time.Second)
		if !r.Verified {
			t.Fatal("owned group should be verified clean")
		}
		m, ok := hasMember(r.Escaped, esc)
		if !ok || m.Why != "descendant" {
			t.Fatalf("escapee %d not reported via lineage: %v", esc, r.Escaped)
		}
		if !alive(esc) {
			t.Fatal("EscapeReport must not kill the escapee")
		}
		if _, ok := hasMember(r.EscapedLive, esc); !ok {
			t.Fatal("live escapee missing from EscapedLive")
		}
	})
	// d2: direct child already gone (escapee orphaned): lineage is lost but
	// the escapee still holds our stdout pipe -> holder scan finds it.
	for _, mode := range []SessionMode{NewGroup, NewSessionWithCtty} {
		t.Run("holder-"+mode.String(), func(t *testing.T) {
			spec := helperSpec("spawn", "{", "setsid", "report=esc", "sleep", "}", "report=child", "sleepms=50", "exit")
			spec.Escapes = EscapeKill
			var tr *tree
			if mode == NewGroup {
				tr = startPipe(t, spec, "child", "esc")
			} else {
				tr = startPTY(t, spec, "child", "esc")
			}
			esc := tr.pids["esc"]
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tr.p.Wait(ctx)
			if m, _ := readStat(esc); m.Sid != esc || m.Pgid != esc {
				t.Fatalf("escapee not in its own session: %v", m)
			}
			r := terminate(t, tr.p, time.Second)
			m, ok := hasMember(r.Escaped, esc)
			if !ok || m.Why != "holds-stdio" {
				t.Fatalf("escapee not found via stdio: %v", r.Escaped)
			}
			if !r.Verified || alive(esc) {
				t.Fatalf("EscapeKill should have killed escapee %d", esc)
			}
		})
	}
	// d3: orphaned and closed its stdio: undetectable (documented gap).
	t.Run("undetectable", func(t *testing.T) {
		spec := helperSpec("spawn", "{", "setsid", "report=esc", "closeio", "sleep", "}", "report=child", "sleepms=50", "exit")
		spec.Escapes = EscapeKill
		tr := startPipe(t, spec, "child", "esc")
		esc := tr.pids["esc"]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tr.p.Wait(ctx)
		time.Sleep(20 * time.Millisecond) // let closeio happen
		r := terminate(t, tr.p, time.Second)
		if _, ok := hasMember(r.Escaped, esc); ok {
			t.Fatalf("unexpectedly detected; update SPIKE.md")
		}
		if !r.Verified || !alive(esc) {
			t.Fatalf("expected a false 'verified' with a live escapee (the known gap)")
		}
		m, _ := readStat(esc)
		t.Logf("GAP: escapee %d survives undetected, reparented to ppid %d", esc, m.PPid)
	})
}

// setpgid escape: leaves the pgroup but stays in the session. PTY mode's
// session ownership catches it; pipe mode only via lineage.
func TestCleanupSetpgidEscape(t *testing.T) {
	plan := []string{"spawn", "{", "setpgid", "report=esc", "sleep", "}", "report=child", "wait"}
	t.Run("pty-session-owned", func(t *testing.T) {
		tr := startPTY(t, helperSpec(plan...), "child", "esc")
		esc := tr.pids["esc"]
		r := terminate(t, tr.p, time.Second)
		m, ok := hasMember(r.Members, esc)
		if !ok || m.Pgid == tr.p.Pid() {
			t.Fatalf("want esc as owned member in another pgroup: %v", r.Members)
		}
		if !r.Verified || alive(esc) {
			t.Fatal("session-owned member survived")
		}
	})
	t.Run("pipe-lineage-only", func(t *testing.T) {
		tr := startPipe(t, helperSpec(plan...), "child", "esc")
		esc := tr.pids["esc"]
		r := terminate(t, tr.p, time.Second)
		if _, ok := hasMember(r.Escaped, esc); !ok {
			t.Fatalf("want esc as escapee: %v", r.Escaped)
		}
		if !alive(esc) {
			t.Fatal("EscapeReport should leave it")
		}
	})
}

// (e) double fork: the grandchild is orphaned (reparented away from the
// workload) but stays in the group, so group ownership still covers it.
func TestCleanupDoubleForkStaysInGroup(t *testing.T) {
	spec := helperSpec("spawn", "{", "spawn", "{", "report=gc", "sleep", "}", "exit", "}", "wait", "report=child", "sleep")
	tr := startPipe(t, spec, "child", "gc")
	gc, child := tr.pids["gc"], tr.pids["child"]
	m, _ := readStat(gc)
	if m.PPid == child || m.Pgid != child {
		t.Fatalf("want orphan in our group, got %v", m)
	}
	t.Logf("double-forked grandchild reparented to ppid %d (%s)", m.PPid, commOf(m.PPid))
	r := terminate(t, tr.p, time.Second)
	if !r.Verified || alive(gc) {
		t.Fatal("double-forked grandchild survived")
	}
}

func commOf(pid int) string {
	m, err := readStat(pid)
	if err != nil {
		return "?"
	}
	return m.Comm
}

// Stopped member: TERM stays pending until SIGCONT; Terminate sends both.
func TestCleanupStoppedGrandchild(t *testing.T) {
	spec := helperSpec("spawn", "{", "report=gc", "stop", "sleep", "}", "report=child", "wait")
	tr := startPipe(t, spec, "child", "gc")
	gc := tr.pids["gc"]
	waitFor(2*time.Second, func() bool { m, _ := readStat(gc); return m.State == 'T' })
	r := terminate(t, tr.p, 2*time.Second)
	if !r.Verified || r.SentKill || r.Elapsed() > time.Second {
		t.Fatalf("stopped member should die on TERM+CONT without KILL")
	}
}

// Zombies are dead work: an unreaped grandchild must not block verification.
func TestCleanupZombieGrandchild(t *testing.T) {
	spec := helperSpec("spawn", "{", "report=gc", "exit", "}", "report=child", "sleep")
	tr := startPipe(t, spec, "child", "gc")
	gc := tr.pids["gc"]
	if !waitFor(2*time.Second, func() bool { m, _ := readStat(gc); return m.State == 'Z' }) {
		t.Fatal("grandchild did not become a zombie")
	}
	r := terminate(t, tr.p, time.Second)
	if _, ok := hasMember(r.Members, gc); ok {
		t.Error("zombie listed as live member")
	}
	if !r.Verified || r.SentKill {
		t.Fatal("zombie should not need KILL")
	}
}

// Normal completion: nothing left, Terminate only verifies and reaps.
func TestCleanupAfterCleanExit(t *testing.T) {
	tr := startPipe(t, helperSpec("report=child", "exit=3"), "child")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr.p.Wait(ctx)
	r := terminate(t, tr.p, time.Second)
	if !r.Verified || r.SentTerm || r.Exit.Code != 3 || !r.Reaped {
		t.Fatal("unexpected report")
	}
	if r2 := tr.p.Terminate(ctx, 0); r2.Done != r.Done {
		t.Error("Terminate not idempotent")
	}
}

// PTY: when the session leader exits, the kernel hangs up the controlling
// terminal and SIGHUPs its foreground pgroup.
func TestPTYLeaderExitHangsUpForeground(t *testing.T) {
	t.Run("default-HUP", func(t *testing.T) {
		tr := startPTY(t, helperSpec("spawn", "{", "report=gc", "sleep", "}", "report=child", "sleepms=50", "exit"), "child", "gc")
		gc := tr.pids["gc"]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tr.p.Wait(ctx)
		if !waitFor(time.Second, func() bool { return !alive(gc) }) {
			t.Fatal("grandchild survived session-leader exit")
		}
		r := terminate(t, tr.p, time.Second)
		if !r.Verified || r.SentTerm {
			t.Fatal("nothing should be left to signal")
		}
	})
	t.Run("ignore-HUP", func(t *testing.T) {
		tr := startPTY(t, helperSpec("spawn", "{", "ignore=HUP", "report=gc", "sleep", "}", "report=child", "sleepms=50", "exit"), "child", "gc")
		gc := tr.pids["gc"]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tr.p.Wait(ctx)
		time.Sleep(50 * time.Millisecond)
		if !alive(gc) {
			t.Fatal("HUP-ignoring grandchild died")
		}
		r := terminate(t, tr.p, time.Second)
		if !r.Verified || alive(gc) {
			t.Fatal("session member survived")
		}
	})
}

// PTY: a grandchild that ignores only SIGTERM still dies without SIGKILL,
// because TERM kills the session leader, which SIGHUPs the foreground pgroup.
func TestPTYTermIgnorerDiesOfHangup(t *testing.T) {
	tr := startPTY(t, helperSpec("spawn", "{", "ignore=TERM", "report=gc", "sleep", "}", "report=child", "wait"), "child", "gc")
	r := terminate(t, tr.p, 2*time.Second)
	if !r.Verified || r.SentKill || alive(tr.pids["gc"]) {
		t.Fatal("expected death by SIGHUP without KILL")
	}
}
