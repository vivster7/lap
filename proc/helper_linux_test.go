//go:build linux

package proc

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func platformHelper(name string, inner []string) {
	switch name {
	case "runner":
		// A supervisor that will be SIGKILLed by the test. Its child shares
		// the runner's stdout so the test sees the grandchildren's reports.
		spec := Spec{Argv: helperArgv(inner...), Env: os.Environ(), Stdout: os.Stdout, Stderr: os.Stderr, Pdeathsig: syscall.SIGKILL}
		p, err := Start(spec)
		if err != nil {
			say("runner-failed %v", err)
			os.Exit(3)
		}
		say("runner %d", os.Getpid())
		say("child %d", p.Pid())
		time.Sleep(30 * time.Second)
	case "runnerpty":
		// Like runner, but PTY mode and no Pdeathsig: the workload's output
		// is relayed from the master to our stdout.
		master, slave, err := pty.Open()
		if err != nil {
			say("runnerpty-failed %v", err)
			os.Exit(3)
		}
		p, err := Start(Spec{Argv: helperArgv(inner...), Env: os.Environ(),
			Stdin: slave, Stdout: slave, Stderr: slave, Session: NewSessionWithCtty})
		slave.Close()
		if err != nil {
			say("runnerpty-failed %v", err)
			os.Exit(3)
		}
		say("runner %d", os.Getpid())
		say("child %d", p.Pid())
		go io.Copy(os.Stdout, master)
		time.Sleep(30 * time.Second)
	case "subreaper":
		subreaperProbe(inner)
	}
}

// subreaperProbe runs inner (which must report "esc" and let the direct
// child exit) with this process as child subreaper, then reports what
// lineage looks like afterwards.
func subreaperProbe(inner []string) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		say("subreaper-failed %v", err)
		os.Exit(3)
	}
	rd, wr, _ := os.Pipe()
	p, err := Start(Spec{Argv: helperArgv(inner...), Env: os.Environ(), Stdout: wr, Stderr: wr})
	wr.Close()
	if err != nil {
		say("subreaper-failed %v", err)
		os.Exit(3)
	}
	var esc int
	if _, err := fmt.Fscanf(rd, "esc %d\n", &esc); err != nil {
		say("subreaper-failed read: %v", err)
		os.Exit(3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.Wait(ctx)
	r := p.Terminate(ctx, 100*time.Millisecond)
	m, err := readStat(esc)
	reparented := err == nil && m.PPid == os.Getpid()
	foundByTerminate := false
	for _, e := range r.Escaped {
		foundByTerminate = foundByTerminate || e.Pid == esc
	}
	say("result esc=%d ppid=%d self=%d reparented=%v terminate_found=%v", esc, m.PPid, os.Getpid(), reparented, foundByTerminate)
	if err == nil {
		unix.Kill(esc, syscall.SIGKILL)
		var ws unix.WaitStatus
		unix.Wait4(esc, &ws, 0, nil) // we are its parent now: reap it
	}
	os.Exit(0)
}
