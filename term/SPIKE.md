# term spike: PTY/pipe capture, journal, rendered and plain views

Status: exploratory spike, Linux-verified (kernel 7.2, Go 1.27.1, amd64). darwin
compiles (`GOOS=darwin go vet ./term/`) but nothing was run on macOS.

Reproduce:

    mise exec -- go test -race ./term/...                         # ~23 s
    mise exec -- go test -v -run 'TTYDetection|TerminalQueries' ./term/
    mise exec -- go test -run XXX -bench Capture200MB -benchtime 1x ./term/
    cd term/spike/emucompare && mise exec -- go run .            # emulator comparison

## Final API

```go
type Mode int        // PTY, Pipes, StdoutPipeStderrPTY
type Stdin int       // StdinNull (default), StdinTTY (PTY mode only)
type Queries int     // QueriesAnswer (default), QueriesIgnore
type Spec struct {
    Mode Mode; Cols, Rows uint16 /* default 80x24 */; Dir string; Term string /* default xterm-256color */
    Stdin Stdin; Queries Queries; Live bool; Record map[string]string // e.g. color policy given to the child
    ReadSize, QueueChunks, LiveQueueBytes int                          // bounds (32 KiB, 64 chunks, 1 MiB)
}
func Open(Spec) (*Session, error)
func (*Session) ChildStdio() (stdin, stdout, stderr *os.File)
func (*Session) NeedsCtty() bool; CttyFD() int; LaunchAttrs() LaunchAttrs // {Setsid,Setctty,CttyFD} | {Setpgid}
func (*Session) Env() []string                    // ["TERM=..."] in PTY modes, nil for pipes
func (*Session) CloseChildEnds() error             // call right after Start (Done also calls it)
func (*Session) Resize(cols, rows uint16) error
func (*Session) Drained() <-chan struct{}          // every stream at EOF/hangup
func (*Session) Done(ctx) (Summary, error)         // ctx = drain deadline; err wraps ErrIncomplete
func (*Session) Close() error                      // always call; hangs up a still-open PTY
func (*Session) Screen() string; Updates() <-chan struct{}; LiveDropped() int64   // live view

func OpenCapture(dir) (*Capture, error)
func (*Capture) Summary() (Summary, bool)          // false: never finalized (crash/running) => incomplete
func (*Capture) Raw() (*os.File, error)            // output.bytes
func (*Capture) Replay(func(Event, []byte) error) error   // journal + chunk bytes, bounded memory
func (*Capture) WriteStream(w, stream) error
func (*Capture) Plain(w, PlainOptions) error
func (*Capture) Rendered(w, RenderOptions) (RenderInfo, error)
func (*Capture) WriteAsciicast(w, streams) (AsciicastInfo, error)
func (*Capture) BuildViews(RenderOptions) (ViewsInfo, error); ViewsCurrent() bool
```

Recommended launcher sequence (proc): `Open` → `exec.Cmd{Stdin/Stdout/Stderr: ChildStdio(), Env: +Env(), SysProcAttr from LaunchAttrs()}` →
`Start` → `CloseChildEnds` → wait for exit → `select { <-Drained(); <-tailGrace }` → if not drained, terminate the
owned session/group → `Done(ctx with remaining cleanup allowance)` → `Close`. Tested in
`TestPTYGrandchild/recommended`.

## Storage format (`Dir`)

| file | content |
| --- | --- |
| `output.bytes` | primary record: raw bytes of all streams in read order, byte-exact, binary safe |
| `events.jsonl` | `start` (version, mode, cols, rows, term, streams, tty flags, wall clock, stdin, queries, record), `chunk {s, off, n, ns}`, `resize {off, cols, rows}`, `input {s, data}` (query replies written to the terminal), `eof {s, end}`, `error {s, err}`, `write_error {off, err}`, `incomplete {off, n, reason}` (n = -1: unknown tail), `end` |
| `capture.json` | `Summary`, written atomically at finalize; absence = not finalized |
| `plain.txt`, `rendered.ansi`, `views.json` | derived caches with renderer id `github.com/charmbracelet/x/vt@v0.0.0-20261001101533-953920dd3285+lap-term.r1` and plain revision |

`ns` is monotonic nanoseconds since Open. Bytes are written before their chunk event, so an event never
points at unwritten bytes; events are flushed whenever the writer queue is empty (64 KiB batches under load).
After a crash, `Replay` tolerates a partial last journal line (`ErrTruncatedJournal`) and delivers bytes
past the last journaled chunk as stream `unjournaled` (`TestCrashLeftovers`). Asciicast v2 is an export
only (`WriteAsciicast`): split UTF-8 is carried across events, invalid bytes become U+FFFD and are counted.

Architecture: one reader goroutine per stream (pollable fd) → bounded queue (64 x 32 KiB, lossless
backpressure on the child) → one disk writer goroutine (output.bytes + journal) → non-blocking, lossy
offer to the live emulator. Display never touches the drain path.

## Q1. EIO on the master after child exit

- Confirmed: once the *last* slave fd closes, `read(/dev/ptmx)` returns all buffered output and then
  `EIO`. Mapped to end `hangup` only for PTY streams; EIO on a pipe and any other PTY error stay
  `error` with the message (`TestReadErrorsStayVisible`).
- Tail never lost: `python3 -c 'os.write(1, b"x"*N+b"END"); os._exit(0)'` for N in
  {0,1,100,4095,4096,4097,64 KiB,1 MiB}, 8 in parallel: **2000 iterations, 0 lost bytes** (plus 200
  iterations of `head -c 300000 /dev/zero | tr '\0' x; printf END`, last writer a grandchild).
- The parent's own slave copy must be closed: with it open the master read just blocks
  (`TestPTYHangupNeedsParentSlaveClosed`). `CloseChildEnds` exists for this; `Done` calls it defensively.
- **creack/pty v1.1.24 returns a blocking master.** `Open` and `Setsize` call `(*os.File).Fd()` through
  its `ioctl` helper, which permanently switches the fd to blocking mode: `SetReadDeadline` succeeds but
  has no effect and `Close` cannot interrupt a blocked `Read` (`TestCreackMasterIsBlocking`). term
  re-wraps the master (`dup` + `O_NONBLOCK` + `os.NewFile`, Linux) and sets the window size through
  `SyscallConn` so a drain deadline can actually stop reading.
- Grandchildren (`TestPTYGrandchild`, `TestPipesGrandchild`):
  - PTY, ordinary background job (`(sleep 1; echo late) &`): when the session leader exits the kernel
    sends SIGHUP to the terminal's foreground process group, the job dies, drain ends in 3 ms. Its later
    output never exists.
  - PTY, HUP-ignoring descendant: it keeps the slave open; output keeps flowing and is captured (Done
    waited 1.0 s and got the late write).
  - PTY or pipes, descendant outliving the deadline: `Done(ctx)` returns at the deadline (301 ms for a
    300 ms ctx) with `ErrIncomplete`, stream end `open_at_deadline`, an `incomplete {n:-1}` journal event,
    and everything read before the deadline stored.
  - Pipes never hang up: even an ordinary `sleep 30 &` holds the stream open until the group is killed.
  - Killing the group after a grace and then calling Done gives a *complete* capture (hangup).

## Q2. Line discipline

Fresh Linux PTY termios (measured): `OPOST ONLCR ECHO ECHOCTL ICANON ISIG ICRNL IXON` on, `IUTF8` off; the
child sees the same through `/dev/tty` (`stty -a`). **Decision: leave the kernel defaults (cooked).**
They are what an interactive terminal gives a program, so tools behave as for a user, and programs that
need raw mode set it themselves (the query fixture does). Clearing OPOST/ONLCR would turn each `\n` into a
bare LF, which an emulator renders as a staircase unless the renderer guesses. The primary record keeps
what was received (`a\nb\n` arrives as `a\r\nb\r\n`, `TestLineEndingsPreserved`); only derived text
normalizes CRLF. ECHO only matters for bytes written to the master, i.e. query replies: a program that
queries without raw mode gets the reply echoed into its output (`q\x1b[6n^[[1;2Rdone`,
`TestReplyEchoedInCookedMode`), exactly as in a real terminal, and the journal records the `input` event
so it is explainable. Stdin defaults to `/dev/null` (isatty(0) false, so prompt-before-reading tools do not
prompt); the PTY is still the controlling terminal, so `/dev/tty` readers can still block, which ECHO or
not does not change.

## Q3. TTY detection (`TestTTYDetection`)

| probe | PTY | Pipes | stdout pipe + stderr PTY |
| --- | --- | --- | --- |
| python `isatty(0,1,2)` | False True True | False False False | False False True |
| node `isTTY(1,2)` | true true (and colored) | false false | false true |
| node `console.log({n:1})` | `{ n: \e[33m1\e[39m }` | `{ n: 1 }` | `{ n: 1 }` (stdout) |
| `ls --color=auto` | `\e[01;34msubdir\e[0m` | plain | plain |
| `grep --color=auto` | `h\e[01;31m\e[Kell\e[m\e[Ko` | plain | plain |
| python 3.14 traceback (stderr) | colored | plain | colored |

Tests strip color overrides (NO_COLOR, FORCE_COLOR, CLICOLOR*, PYTHON_COLORS, LS_COLORS, COLORTERM) so
TTY detection alone decides; pipe mode gets `TERM=dumb`.

## Q4. Emulator evaluation (`term/spike/emucompare`)

| fixture | charmbracelet/x/vt | danielgatis/go-headless-term v1.0.9 | ActiveState/vt10x v1.3.1 |
| --- | --- | --- | --- |
| CR progress, cursor-up + EL, alt screen, CSI split across writes | ok | ok | ok |
| CJK width (cursor col after `a界b`) | 4 ok | 4 ok | 3 wrong |
| emoji / ZWJ family / flag width | 2 / 2 / 2 ok | 2 / 6 / 4, ZWJ dropped | 1 / 5 / 2 wrong |
| UTF-8 split across writes | ok | ok | 4-byte split lost (short-write contract) |
| combining mark split across writes | **lost** (fixed by term's feeder) | lost | ok-ish |
| DSR / DA1 / OSC 11 replies | `\e[1;1R` / VT220 / rgb | yes / minimal / none | yes / none / none |
| scrollback | bounded 10k lines, in memory | pluggable provider | unbounded slice |
| speed (escape-heavy lines) | 5.4 MB/s | 10.7 MB/s | 29.8 MB/s |
| heap for 10k scrollback lines | 63 MB (~6 KB/line) | 238 MB | 210 MB (unbounded) |

**Chosen: `github.com/charmbracelet/x/vt v0.0.0-20261001101533-953920dd3285`** (pinned in go.mod; with
`ultraviolet v0.0.0-20260303162955-0b88c25f3fff`, `x/ansi v0.11.7`). Best fidelity (grapheme clusters,
widths, styles, hyperlinks, ANSI re-render of lines, query replies). Gaps found and how term handles them:

1. Graphemes split across `Write` calls are broken (vt flushes the pending cluster at the end of every
   Write; `TestVTSplitGraphemeGap`). term's `feeder` writes only up to the last C0 control byte and holds
   the printable tail (≤ 4 KiB), making rendering independent of read boundaries
   (`TestRenderChunkingInvariant`: chunk sizes 1, 2, 3, 5, 7, 13, 64 and whole produce identical output).
2. Query replies go to an unbuffered `io.Pipe`: without a reader, the first DSR/DA in the stream
   deadlocks `Write`. term always drains it (`newEmulator`), and closes the pipe writer rather than
   calling `Close` (its `closed` flag is unsynchronized).
3. Scrollback is in memory, evicts with an O(n) `slices.Delete` when full, costs ~6 KB per line, and is
   erased by ED 3 / RIS (`clear` emits `\e[H\e[2J\e[3J` in one write). term streams scrollback out after
   every ≤ 4 KiB emulator write and splits writes before ED 3/RIS, so the rendered view keeps full history
   on disk with bounded RAM (100k lines: peak heap +15 MB; `TestRenderKeepsHistoryAcrossClear`).
4. Throughput ~2.7-6 MB/s, dominated by `DeleteLineArea` per scrolled line (60% of CPU). A 200 MB log
   would take ~40-70 s to render. Rendered views are derived on demand, off the drain path; `RenderOptions.MaxBytes`
   renders a tail (8 MB tail of the 200 MB capture: 2.1 s). The plain projection (70-87 MB/s) is the
   scalable searchable view.
5. Lines scrolled inside the alternate screen also go to (the alt screen's) scrollback; term discards
   them. If a capture ends in the alt screen, the rendered view shows it, then a marker, then the main
   screen. Rendered lines are screen rows (long lines appear wrapped, no reflow on resize).
6. Experimental, pseudo-versioned API (the pinned commit is from 2026-10-01). Upgrades need the
   test suite and a renderer-id bump.

## Q5. Plain chronological projection (`plain.go`, revision 1)

Per stream, a pending line of cells with a cursor: printable text overwrites at the cursor; CR → column 0;
BS left; CSI G/`/C/D move; CSI K (0/1/2), X, P, @ edit; LF/VT/FF commit; any vertical movement
(CSI A B E F H f d) commits a non-empty pending line; SGR, other CSI, OSC (hyperlink text kept), DCS/SOS/PM/APC,
other ESC sequences and C0 controls are stripped; zero-width code points attach to the previous cell;
invalid UTF-8 → U+FFFD; UTF-8 and escape sequences split across chunks are reassembled; trailing blanks
trimmed; lines over 64 Ki cells are committed in pieces (bounded memory, `TestPlainLongLineBounded`).
Fixture `testdata/progress.py` (CR + `\e[2K` redrawn colored bar, é split across two writes 20 ms apart,
red error on stderr) through a PTY: raw 478 bytes → plain
`building [##########] 100%\ncafé done\nERROR: tests failed: 3\n` (no intermediate percentages;
`TestPlainProgressFixture`). Limitations: cursor-up redraws (multi-line progress) appear once per committed
version; clears/absolute positioning are not replayed; alt-screen text is included; wide characters count as
one cell for overwrite purposes. Pipe streams keep separate line state; `Prefix` labels lines.

## Q6. Terminal queries

`testdata/query.py` prints `before`, enters raw mode, sends DSR 6 and reads until `R`:
- no reply, no timeout: **the child hangs** (still blocked after 1.5 s, killed by the test). With its own
  0.5 s timeout it continues with `NO-REPLY`.
- `QueriesAnswer` (default): a live vt emulator follows the PTY stream and answers from its state:
  `REPLY 1;7` (row 1, col 7, after the 6-cell `before`), with stdin `/dev/null` (child opens `/dev/tty`),
  stdin = TTY, and on the stderr PTY of the mixed mode. vt answers DSR 5/6, DECXCPR, DA1/DA2, DECRQM and
  OSC 10/11/12 color queries.
- Safety: replies are written by a separate goroutine with a 1 s write deadline and a bounded queue; they
  are journaled as `input` events. The live emulator never blocks the writer: beyond 1 MiB of backlog it
  drops bytes (counted in `Summary.LiveDropped`), after which CPR answers are best effort. Cost: the
  emulator follows at ~5 MB/s, so heavy output burns up to one core in the follower (+0.7 CPU-s per 100 MB
  in the 200 MB benchmark); adapters with huge output can set `QueriesIgnore`.
- TERM: `xterm-256color`. tput's xterm-256color sequences (setaf, sgr0, cup, smcup/rmcup =
  `\e[?1049h\e[22;0;0t`, el, bold) render correctly (`TestRenderTputSequences`); vt implements 256-color
  and truecolor SGR, so COLORTERM=truecolor would also be accurate, but it is left to the caller's color
  policy (record it via `Spec.Record`). NO_COLOR etc. are passed through by the launcher, not set by term.

## Q7. Throughput, memory, backpressure (`BenchmarkCapture200MB`, 1 run each, tmpfs and btrfs)

| mode | drain MB/s | bytes/read | parent CPU s / 100 MB | peak RSS | peak Go heap |
| --- | --- | --- | --- | --- | --- |
| generator to /dev/null | 3800-4500 | | | | |
| PTY | 156-183 | 700-960 | 1.0-1.3 | 19-21 MB | 6 MB |
| PTY + live emulator answering queries | 137-166 | ~700-960 | 1.8-2.3 | 29-32 MB | 17-19 MB |
| pipes | 1240-2015 | 8-20 KiB | 0.10-0.15 | 18-28 MB | 4 MB |
| stdout pipe + stderr PTY (all on stdout) | 1330-1890 | 7-17 KiB | 0.10-0.14 | 19-20 MB | 9 MB |

RSS is bounded and independent of output size. PTY throughput is limited by the kernel tty path (each master
read returns ≤ 4 KiB, ~1 KiB on average, so ~10x more reads than pipes). Slow display: a consumer that
sleeps 50 ms per frame while 31 MB stream through the PTY gets 5 frames; drain time is unchanged (229 ms
without vs 257 ms with the live view), 28.6 MB were skipped by the live view only, the record is complete
(`TestSlowLiveConsumerDoesNotBlockDrain`). Slow *disk* is lossless backpressure: readers block on the
bounded queue, so the child blocks on write; the drain deadline still bounds Done.

## Q8. Disk failure (`TestDiskFailureKeepsDraining`)

A writer failing with ENOSPC after 100 000 bytes, 4 MB of output (far beyond PTY/pipe buffers): draining
continues and discards (pty 19 ms, pipes 6 ms total; the child never blocked), `output.bytes` holds exactly
the first 100 000 bytes, journal has `write_error {off:100000}` and `incomplete {off:100000, n:3900000}`,
`Summary{Complete:false, Received:4000000, Stored:100000, Incomplete:[{100000, 3900000, "write
output.bytes: no space left on device"}]}`, `capture.json` says incomplete and `Done` returns
`ErrIncomplete`. If the journal itself cannot be written, the summary still records it as a problem.

## Open issues

- **macOS untested.** Master stays blocking (creack/pty's darwin Open; kqueue on PTY masters not verified),
  so a reader blocked past the drain deadline is abandoned rather than interrupted; macOS hangup semantics
  (EIO vs 0) unverified. Needs a macOS run of this suite.
- A writer stuck inside a disk write syscall cannot be interrupted: Done reports `storage writer did not
  finish` after a 250 ms grace and the goroutine finishes later.
- Live view desync after drops is permanent for that session (no resync from disk); acceptable for display,
  makes later CPR answers best effort.
- Asciicast export merges streams and carries partial UTF-8 across streams; fine for PTY captures.
- Resize offsets are the logical offset when the writer processed the resize, i.e. relative to bytes
  already read, not to what the child had written: within one read of the kernel's ordering.
- Plain projection's one-cell-per-code-point model is approximate for CR-overwrites of wide text.
- Not measured: concurrent captures from many tasks, fsync policy (none is done; recording stays inside
  the deadline), retention/compression of closed captures.

## Design-doc corrections and additions

1. "creack/pty uses Setsid and Setctty": true, but v1.1.24's `Open`/`Setsize` leave the master in
   blocking mode, which silently defeats "finish draining within the cleanup allowance". The master must be
   made pollable (or read through a dedicated, abandonable thread) and resized without `Fd()`.
2. Descendants holding output open behave differently per mode: in PTY mode the session leader's exit
   SIGHUPs the foreground group, so ordinary background jobs die immediately; in pipe mode they keep the
   stream open until the group is killed. "Descendants holding the slave open" are therefore only
   HUP-ignoring/daemonizing ones. Drain completion needs the launcher: tail grace → kill group → Done.
3. Hangup occurs only when *every* slave fd is closed, including the launcher's copy; closing the parent
   copies after Start is part of the term/proc contract.
4. The rendered view is not cheap: the chosen emulator renders ~3-6 MB/s, so for large captures it must be
   lazy and tail-limited; the plain projection is the view that scales. Its scrollback must be streamed out
   (not kept in the emulator) because ED 3 and `clear` erase it and memory is ~6 KB per line.
5. Per-attempt layout gains `capture.json` (term's finalized summary) and derived `plain.txt`,
   `rendered.ansi`, `views.json`; query replies written to the child are part of the journal (`input`).
6. "Disabling echo": term keeps the kernel's cooked defaults (including ECHO); the only bytes ever written
   to a child terminal are query replies.


## macOS results from CI (2026-10-04)

The first macOS CI runs (GitHub `macos-latest`, arm64) showed:

- A PTY stream ends with EOF, not EIO, once the slave side is closed; every
  byte was still captured (the 2000-run tail test passes with EOF accepted).
- The master reports EOF as soon as the session leader exits, even while a
  SIGHUP-ignoring descendant holds the slave, so that descendant's later
  output is not captured. Tests for that Linux behavior skip on darwin.
- The non-blocking re-wrap is Linux-only: on macOS read deadlines on the
  master are ignored, so the Linux evidence tests that depend on them skip.
- BSD `ls` color output differs from GNU's; that TTY probe runs on Linux only.
