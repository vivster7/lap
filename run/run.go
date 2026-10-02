// Package run composes proc (process lifetime) and term (output capture)
// into a single supervised command execution.
//
// The sequence follows the contracts in proc/SPIKE.md and term/SPIKE.md:
//
//  1. open the capture session and launch the child with the session's
//     launch attributes (new session + ctty for PTY modes, new process
//     group for pipes);
//  2. close the parent's copies of the child stdio, so hangup/EOF can arrive;
//  3. wait for the direct child to exit or the deadline;
//  4. give output a short tail grace to drain (a descendant may still hold
//     the terminal or pipe);
//  5. terminate and verify the owned group/session, which also reaps the
//     child;
//  6. finish draining within the cleanup allowance and close the session.
package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vivster7/lap/proc"
	"github.com/vivster7/lap/term"
)

// Command is one supervised execution.
type Command struct {
	// Argv[0] is resolved against PATH from Env.
	Argv []string
	Dir  string
	// Env is the complete child environment. Nil means os.Environ().
	// Entries required by the capture session (e.g. TERM) override it.
	Env []string

	// Capture configures output capture; Capture.Dir is required.
	Capture term.Spec

	// TailGrace is how long to wait for output to drain after the direct
	// child exits before terminating whatever still holds it. Default 200ms.
	TailGrace time.Duration
	// TermGrace is the time between SIGTERM and SIGKILL. Default 500ms.
	TermGrace time.Duration
	// Cleanup bounds termination, verification and final draining together.
	// Default 2s. It is spent after ctx is done, so callers enforcing a
	// total deadline must reserve it inside their budget.
	Cleanup time.Duration

	// Escapes selects how processes that left the owned group are handled.
	Escapes proc.EscapePolicy
}

// Result describes what happened. Execution, cleanup and capture are
// reported separately; a zero exit code does not imply a complete capture
// or a verified cleanup.
type Result struct {
	// Exit is the direct child's exit. Valid when Exited is true or when
	// Cleanup reaped the child.
	Exit proc.ExitInfo
	// Exited is true when the direct child exited before ctx was done.
	Exited bool
	// TimedOut is true when ctx ended before the direct child exited.
	TimedOut bool
	// TailHeld is true when output was still held open after the child
	// exited and the tail grace expired (a descendant kept it open).
	TailHeld bool

	Cleanup proc.CleanupReport
	Capture term.Summary
	// CaptureErr wraps term.ErrIncomplete when output is missing.
	CaptureErr error

	Elapsed time.Duration
}

// OK reports a clean, fully observed run: exited 0 before the deadline,
// verified cleanup and a complete capture.
func (r Result) OK() bool {
	return r.Exited && r.Exit.Code == 0 && r.Cleanup.Verified &&
		len(r.Cleanup.EscapedLive) == 0 && r.CaptureErr == nil
}

func (c *Command) defaults() {
	if c.TailGrace == 0 {
		c.TailGrace = 200 * time.Millisecond
	}
	if c.TermGrace == 0 {
		c.TermGrace = 500 * time.Millisecond
	}
	if c.Cleanup == 0 {
		c.Cleanup = 2 * time.Second
	}
}

// Run executes c under ctx. It returns an error only when the command could
// not be started or captured at all; outcomes of a started command are in
// Result.
func Run(ctx context.Context, c Command) (Result, error) {
	c.defaults()
	start := time.Now()
	var res Result
	if len(c.Argv) == 0 {
		return res, errors.New("run: empty argv")
	}

	s, err := term.Open(c.Capture)
	if err != nil {
		return res, fmt.Errorf("run: open capture: %w", err)
	}
	defer s.Close()

	stdin, stdout, stderr := s.ChildStdio()
	attrs := s.LaunchAttrs()
	spec := proc.Spec{
		Argv:    c.Argv,
		Dir:     c.Dir,
		Env:     mergeEnv(c.Env, s.Env()),
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		Session: proc.NewGroup,
		Escapes: c.Escapes,
	}
	if attrs.Setsid {
		spec.Session = proc.NewSessionWithCtty
		spec.CttyFD = attrs.CttyFD
	}

	p, startErr := proc.Start(spec)
	// Close the parent's copies of the child ends whether or not the start
	// succeeded; otherwise the readers never see EOF/hangup.
	closeErr := s.CloseChildEnds()
	if startErr != nil {
		dctx, cancel := context.WithTimeout(context.Background(), c.Cleanup)
		defer cancel()
		_, _ = s.Done(dctx)
		return res, fmt.Errorf("run: start: %w", startErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("run: close child stdio: %w", closeErr)
	}

	exit, waitErr := p.Wait(ctx)
	if waitErr == nil {
		res.Exited = true
		res.Exit = exit
		t := time.NewTimer(c.TailGrace)
		select {
		case <-s.Drained():
		case <-t.C:
			res.TailHeld = true
		}
		t.Stop()
	} else {
		res.TimedOut = ctx.Err() != nil
	}

	// Cleanup runs on a fresh context: the caller's ctx may already be done.
	cctx, cancel := context.WithTimeout(context.Background(), c.Cleanup)
	defer cancel()
	res.Cleanup = p.Terminate(cctx, c.TermGrace)
	if res.Cleanup.Reaped {
		res.Exit = res.Cleanup.Exit
	}

	res.Capture, res.CaptureErr = s.Done(cctx)
	if res.CaptureErr == nil && closeErr != nil {
		res.CaptureErr = closeErr
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// mergeEnv returns base (or os.Environ() when nil) with every key in extra
// replaced, so the child never sees duplicate entries for those keys.
func mergeEnv(base, extra []string) []string {
	if base == nil {
		base = os.Environ()
	}
	if len(extra) == 0 {
		return base
	}
	override := make(map[string]bool, len(extra))
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		override[k] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !override[k] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}
