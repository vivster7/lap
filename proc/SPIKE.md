# proc spike: process supervision findings

Environment: Linux 7.2.5 (omarchy, systemd user session), amd64, 12 CPUs,
about 425 processes visible in /proc, Go 1.27.1, golang.org/x/sys v0.41.0,
creack/pty v1.1.24 (tests only). Darwin code is compile-checked only
(`GOOS=darwin go vet ./proc/` for amd64 and arm64). None of it has been run.

Run: `mise exec -- go test -race ./proc/...` takes about 7 s. `-v` prints
every measurement quoted below. Every probe asserts what it observed, so a
kernel or toolchain change that alters a result fails the test.

Fixtures: the test binary re-executes itself as the workload
(`LAP_PROC_HELPER=1`, plan tokens documented in `helper_test.go`). Helpers
set `GORACE=atexit_sleep_ms=0` because race-instrumented binaries otherwise
sleep 1 s on exit, which distorted every timing under `-race`. Each test
SIGKILLs every PID it saw, after checking start time, and calls Terminate in
`t.Cleanup`. `pgrep -af proc.test` was empty after the runs.

## Answers

### Q1. Does Setsid + Setpgid fail? (`TestProbeSetsidPlusSetpgid`)

- **It fails.** `Start` returns `*fs.PathError{Op:"fork/exec"}` wrapping
  `EPERM` ("operation not permitted"). Go's child runs `setsid()` and then
  `setpgid(0,0)`. POSIX forbids changing a session leader's process group.
  The child never execs.
- **Setsid alone gives `pid == pgid == sid`.** Setpgid alone gives
  `pid == pgid`, with the sid inherited from the test.
- PTY mode therefore uses `Setsid+Setctty` with `Ctty` set to a *child* fd
  number (0..2). Pipe mode uses `Setpgid`.

### Q2. exec.Cmd Cancel/WaitDelay, and exec.Cmd or not

- **Default cancellation kills only the direct child**
  (`TestProbeCommandContextKillsOnlyDirectChild`). After `cancel()`, Wait
  returned `signal: killed`. The grandchild was still alive 100 ms later,
  reparented to systemd --user (pid 1168, a subreaper on this desktop) and
  still in the old pgroup.
- **A group Cancel works only for cooperative trees**
  (`TestProbeCommandContextGroupCancel`). With
  `cmd.Cancel = kill(-pid, SIGTERM)` and `WaitDelay = 200ms`, Wait returned
  about 0.2 ms after cancel because the direct child died of TERM. A
  TERM-ignoring grandchild **survived past WaitDelay**. WaitDelay escalates
  with `Process.Kill`, which reaches the direct child only, and it has
  nothing left to kill once the child is dead.
- **Decision: do not use exec.Cmd. Use `syscall.ForkExec` with
  `SysProcAttr.PidFD`.** The reasons:
  1. proc's stdio is always `*os.File`, so exec.Cmd's copy goroutines and
     WaitDelay never apply.
  2. Cleanup needs to see the child exit *without reaping it*
     (`waitid(P_PIDFD, WEXITED|WNOWAIT)`) and to reap it only after
     verification. `os.Process.Wait` and `exec.Cmd.Wait` always reap.
  3. Cancel/WaitDelay would have to be replaced anyway.
  4. `exec.Command` resolves argv[0] with the *supervisor's* PATH, not
     `cmd.Env`'s. mise environments change PATH, so proc resolves it
     against `Spec.Env` (`TestLookPathUsesSpecEnv`).

### Q3. Cleanup cases (`cleanup_linux_test.go`)

| Case | Pipe mode (NewGroup) | PTY mode (NewSessionWithCtty) |
| --- | --- | --- |
| (a) normal grandchild sleeping | TERM only, verified | TERM only, verified |
| (b) grandchild ignores TERM | KILL after grace, verified | KILL after grace, verified. The fixture must also ignore **HUP**, see below |
| (c) leader exits, grandchild holds stdout | Wait returns at once (exit 7). Leader stays a zombie (state Z). Reader gets no EOF until Terminate. EOF arrives about 80 µs after Terminate returns | grandchild is SIGHUPed by the kernel when the leader exits (`TestPTYLeaderExitHangsUpForeground`). A HUP-ignorer survives until Terminate |
| (d) grandchild calls setsid | not in the group. Found by **lineage** while its parent lives. Found by **stdio-holder scan** once orphaned. **Undetectable** once orphaned with stdio closed | the same. setsid leaves the session too, so session scanning does **not** catch it |
| setpgid-only escape | not in the group. Lineage only | **owned**: the sid still matches. Killed with an identity-checked per-PID signal |
| (e) double fork, stays in group | orphan reparented to pid 1168 with pgid unchanged. Group kill covers it | (same mechanism) |
| stopped member (SIGSTOP) | TERM+CONT. Dies without KILL | – |
| zombie grandchild | not counted as live. No KILL needed | – |

- **(c) and WaitDelay.** proc never reads output, so Wait cannot hang on a
  pipe. `TestProbeWaitDelayAndGrandchildHoldingStdout` shows exec.Cmd's
  behaviour:
  - With a `bytes.Buffer` Stdout and no WaitDelay, Wait was still blocked
    500 ms after the child exited. It blocks for as long as the grandchild
    lives.
  - With `WaitDelay=150ms`, Wait returned `exec.ErrWaitDelay` at about
    157 ms. The pipe is abandoned and the **grandchild is left alive**.
  - With an `*os.File` Stdout, Wait returned when the child exited.

  Draining therefore belongs to term. proc's job is to make EOF arrive by
  killing every holder.
- **(d) and the session question.** Session-based scanning in PTY mode
  catches *setpgid* escapes, which is what job-control shells do. It does
  not catch *setsid* escapes, in either mode. Three mechanisms catch
  escapees:
  - *Lineage*: walk PPid from the identity-checked leader. This works only
    while the intermediate parents are alive.
  - *Stdio holders*: any live process outside the owned set whose
    `/proc/<pid>/fd` points at the task's output pipe inode or
    `/dev/pts/N`. This catches orphaned escapees that would block draining.
  - *Subreaper* (`TestProbeSubreaperKeepsLineage`): with
    `PR_SET_CHILD_SUBREAPER` on the supervisor, an orphaned setsid escapee
    was reparented to the supervisor (`ppid == self`). That keeps it
    discoverable even after it closes its stdio. Terminate's lineage walk
    starts at the leader and did not find it; that is asserted.

  The remaining gap: an escapee that is orphaned, has closed its stdio, and
  runs without a subreaper. The test asserts `Verified=true` while it is
  still alive (`TestCleanupSetsidEscape/undetectable`). This is the
  documented "process traversal cannot close every escape race".

### Q4. Verifying "no live owned members"

- **Linux** (`proc_linux.go`):
  - Reads `/proc/*/stat` with open/read/close and parses after the *last*
    `)`, because comm can contain `) (`. See `TestParseStatHostileComm`.
  - Ownership is `pgid == leader` (group mode) or `sid == leader` (PTY
    mode). States `Z` and `X` do not count as live.
- **PID reuse.**
  - Group signals: the direct child is **not reaped until Terminate has
    finished verifying**. A zombie keeps its PID allocated, and therefore
    its pgid/sid number too, so `kill(-pgid)` can never hit a recycled
    group.
  - Per-process signals (other pgroups in the session, escapees):
    `pidfd_open`, then re-read `/proc/<pid>/stat` field 22 (start time) and
    compare, then `pidfd_send_signal`. The pidfd pins the process, so no
    reuse window remains (`TestSignalMemberRejectsStaleIdentity`).
  - The direct child itself is waited on through its pidfd.
- **Holder-scan cost.**
  - An unfiltered scan reads every fd link of every readable process:
    about 11 ms here.
  - Terminate scans only processes whose start time is at or after the
    leader's. Nothing older can have inherited the task's pipe.
- **Darwin** (`proc_darwin.go`, untested):
  - Process table from `sysctl kern.proc.all` (`unix.SysctlKinfoProcSlice`),
    sid from `getsid(2)`, identity from `p_starttime`.
  - Exit is observed without reaping through kqueue
    `EVFILT_PROC NOTE_EXIT|NOTE_EXITSTATUS`. If registration fails, it falls
    back to a reaping wait4, which loses the pgid pin.
  - No pidfd, so per-PID signals have a check-then-kill window.
  - No stdio-holder scan. It is reported in
    `CleanupReport.Unverifiable`; it would need libproc
    `proc_pidinfo(PROC_PIDLISTFDS)`.
  - `ru_maxrss` is in bytes on darwin and KiB on Linux. Both are normalised
    to `MaxRSSBytes`.

### Q5. Cleanup latency (`TestCleanupLatency`, n=10, no `-race`)

| Measurement | p50 | max |
| --- | --- | --- |
| Full /proc scan (about 428 procs) | 3.8 ms | 4.7 ms |
| Unfiltered stdio-holder scan, all procs | 11.1 ms | 12.7 ms |
| Verify and reap only (child already exited, nothing left) | 3.9 ms | 5.1 ms |
| TERM-responsive child+grandchild: Terminate total (TERM to verified to reaped) | 11.7 ms | 13.5 ms |
| TERM-ignoring grandchild, grace 20 ms: KILL to verified | 6.7 ms | 8.7 ms |
| TERM-ignoring grandchild, grace 20 ms: Terminate total | 33.6 ms | 35.9 ms |

Notes on these numbers:

- The first version used `os.ReadFile`. Its scan took 5 ms, and it ran the
  holder scan twice, unfiltered: verify-only took 35 ms.
- Under `-race`, Terminate totals are about 25–60 ms.
- Polling backs off from 1 ms to 16 ms, so detection adds at most one
  interval.
- **Rule of thumb:** cleanup costs grace + about 10 ms + a few scans. Size
  the cleanup reserve as `grace + ~50 ms` on a responsive host. These are
  synthetic Go helpers; real tools may need a longer grace to flush.

### Q6. Lease ordering (`lease_linux_test.go`)

- **Normal-cancellation path.** The token was opened with O_CLOEXEC (Go's
  default); the test asserts that neither child nor grandchild has it in
  `/proc/<pid>/fd`.
  - A contender polled the token every 1 ms throughout. It did **not**
    acquire it during Terminate (TERM to verified took 66 ms with a
    TERM-ignoring grandchild).
  - It acquired the token about 130 µs after the supervisor's `close()`,
    which came after `Verified`. The grandchild was dead at the moment of
    acquisition.
- **Why close and not LOCK_UN.** When a duplicate of the lock fd leaks into
  a child:
  - `close()` in the supervisor leaves the token **held** until the child
    exits.
  - `LOCK_UN` frees it **immediately**, while the child still runs.

  Close is the conservative release. LOCK_UN acts on the open file
  description and silently releases it for every holder.
- **Rule:** release leases only after `Verified`. If `Verified` is false or
  `EscapedLive` is non-empty, keep the lease until a later check passes, or
  report the leak; do not hand the capacity back.

### Q7. Pdeathsig and OS-thread exit

- **Pdeathsig fires on thread exit, not process exit**
  (`TestProbePdeathsigThreadExit`). A child started with
  `Pdeathsig=SIGKILL` from a goroutine that returned while
  `LockOSThread`ed was killed after about 10 ms. Go terminates that thread.
- When the locked goroutine happened to run on the **main thread**, the
  runtime wedges the thread instead of exiting it, and nothing fired. The
  outcome is nondeterministic and depends on which M ran the goroutine. With
  `UnlockOSThread` the child lived.
- **Practical rule:** never call `Start` (with Pdeathsig) from a goroutine
  that is locked to its thread and may exit. If Pdeathsig is adopted, fork
  from one dedicated goroutine that stays locked for the life of the
  process.
- **What Pdeathsig covers on runner SIGKILL**
  (`TestProbeRunnerKilledLeavesGrandchild`):
  - It kills the **direct child only**. The grandchild survives,
    reparented, in a pgroup whose leader is gone.
  - That pgroup stays addressable with `kill(-pgid)` while it has members.
    That is the only handle lazy recovery would have, and its identity
    cannot be proven once the group has been empty.
- **PTY mode needs no Pdeathsig** (`TestProbeRunnerKilledPTYHangup`). When
  the runner dies, the kernel closes the master, hangs up the slave, and
  SIGHUPs the session. The leader and a cooperative grandchild died; a
  HUP-ignoring grandchild survived.

## Final API

```go
type SessionMode int // NewGroup (pipes: Setpgid) | NewSessionWithCtty (pty: Setsid+Setctty)
type EscapePolicy int // EscapeReport (default) | EscapeKill

type Spec struct {
    Argv []string; Dir string; Env []string // nil Env = os.Environ(); argv[0] resolved via Env's PATH
    Stdin, Stdout, Stderr *os.File         // child ends; nil = /dev/null; caller closes its copies after Start
    Session SessionMode
    CttyFD int                              // child fd that is the PTY slave (default 0)
    Pdeathsig syscall.Signal                // Linux only; direct child only; see Q7
    Escapes EscapePolicy
}

func Start(spec Spec) (*Process, error)
func (p *Process) Pid() int                                   // == pgid == sid (PTY)
func (p *Process) Exited() <-chan struct{}                    // direct child exited (not reaped)
func (p *Process) Wait(ctx) (ExitInfo, error)                 // non-reaping; no Rusage yet
func (p *Process) Terminate(ctx, grace time.Duration) CleanupReport // mandatory, idempotent; the only reaper

type ExitInfo struct { Pid int; At time.Time; Code int; Signal syscall.Signal; Rusage *Rusage }
type Rusage struct { User, System time.Duration; MaxRSSBytes int64 }
type CleanupReport struct {
    Mode SessionMode; ID int
    Exit ExitInfo; ExitSeen, Reaped bool
    Members, Escaped, EscapedLive, Survivors []Member
    SentTerm, SentKill bool; Started, TermAt, KillAt, Done time.Time
    Verified bool; Unverifiable []string; Err error
}
```

How Terminate works:

1. Scan, run lineage and the holder scan.
2. If anything owned is live, send `TERM` and then `CONT` with one
   `kill(-pgid)` plus identity-checked per-PID signals for members of other
   pgroups and for escapees (under EscapeKill).
3. Poll with backoff until nothing is live or `grace` expires.
4. Send `KILL` and poll until `ctx` is done.
5. Run a final scan that includes holders. If it finds new targets, KILL
   them.
6. Reap the leader.

It is always called, including after a clean exit. A clean exit costs about
4 ms.

Contract with term:

- term passes the child ends and closes its own copies right after `Start`.
- term keeps draining until EOF/EIO, or until its own drain deadline.
- Output fds (Stdout/Stderr) **must be exclusive to the task**. The holder
  scan treats any holder of them as part of the workload. The supervisor's
  own PID is excluded, and stdin is not scanned.

## Known gaps / unsupported

- **Undetectable escapes.** A setsid or double-forked daemon that is
  orphaned and has closed its stdio is not found on Linux without a
  subreaper, and not on darwin at all. Terminate reports `Verified=true`.
  This is the tested gap.
- **Subreaper is not wired in.** It is process-wide. The supervisor would
  have to reap reparented orphans itself, and it must never `wait4(-1)`,
  which would steal other in-process children such as os/exec users. A
  safe design reaps only zombies whose ppid is the supervisor and that no
  `Process` or other code owns, and that ownership is hard to prove in a
  library. Recommended only as an opt-in for the starter binary.
- **Rusage is incomplete.** It covers the direct child plus the descendants
  that child itself waited for. Orphans reaped by systemd or init are
  missing. MaxRSS is the largest single process.
- **Darwin:** untested. No pidfd, so there is a small reuse window on
  per-PID signals. No holder scan. The kqueue registration race falls back
  to reaping.
- **Processes in uninterruptible sleep (D state) can outlive KILL.**
  Terminate returns `Verified=false` with `Survivors` and reaps the leader
  in the background.
- **Other users' processes.** setuid helpers and hidepid mounts can hide
  `/proc` entries or fds. Ownership by pgid/sid still works when stat is
  readable; holder detection silently skips unreadable fd dirs.
- **pidfd fallback.** Kernels without pidfd waitid (< 5.4) fall back to P_PID wait
  and check-then-kill. Not exercised.

## Recommendations for docs/design.md

1. **§Process supervision, "Do not combine that setup with an unnecessary
   Setpgid".** The combination is *invalid*, not unnecessary: Go's
   ForkExec fails with EPERM. Say so. Setsid alone gives pgid == sid == pid.
2. **PTY ownership is by session, not group.** "Track the actual owned
   groups" should become "in PTY mode, own the session (sid == leader)".
   Job-control shells create pgroups inside the session; session scanning
   kills them (tested). Neither scan catches setsid escapes.
3. **Add the reaping-order rule.** "Reaping direct children" should be
   "observe exit with WNOWAIT, reap only after group/session verification".
   The unreaped leader pins the pgid/sid number, which closes PID reuse for
   group signals. Use pidfd plus a start-time check for per-PID signals.
4. **exec.Cmd is not a good basis.** Beyond the cited CommandContext
   behaviour:
   - WaitDelay escalation also reaches only the direct child.
   - exec.Cmd resolves argv[0] with the parent's PATH, not `cmd.Env`.
   - It cannot wait without reaping.

   Recommend ForkExec with PidFD on Linux.
5. **The PTY hangup changes the acceptance fixtures.** When a PTY session
   leader exits, or the master closes, the kernel SIGHUPs the foreground
   pgroup. A "TERM-resistant grandchild" fixture in PTY mode passes *for
   the wrong reason* unless it also ignores SIGHUP. Specify both in the
   acceptance row. The same effect means a runner SIGKILL already tears
   down cooperative PTY workloads. The "Runner killed" row should be
   tested per capture mode.
6. **Add stdio-holder detection** as a named verification criterion
   alongside group/session membership: a live process outside the owned
   set holding the task's output pipe or pty. It explains exactly why
   draining cannot finish. It requires per-task exclusive output fds,
   which is a term contract.
7. **Leases.** Confirm "release after verified cleanup" and add three
   points:
   - Release by `close`, never `LOCK_UN`.
   - Open tokens O_CLOEXEC; never pass them to children.
   - If verification fails or live escapees remain, the lease is not
     released as "free capacity" without reporting it.
8. **Pdeathsig is optional and limited.** It reaches the direct child only
   and fires on *thread* exit (LockOSThread hazard). If used, fork from a
   permanently locked spawner goroutine. Mention `PR_SET_CHILD_SUBREAPER`
   as the Linux mechanism that keeps orphans discoverable, with its
   process-wide reaping caveat, as an input to the "separate supervisor"
   decision.
9. **Cleanup reserve sizing.** The measured mechanics cost about 10 ms
   beyond `grace` on a responsive host with about 430 processes. The
   reserve is dominated by the chosen grace and by how long tools take to
   exit after TERM, not by supervision overhead.
10. **Rusage units/coverage.** ru_maxrss is KiB on Linux and bytes on
    darwin, and it covers only waited-for descendants. The doc mentions
    the semantics; add the units.
