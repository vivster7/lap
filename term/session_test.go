//go:build unix

package term

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Q2: PTY output post-processing (ONLCR) is preserved in the primary record;
// pipes are untouched.
func TestLineEndingsPreserved(t *testing.T) {
	sum, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "printf", `a\nb\n`)
	if err != nil {
		t.Fatal(err, sum)
	}
	if got := string(rawOf(t, c)); got != "a\r\nb\r\n" {
		t.Fatalf("pty raw = %q, want ONLCR CRLF", got)
	}
	if !ptyEnded(sum.Streams[0].End) {
		t.Fatalf("pty end = %q, want hangup", sum.Streams[0].End)
	}
	if got := plainOf(t, c, PlainOptions{}); got != "a\nb\n" {
		t.Fatalf("plain = %q", got)
	}

	sum, err, c = run(t, Spec{Mode: Pipes}, 5*time.Second, "sh", "-c", `printf 'out\n'; printf 'err\n' >&2`)
	if err != nil {
		t.Fatal(err, sum)
	}
	if got := streamOf(t, c, StreamStdout); got != "out\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := streamOf(t, c, StreamStderr); got != "err\n" {
		t.Fatalf("stderr = %q", got)
	}
	for _, st := range sum.Streams {
		if st.End != EndEOF {
			t.Fatalf("%s end = %q", st.Name, st.End)
		}
	}
}

// Q1: the child writes and exits immediately; the final bytes must never be
// lost, and the PTY's EIO must become a clean hangup EOF.
func TestPTYTailNeverLost(t *testing.T) {
	need(t, "python3")
	sizes := []int{0, 1, 100, 4095, 4096, 4097, 65536, 1 << 20}
	iters := 200
	if testing.Short() {
		iters = 40
	}
	var mu sync.Mutex
	failures := 0
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := 0; i < iters; i++ {
		size := sizes[i%len(sizes)]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// os._exit skips any flushing/cleanup: write(2) then exit(2).
			script := fmt.Sprintf("import os\nb=b'x'*%d+b'END'\nn=0\nwhile n<len(b): n+=os.write(1,b[n:])\nos._exit(0)", size)
			sum, err, c := run(t, Spec{Mode: PTY}, 10*time.Second, "python3", "-c", script)
			got := rawOf(t, c)
			want := strings.Repeat("x", size) + "END"
			if err != nil || string(got) != want || !ptyEnded(sum.Streams[0].End) || !sum.Complete {
				mu.Lock()
				failures++
				mu.Unlock()
				t.Errorf("size %d: got %d bytes (tail %q), end=%s err=%v", size, len(got), tail(got), sum.Streams[0].End, err)
			}
		}()
	}
	wg.Wait()
	t.Logf("%d iterations, %d failures", iters, failures)
}

// Q1: the same with a shell pipeline whose last writer is a grandchild, and
// with output written by a process that exits while the parent still reads.
func TestPTYTailPipeline(t *testing.T) {
	for i := 0; i < 20; i++ {
		sum, err, c := run(t, Spec{Mode: PTY}, 10*time.Second, "sh", "-c", `head -c 300000 /dev/zero | tr '\0' x; printf END`)
		got := rawOf(t, c)
		if err != nil || len(got) != 300003 || !bytes.HasSuffix(got, []byte("END")) {
			t.Fatalf("iter %d: %d bytes, err=%v sum=%+v", i, len(got), err, sum)
		}
	}
}

func tail(b []byte) []byte {
	if len(b) > 8 {
		return b[len(b)-8:]
	}
	return b
}

// Q1 evidence: the EIO/hangup only happens once *every* slave fd is closed,
// including the parent's copy. Without CloseChildEnds the master read blocks.
func TestPTYHangupNeedsParentSlaveClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		// Needs read deadlines on the master, which only the Linux
		// non-blocking re-wrap provides (see SPIKE.md open issues).
		t.Skip("Linux-specific evidence test")
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if master, err = pollableMaster(master); err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	cmd := exec.Command("printf", "hi")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	buf := make([]byte, 64)
	n, err := master.Read(buf)
	if string(buf[:n]) != "hi" || err != nil {
		t.Fatalf("first read %q %v", buf[:n], err)
	}
	master.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = master.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("with parent slave open: want blocked read (deadline), got %v", err)
	}
	slave.Close()
	master.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = master.Read(buf)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("after closing last slave fd: want EIO, got %v", err)
	}
	t.Logf("master read after last slave close: %v (EIO, mapped to hangup)", err)
}

// Q1: grandchildren holding the terminal open after the child exits.
func TestPTYGrandchild(t *testing.T) {
	t.Run("background job gets SIGHUP when the session leader exits", func(t *testing.T) {
		start := time.Now()
		sum, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "sh", "-c", `(sleep 1; echo late) & echo early`)
		el := time.Since(start)
		raw := string(rawOf(t, c))
		if err != nil || !strings.Contains(raw, "early") || strings.Contains(raw, "late") || el > 900*time.Millisecond {
			t.Fatalf("raw=%q err=%v elapsed=%v sum=%+v", raw, err, el, sum)
		}
		t.Logf("drained in %v; background job was hung up (no 'late')", el.Round(time.Millisecond))
	})
	t.Run("HUP-ignoring grandchild keeps the slave open; Done waits for it", func(t *testing.T) {
		skipOnDarwinLeaderExit(t)
		start := time.Now()
		sum, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "sh", "-c", `trap '' HUP; (sleep 1; echo late) & echo early`)
		el := time.Since(start)
		raw := string(rawOf(t, c))
		if err != nil || !strings.Contains(raw, "late") || el < 900*time.Millisecond {
			t.Fatalf("raw=%q err=%v elapsed=%v sum=%+v", raw, err, el, sum)
		}
		t.Logf("drained in %v including the grandchild's late write", el.Round(time.Millisecond))
	})
	t.Run("grandchild outlives the drain deadline", func(t *testing.T) {
		skipOnDarwinLeaderExit(t)
		s := openTest(t, Spec{Mode: PTY})
		cmd := launch(t, s, "sh", "-c", `trap '' HUP; sleep 30 & echo early`)
		cmd.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		sum, err := s.Done(ctx)
		el := time.Since(start)
		if !errors.Is(err, ErrIncomplete) || sum.Complete || sum.Streams[0].End != EndOpen || el > 700*time.Millisecond {
			t.Fatalf("err=%v elapsed=%v sum=%+v", err, el, sum)
		}
		c, _ := OpenCapture(s.Dir())
		if !strings.Contains(string(rawOf(t, c)), "early") {
			t.Fatal("lost output before the deadline")
		}
		var inc []Event
		for _, ev := range events(t, c) {
			if ev.T == "incomplete" {
				inc = append(inc, ev)
			}
		}
		if len(inc) != 1 || inc[0].N != -1 {
			t.Fatalf("incomplete events %+v", inc)
		}
		t.Logf("Done returned after %v: %v", el.Round(time.Millisecond), err)
	})
	t.Run("recommended: tail grace, kill the group, then Done", func(t *testing.T) {
		skipOnDarwinLeaderExit(t)
		s := openTest(t, Spec{Mode: PTY})
		cmd := launch(t, s, "sh", "-c", `trap '' HUP; sleep 30 & echo early`)
		cmd.Wait()
		select {
		case <-s.Drained():
			t.Fatal("drained although a descendant holds the terminal")
		case <-time.After(200 * time.Millisecond):
			// proc's job: terminate the owned session/group.
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sum, err := s.Done(ctx)
		if err != nil || !sum.Complete || sum.Streams[0].End != EndHangup {
			t.Fatalf("err=%v sum=%+v", err, sum)
		}
	})
}

func TestReadErrorsStayVisible(t *testing.T) {
	s := &Session{abort: make(chan struct{})}
	tty := &stream{tty: true}
	pipe := &stream{}
	if e, _ := s.classify(tty, syscall.EIO); e != EndHangup {
		t.Fatal("pty EIO must be hangup")
	}
	if e, err := s.classify(pipe, syscall.EIO); e != EndError || err == nil {
		t.Fatal("pipe EIO must stay an error")
	}
	if e, err := s.classify(tty, syscall.EBADF); e != EndError || err == nil {
		t.Fatal("other pty errors must stay errors")
	}
	if e, _ := s.classify(pipe, io.EOF); e != EndEOF {
		t.Fatal("EOF")
	}
}

// Pipes do not hang up: any descendant holding the write end keeps the
// stream open, even an ordinary background job.
func TestPipesGrandchild(t *testing.T) {
	s := openTest(t, Spec{Mode: Pipes})
	cmd := launch(t, s, "sh", "-c", `sleep 30 & echo early`)
	cmd.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sum, err := s.Done(ctx)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err=%v sum=%+v", err, sum)
	}
	for _, st := range sum.Streams {
		if st.End != EndOpen {
			t.Fatalf("%s end=%s", st.Name, st.End)
		}
	}
}

func TestDoneWithoutLaunch(t *testing.T) {
	for _, m := range []Mode{PTY, Pipes, StdoutPipeStderrPTY} {
		s := openTest(t, Spec{Mode: m})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		sum, err := s.Done(ctx)
		cancel()
		if err != nil || !sum.Complete || sum.Received != 0 {
			t.Fatalf("%v: err=%v sum=%+v", m, err, sum)
		}
	}
}

// failAfter simulates a full disk: writes fail after n bytes.
type failAfter struct {
	w io.Writer
	n int64
}

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, syscall.ENOSPC
	}
	if int64(len(p)) > f.n {
		k, _ := f.w.Write(p[:f.n])
		f.n -= int64(k)
		return k, syscall.ENOSPC
	}
	k, err := f.w.Write(p)
	f.n -= int64(k)
	return k, err
}

// Q8: a storage failure must not stop draining (the child must not block),
// and the missing range must be explicit.
func TestDiskFailureKeepsDraining(t *testing.T) {
	for _, m := range []Mode{PTY, Pipes} {
		t.Run(m.String(), func(t *testing.T) {
			const limit = 100_000
			spec := Spec{Mode: m, wrapOutput: func(w io.Writer) io.Writer { return &failAfter{w: w, n: limit} }}
			start := time.Now()
			// 4 MB, far more than pipe/PTY buffers: if draining stopped, the
			// child would block and the drain deadline would expire.
			sum, err, c := run(t, spec, 10*time.Second, "sh", "-c", `head -c 4000000 /dev/zero | tr '\0' x`)
			if !errors.Is(err, ErrIncomplete) || sum.Complete {
				t.Fatalf("want ErrIncomplete, got %v %+v", err, sum)
			}
			if sum.Received != 4_000_000 || sum.Stored != limit {
				t.Fatalf("received=%d stored=%d", sum.Received, sum.Stored)
			}
			if len(sum.Incomplete) != 1 || sum.Incomplete[0].Off != limit || sum.Incomplete[0].N != 4_000_000-limit {
				t.Fatalf("incomplete=%+v", sum.Incomplete)
			}
			for _, st := range sum.Streams {
				if st.End != EndEOF && st.End != EndHangup {
					t.Fatalf("stream %s ended %s: draining stopped", st.Name, st.End)
				}
			}
			if got := len(rawOf(t, c)); got != limit {
				t.Fatalf("output.bytes = %d", got)
			}
			var sawWriteErr, sawIncomplete bool
			for _, ev := range events(t, c) {
				sawWriteErr = sawWriteErr || ev.T == "write_error"
				sawIncomplete = sawIncomplete || (ev.T == "incomplete" && ev.Off == limit && ev.N == 4_000_000-limit)
			}
			if !sawWriteErr || !sawIncomplete {
				t.Fatal("journal lacks write_error/incomplete events")
			}
			if cs, ok := c.Summary(); !ok || cs.Complete {
				t.Fatal("capture.json must say incomplete")
			}
			t.Logf("%v: drained 4 MB in %v after the disk failed at %d bytes; %v", m, time.Since(start).Round(time.Millisecond), limit, sum.Problems)
		})
	}
}

// A crash leaves no capture.json, maybe a partial journal line and bytes that
// were stored but not journaled. Readers must cope and say so.
func TestCrashLeftovers(t *testing.T) {
	sum, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "printf", `one\ntwo\n`)
	if err != nil {
		t.Fatal(err, sum)
	}
	dir := c.Dir
	os.Remove(filepath.Join(dir, "capture.json"))
	ev, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	// Drop the eof/end lines and leave a partial line.
	lines := strings.SplitAfter(string(ev), "\n")
	var keep []string
	for _, l := range lines {
		if strings.Contains(l, `"t":"chunk"`) || strings.Contains(l, `"t":"start"`) {
			keep = append(keep, l)
		}
	}
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(keep, "")+`{"t":"chunk","s":"pty","of`), 0o644)
	f, _ := os.OpenFile(filepath.Join(dir, "output.bytes"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("three\r\n")
	f.Close()

	c2, err := OpenCapture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.Summary(); ok {
		t.Fatal("unfinalized capture must not report a summary")
	}
	var unj string
	err = c2.Replay(func(ev Event, data []byte) error {
		if ev.S == StreamUnjournaled {
			unj += string(data)
		}
		return nil
	})
	if !errors.Is(err, ErrTruncatedJournal) || unj != "three\r\n" {
		t.Fatalf("err=%v unjournaled=%q", err, unj)
	}
	var pb bytes.Buffer
	if err := c2.Plain(&pb, PlainOptions{}); !errors.Is(err, ErrTruncatedJournal) || pb.String() != "one\ntwo\nthree\n" {
		t.Fatalf("plain=%q err=%v", pb.String(), err)
	}
}

// Evidence for pollableMaster: creack/pty v1.1.24 hands back a master that is
// already in blocking mode (its ioctl helper calls Fd()). SetReadDeadline
// succeeds but has no effect: Read sits in a blocking read(2), so a reader
// cannot be interrupted at the drain deadline.
func TestCreackMasterIsBlocking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the non-blocking re-wrap is Linux-only; macOS masters stay blocking (open issue)")
	}
	check := func(m *os.File) (returned bool) {
		m.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		done := make(chan error, 1)
		go func() { _, err := m.Read(make([]byte, 16)); done <- err }()
		select {
		case <-done:
			return true
		case <-time.After(500 * time.Millisecond):
			return false
		}
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if check(master) {
		t.Fatal("creack master honoured the read deadline; pollableMaster may be unnecessary")
	}
	slave.Close() // unblocks the stuck read with EIO
	master.Close()

	master, slave, err = pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	m, err := pollableMaster(master)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if !check(m) {
		t.Fatal("pollable master ignored the read deadline")
	}
	t.Log("creack/pty master: deadline ignored (blocking read); after pollableMaster: deadline honoured")
}

// ptyEnded reports whether a PTY stream ended normally for this platform:
// Linux reports EIO (hangup) once every slave copy is closed; macOS returns a
// plain EOF.
func ptyEnded(end string) bool {
	if runtime.GOOS == "darwin" {
		return end == EndHangup || end == EndEOF
	}
	return end == EndHangup
}

// skipOnDarwinLeaderExit skips tests of descendants that outlive the session
// leader: on macOS the master reports EOF as soon as the leader exits, even
// while a descendant still holds the slave, so such output is not captured
// there (documented open issue).
func skipOnDarwinLeaderExit(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("macOS: master EOF at session-leader exit (open issue)")
	}
}
