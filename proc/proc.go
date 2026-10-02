// Package proc launches and supervises one foreground workload: a direct
// child plus whatever it spawns into its process group (pipe capture) or
// session (PTY capture). It owns launch, lifetime, cancellation and cleanup
// verification. It never reads the workload's output; the term package hands
// it the child ends of stdio.
//
// Lifecycle: Start, then optionally Wait / Exited, then always Terminate.
// Terminate is the only call that reaps the direct child. Until then the
// child is kept as an unreaped zombie after it exits, which pins its PID (and
// therefore the process-group and session IDs equal to it) so that group
// signals can never hit a recycled ID.
package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SessionMode selects how the direct child is detached from the supervisor.
type SessionMode int

const (
	// NewGroup puts the child in a new process group (Setpgid). Use it for
	// pipe capture. Ownership is "pgid == child pid".
	NewGroup SessionMode = iota
	// NewSessionWithCtty makes the child a session leader with the PTY slave
	// as its controlling terminal (Setsid+Setctty, no Setpgid: setsid already
	// makes pgid == sid == pid). Ownership is "sid == child pid", which also
	// covers job-control pgroups the workload creates inside its session.
	NewSessionWithCtty
)

func (m SessionMode) String() string {
	switch m {
	case NewGroup:
		return "group"
	case NewSessionWithCtty:
		return "session"
	}
	return fmt.Sprintf("SessionMode(%d)", int(m))
}

// EscapePolicy controls what Terminate does with processes that are provably
// ours (descendants of the direct child, or holders of the child's stdio)
// but have left the owned group/session.
type EscapePolicy int

const (
	// EscapeReport lists escapees in the report and leaves them running.
	EscapeReport EscapePolicy = iota
	// EscapeKill signals escapees found by lineage or stdio holding, using
	// identity-checked per-process signals.
	EscapeKill
)

// Spec describes one launch.
type Spec struct {
	// Argv[0] is resolved against PATH taken from Env (not from the
	// supervisor's environment) unless it contains a slash.
	Argv []string
	Dir  string
	// Env is the complete child environment. Nil means os.Environ().
	Env []string

	// Child ends of stdio. Nil means /dev/null. Start does not take
	// ownership: the caller must close its copies after Start returns,
	// otherwise the reader will never see EOF/EIO.
	Stdin, Stdout, Stderr *os.File

	Session SessionMode
	// CttyFD is the child descriptor (0, 1 or 2) that is the PTY slave in
	// NewSessionWithCtty mode. Defaults to 0.
	CttyFD int

	// Pdeathsig (Linux only) is sent to the direct child when the OS thread
	// that started it exits. It does not reach grandchildren. See SPIKE.md.
	Pdeathsig syscall.Signal

	Escapes EscapePolicy
}

// ExitInfo describes the direct child's exit.
type ExitInfo struct {
	Pid    int
	At     time.Time      // when the supervisor observed the exit
	Code   int            // exit code, or -1 if killed by a signal
	Signal syscall.Signal // terminating signal, or 0
	// Resource usage of the direct child and the descendants it waited
	// for. Only set after reaping (Terminate). MaxRSS is the largest single
	// process, not the tree total.
	Rusage *Rusage
}

// Rusage is a portable subset of getrusage data.
type Rusage struct {
	User, System time.Duration
	MaxRSSBytes  int64
}

func (e ExitInfo) String() string {
	if e.Signal != 0 {
		return fmt.Sprintf("pid %d killed by %v", e.Pid, e.Signal)
	}
	return fmt.Sprintf("pid %d exited %d", e.Pid, e.Code)
}

// Member is a process observed during a scan. (Pid, Start) is the identity;
// Start is platform-specific (Linux: clock ticks since boot from
// /proc/<pid>/stat field 22; darwin: microseconds since the epoch).
type Member struct {
	Pid, PPid, Pgid, Sid int
	Start                uint64
	State                byte // Linux letter; 'Z' zombie, 'X' dead
	Comm                 string
	Why                  string // owned: "group"/"session"; escaped: "descendant"/"holds-stdio"
}

func (m Member) live() bool { return m.State != 'Z' && m.State != 'X' }

func (m Member) String() string {
	return fmt.Sprintf("%d(%s,%c,pgid=%d,sid=%d,ppid=%d,%s)", m.Pid, m.Comm, m.State, m.Pgid, m.Sid, m.PPid, m.Why)
}

type ident struct {
	pid   int
	start uint64
}

func (m Member) id() ident { return ident{m.Pid, m.Start} }

// CleanupReport is the outcome of Terminate.
type CleanupReport struct {
	Mode SessionMode
	ID   int // pgid (NewGroup) or sid (NewSessionWithCtty); equals the child pid

	Exit     ExitInfo
	ExitSeen bool // direct child exit observed
	Reaped   bool // direct child reaped (Exit.Rusage set)

	// Live owned members found by the first scan (excluding the direct
	// child), and escapees found at any point.
	Members []Member
	Escaped []Member
	// EscapedLive are escapees still alive at the final scan. Under
	// EscapeReport a non-empty list means containment is incomplete even
	// when Verified is true.
	EscapedLive []Member

	SentTerm, SentKill bool
	Started            time.Time
	TermAt, KillAt     time.Time // zero if not sent
	Done               time.Time

	// Verified is true when a final scan found no live owned process and,
	// under EscapeKill, no live escapee.
	Verified  bool
	Survivors []Member // live owned (or killable escaped) processes at the end
	// Unverifiable lists checks this platform could not perform.
	Unverifiable []string
	Err          error
}

// Elapsed is the total Terminate duration.
func (r *CleanupReport) Elapsed() time.Duration { return r.Done.Sub(r.Started) }

// Process is a started workload.
type Process struct {
	spec  Spec
	pid   int
	start uint64 // leader identity
	h     *handle

	exited  chan struct{}
	exit    ExitInfo
	waitErr error

	termOnce sync.Once
	report   CleanupReport

	holders []fileID // stdio identities for holder scans
}

// Pid returns the direct child's PID (also the pgid / sid).
func (p *Process) Pid() int { return p.pid }

// Exited is closed when the direct child has exited. The child is not reaped.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// Wait blocks until the direct child exits or ctx is done. It does not reap
// and says nothing about descendants; call Terminate afterwards.
func (p *Process) Wait(ctx context.Context) (ExitInfo, error) {
	select {
	case <-p.exited:
		return p.exit, p.waitErr
	case <-ctx.Done():
		return ExitInfo{}, ctx.Err()
	}
}

// ErrBadSpec reports an invalid Spec.
var ErrBadSpec = errors.New("proc: invalid spec")

// Start launches spec. Argument validation and PATH lookup happen here.
func Start(spec Spec) (*Process, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("%w: empty Argv", ErrBadSpec)
	}
	if spec.Session != NewGroup && spec.Session != NewSessionWithCtty {
		return nil, fmt.Errorf("%w: unknown session mode %d", ErrBadSpec, spec.Session)
	}
	if spec.CttyFD < 0 || spec.CttyFD > 2 {
		return nil, fmt.Errorf("%w: CttyFD must be 0, 1 or 2", ErrBadSpec)
	}
	if spec.Env == nil {
		spec.Env = os.Environ()
	}
	path, err := lookPath(spec.Argv[0], spec.Env, spec.Dir)
	if err != nil {
		return nil, err
	}

	var devnull *os.File
	files := [3]*os.File{spec.Stdin, spec.Stdout, spec.Stderr}
	for i, f := range files {
		if f != nil {
			continue
		}
		if devnull == nil {
			devnull, err = os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				return nil, err
			}
			defer devnull.Close()
		}
		files[i] = devnull
	}
	if spec.Session == NewSessionWithCtty && files[spec.CttyFD] == devnull {
		return nil, fmt.Errorf("%w: NewSessionWithCtty needs a terminal on fd %d", ErrBadSpec, spec.CttyFD)
	}

	p := &Process{spec: spec, exited: make(chan struct{})}
	// Outputs only: stdin is often shared (or /dev/null) and is not what
	// blocks draining. Requires the outputs to be exclusive to this task.
	p.holders = holderIDs(files[1:])
	if err := p.startOS(path, files); err != nil {
		return nil, &os.PathError{Op: "fork/exec", Path: path, Err: err}
	}
	go p.waitLoop()
	return p, nil
}

func (p *Process) waitLoop() {
	info, err := p.h.waitExitNoReap()
	info.Pid = p.pid
	info.At = time.Now()
	p.exit, p.waitErr = info, err
	close(p.exited)
}

func lookPath(file string, env []string, dir string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil // execve resolves relative paths after chdir(Dir)
	}
	pathEnv := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			pathEnv = kv[len("PATH="):]
		}
	}
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			d = "."
		}
		cand := filepath.Join(d, file)
		check := cand
		if !filepath.IsAbs(check) && dir != "" {
			check = filepath.Join(dir, check)
		}
		if fi, err := os.Stat(check); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			if !filepath.IsAbs(cand) {
				return check, nil
			}
			return cand, nil
		}
	}
	return "", &os.PathError{Op: "lookpath", Path: file, Err: errors.New("executable not found in Spec.Env PATH")}
}

// Terminate stops the workload and verifies cleanup. It must be called for
// every started Process, including after a normal exit: it is the only
// place the direct child is reaped and the only place leftover members are
// found. It is idempotent; later calls return the first report.
//
//  1. scan; if anything owned is live, SIGTERM (+SIGCONT) the group/session
//  2. poll until nothing owned is live or grace elapses
//  3. SIGKILL survivors, poll until clean or ctx is done
//  4. reap the direct child (only now, so its ID stays pinned)
//
// ctx bounds the whole call; grace bounds step 2.
func (p *Process) Terminate(ctx context.Context, grace time.Duration) CleanupReport {
	p.termOnce.Do(func() { p.report = p.terminate(ctx, grace) })
	return p.report
}

type tracker struct {
	p       *Process
	escaped map[ident]Member
}

// classify returns live owned members (direct child included) and updates
// the escape set from lineage.
func (t *tracker) classify(procs []Member) (owned []Member) {
	p := t.p
	byPid := make(map[int]Member, len(procs))
	children := make(map[int][]int)
	for _, m := range procs {
		byPid[m.Pid] = m
		children[m.PPid] = append(children[m.PPid], m.Pid)
	}
	isOwned := func(m Member) bool {
		if p.spec.Session == NewSessionWithCtty {
			return m.Sid == p.pid
		}
		return m.Pgid == p.pid
	}
	for _, m := range procs {
		if isOwned(m) && m.live() {
			m.Why = p.spec.Session.String()
			owned = append(owned, m)
		}
	}
	// Lineage: descendants of the (identity-checked) direct child that are
	// outside the owned set. Only works while the intermediate parents are
	// alive (or the supervisor is a subreaper).
	if leader, ok := byPid[p.pid]; ok && leader.Start == p.start {
		stack := append([]int(nil), children[p.pid]...)
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			m := byPid[pid]
			if !isOwned(m) && m.live() {
				m.Why = "descendant"
				t.escaped[m.id()] = m
			}
			stack = append(stack, children[pid]...)
		}
	}
	return owned
}

// holderScan records processes outside the owned set that hold the child's
// stdio (pipe or PTY slave). These block output draining.
func (t *tracker) holderScan(procs []Member, owned []Member) (supported bool) {
	if !holderScanSupported {
		return false
	}
	if len(t.p.holders) == 0 {
		return true
	}
	skip := map[int]bool{os.Getpid(): true}
	for _, m := range owned {
		skip[m.Pid] = true
	}
	var cands []Member
	for _, m := range procs {
		// A process that started before the direct child cannot have
		// inherited its stdio (barring SCM_RIGHTS), so skip it: this cuts
		// the scan from every fd on the machine to recent processes.
		if !skip[m.Pid] && m.live() && m.Start >= t.p.start {
			cands = append(cands, m)
		}
	}
	for _, m := range findHolders(cands, t.p.holders) {
		if _, dup := t.escaped[m.id()]; !dup {
			m.Why = "holds-stdio"
			t.escaped[m.id()] = m
		}
	}
	return true
}

// liveEscapees re-checks tracked escapees against a fresh scan.
func (t *tracker) liveEscapees(procs []Member) []Member {
	byPid := make(map[int]Member, len(procs))
	for _, m := range procs {
		byPid[m.Pid] = m
	}
	var out []Member
	for id, e := range t.escaped {
		if m, ok := byPid[id.pid]; ok && m.Start == id.start && m.live() {
			m.Why = e.Why
			out = append(out, m)
		}
	}
	return out
}

func (p *Process) terminate(ctx context.Context, grace time.Duration) (r CleanupReport) {
	r = CleanupReport{Mode: p.spec.Session, ID: p.pid, Started: time.Now()}
	t := &tracker{p: p, escaped: map[ident]Member{}}
	defer func() {
		for _, e := range t.escaped {
			r.Escaped = append(r.Escaped, e)
		}
		r.Done = time.Now()
	}()

	procs, err := listProcs()
	if err != nil {
		r.Err = err
		return r
	}
	owned := t.classify(procs)
	if !t.holderScan(procs, owned) {
		r.Unverifiable = append(r.Unverifiable, "stdio-holder scan unsupported on this platform")
	}
	for _, m := range owned {
		if m.Pid != p.pid {
			r.Members = append(r.Members, m)
		}
	}

	targets := func(procs []Member) []Member {
		live := t.classify(procs)
		if p.spec.Escapes == EscapeKill {
			live = append(live, t.liveEscapees(procs)...)
		}
		return live
	}
	live := targets(procs)

	signalAll := func(sig syscall.Signal, live []Member) {
		// One group signal covers the leader's pgroup atomically (including
		// processes forked after the scan). Everything else, e.g. other
		// pgroups in a PTY session or escapees, gets identity-checked
		// per-process signals.
		_ = p.h.signalGroup(sig)
		for _, m := range live {
			if m.Pgid != p.pid {
				_ = signalMember(m, sig)
			}
		}
	}

	// poll rescans with backoff until no target is live or until stop.
	poll := func(stop <-chan time.Time) []Member {
		delay := time.Millisecond
		for {
			procs, err := listProcs()
			if err != nil {
				r.Err = err
				return live
			}
			live = targets(procs)
			if len(live) == 0 {
				return nil
			}
			select {
			case <-stop:
				return live
			case <-ctx.Done():
				return live
			case <-time.After(delay):
			}
			if delay < 16*time.Millisecond {
				delay *= 2
			}
		}
	}

	if len(live) > 0 {
		r.SentTerm, r.TermAt = true, time.Now()
		signalAll(syscall.SIGTERM, live)
		signalAll(syscall.SIGCONT, live) // a stopped process never sees TERM otherwise
		live = poll(time.After(grace))
	}
	if len(live) > 0 {
		// Sent even if ctx is already done: SIGKILL is cheap and nonblocking.
		r.SentKill, r.KillAt = true, time.Now()
		signalAll(syscall.SIGKILL, live)
		live = poll(nil)
	}

	// Final verification: one more full scan including stdio holders. If
	// nothing was signalled, the first scan already was that verification.
	last := procs
	final := func() {
		if procs, err := listProcs(); err == nil {
			last = procs
			owned := t.classify(procs)
			t.holderScan(procs, owned)
			live = targets(procs)
		}
	}
	if r.SentTerm {
		final()
		if len(live) > 0 && ctx.Err() == nil {
			// Newly found targets (e.g. stdio holders under EscapeKill).
			r.SentKill, r.KillAt = true, time.Now()
			signalAll(syscall.SIGKILL, live)
			poll(nil)
			final()
		}
	}
	r.Survivors = live
	r.EscapedLive = t.liveEscapees(last)
	r.Verified = len(live) == 0 && r.Err == nil

	// The direct child is part of the owned set, so once verified it has
	// exited; otherwise wait for it only as long as ctx allows.
	select {
	case <-p.exited:
	case <-ctx.Done():
	}
	select {
	case <-p.exited:
		r.ExitSeen = true
		r.Exit = p.exit
		if ru, err := p.h.reap(); err == nil {
			r.Exit.Rusage = ru
			r.Reaped = true
		} else if r.Err == nil {
			r.Err = err
		}
	default:
		// Leave the zombie-to-be pinned; reap in the background so it does
		// not leak once it does exit.
		go func() { <-p.exited; _, _ = p.h.reap() }()
		if r.Err == nil {
			r.Err = fmt.Errorf("proc: direct child %d still running at deadline", p.pid)
		}
	}
	return r
}
