//go:build unix

package term

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Q3: tools behave as in a terminal under PTY mode and as non-interactive
// under pipes; the mixed mode gives each stream its own answer.
func TestTTYDetection(t *testing.T) {
	need(t, "python3", "node", "ls", "grep")
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "subdir"), 0o755)

	type probe struct {
		name   string
		argv   []string
		stream map[Mode]string // which stream to inspect
		// what to look for in that stream per mode ("" = expect no ESC)
		want map[Mode]string
	}
	ptyAll := map[Mode]string{PTY: StreamPTY, Pipes: StreamStdout, StdoutPipeStderrPTY: StreamStdout}
	ptyErr := map[Mode]string{PTY: StreamPTY, Pipes: StreamStderr, StdoutPipeStderrPTY: StreamStderr}
	probes := []probe{
		{"python isatty(0,1,2)", []string{"python3", "-c", "import sys;print('isatty', sys.stdin.isatty(), sys.stdout.isatty(), sys.stderr.isatty())"},
			ptyAll, map[Mode]string{PTY: "isatty False True True", Pipes: "isatty False False False", StdoutPipeStderrPTY: "isatty False False True"}},
		{"node isTTY(1,2)", []string{"node", "-e", "console.log('isTTY', !!process.stdout.isTTY, !!process.stderr.isTTY)"},
			ptyAll, map[Mode]string{PTY: "isTTY \x1b[33mtrue\x1b[39m \x1b[33mtrue", Pipes: "isTTY false false", StdoutPipeStderrPTY: "isTTY false true"}},
		{"node console.log colors", []string{"node", "-e", "console.log({n: 1})"},
			ptyAll, map[Mode]string{PTY: "\x1b[33m1\x1b[39m", Pipes: "", StdoutPipeStderrPTY: ""}},
		{"ls --color=auto", []string{"ls", "--color=auto", dir},
			ptyAll, map[Mode]string{PTY: "\x1b[01;34msubdir\x1b[0m", Pipes: "", StdoutPipeStderrPTY: ""}},
		{"grep --color=auto", []string{"sh", "-c", "echo hello | grep --color=auto ell"},
			ptyAll, map[Mode]string{PTY: "ell\x1b[m", Pipes: "", StdoutPipeStderrPTY: ""}},
		{"python traceback (stderr)", []string{"python3", "-c", "raise ValueError('boom')"},
			ptyErr, map[Mode]string{PTY: "\x1b[", Pipes: "", StdoutPipeStderrPTY: "\x1b["}},
	}
	for _, p := range probes {
		for _, m := range []Mode{PTY, Pipes, StdoutPipeStderrPTY} {
			_, err, c := run(t, Spec{Mode: m}, 5*time.Second, p.argv...)
			if err != nil {
				t.Fatalf("%s/%v: %v", p.name, m, err)
			}
			got := streamOf(t, c, p.stream[m])
			want := p.want[m]
			ok := false
			if want == "" {
				ok = !strings.Contains(got, "\x1b")
			} else {
				ok = strings.Contains(got, want)
			}
			if !ok {
				t.Errorf("%-26s %-22v want %q in %s, got %q", p.name, m, want, p.stream[m], got)
			} else {
				t.Logf("%-26s %-22v %s: %q", p.name, m, p.stream[m], firstLine(got))
			}
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Q6: terminal queries. Without a reply, a child that waits without a
// timeout hangs; with QueriesAnswer it gets a correct cursor position from
// the live emulator.
func TestTerminalQueries(t *testing.T) {
	need(t, "python3")
	t.Run("answered, stdin=/dev/null (child opens /dev/tty)", func(t *testing.T) {
		sum, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "python3", fixture("query.py"), "none")
		out := string(rawOf(t, c))
		// "before" is 6 cells: the cursor is at row 1, column 7.
		if err != nil || !strings.Contains(out, "REPLY 1;7") || sum.Replies != 1 {
			t.Fatalf("out=%q err=%v sum=%+v", out, err, sum)
		}
	})
	t.Run("answered, stdin=tty", func(t *testing.T) {
		_, err, c := run(t, Spec{Mode: PTY, Stdin: StdinTTY}, 5*time.Second, "python3", fixture("query.py"), "none")
		if out := string(rawOf(t, c)); err != nil || !strings.Contains(out, "REPLY 1;7") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("answered on the stderr PTY in mixed mode", func(t *testing.T) {
		// query.py writes the query to stdout; route it to stderr instead.
		_, err, c := run(t, Spec{Mode: StdoutPipeStderrPTY}, 5*time.Second, "sh", "-c", "python3 "+fixture("query.py")+" none 1>&2")
		if out := streamOf(t, c, StreamStderr); err != nil || !strings.Contains(out, "REPLY 1;7") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("ignored, child with timeout", func(t *testing.T) {
		start := time.Now()
		_, err, c := run(t, Spec{Mode: PTY, Queries: QueriesIgnore}, 5*time.Second, "python3", fixture("query.py"), "0.5")
		if out := string(rawOf(t, c)); err != nil || !strings.Contains(out, "NO-REPLY") {
			t.Fatalf("out=%q err=%v", out, err)
		}
		t.Logf("child gave up after its own timeout: %v", time.Since(start).Round(10*time.Millisecond))
	})
	t.Run("ignored, child without timeout hangs", func(t *testing.T) {
		s := openTest(t, Spec{Mode: PTY, Queries: QueriesIgnore})
		cmd := launch(t, s, "python3", fixture("query.py"), "none")
		exited := make(chan struct{})
		go func() { cmd.Wait(); close(exited) }()
		select {
		case <-exited:
			t.Fatal("child exited without a reply")
		case <-time.After(1500 * time.Millisecond):
		}
		// Cancellation (proc's job) ends it; the drain then completes.
		cmd.Process.Kill()
		<-exited
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := s.Done(ctx); err != nil {
			t.Fatal(err)
		}
		t.Log("child still blocked after 1.5s without a reply; killed")
	})
}

// Resize delivers SIGWINCH and is journaled in stream order.
func TestResize(t *testing.T) {
	need(t, "python3")
	s := openTest(t, Spec{Mode: PTY, Live: true})
	cmd := launch(t, s, "python3", fixture("winsize.py"))
	deadline := time.After(5 * time.Second)
	for !strings.Contains(s.Screen(), "size 80x24") {
		select {
		case <-s.Updates():
		case <-deadline:
			t.Fatal("no first size line")
		}
	}
	if err := s.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Done(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := OpenCapture(s.Dir())
	if out := string(rawOf(t, c)); !strings.Contains(out, "size 100x30") {
		t.Fatalf("raw=%q", out)
	}
	var sawResize bool
	for _, ev := range events(t, c) {
		if ev.T == "resize" && ev.Cols == 100 && ev.Rows == 30 && ev.Off == int64(len("size 80x24\r\n")) {
			sawResize = true
		}
	}
	if !sawResize {
		t.Fatalf("resize not journaled at offset 12: %+v", events(t, c))
	}
	_, info := renderedOf(t, c)
	if info.Cols != 100 || info.Rows != 30 || info.Resizes != 1 {
		t.Fatalf("render info %+v", info)
	}
}
