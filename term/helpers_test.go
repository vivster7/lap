//go:build unix

package term

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// launch is a minimal stand-in for package proc: it starts argv with the
// session's child ends and launch attributes, then closes the parent's copies.
// The process group is killed at test cleanup.
func launch(t testing.TB, s *Session, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.ChildStdio()
	cmd.Env = testEnv(s.Env())
	a := s.LaunchAttrs()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: a.Setsid, Setctty: a.Setctty, Ctty: a.CttyFD, Setpgid: a.Setpgid}
	if err := cmd.Start(); err != nil {
		s.CloseChildEnds()
		t.Fatalf("start %v: %v", argv, err)
	}
	if err := s.CloseChildEnds(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		// Setsid/Setpgid make the child its own group leader: kill the group,
		// including any grandchildren left in it.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

// testEnv builds a deterministic environment: color overrides from the
// developer's shell are removed so TTY detection is what decides.
func testEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "TERM", "NO_COLOR", "FORCE_COLOR", "CLICOLOR_FORCE", "CLICOLOR", "PYTHON_COLORS", "COLORTERM", "LS_COLORS", "COLUMNS", "LINES", "NODE_DISABLE_COLORS":
			continue
		}
		env = append(env, kv)
	}
	if len(extra) == 0 {
		env = append(env, "TERM=dumb") // pipe mode: what a non-interactive runner typically has
	}
	return append(env, extra...)
}

func openTest(t testing.TB, spec Spec) *Session {
	t.Helper()
	if spec.Dir == "" {
		spec.Dir = filepath.Join(t.TempDir(), "cap")
	}
	s, err := Open(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// run launches argv, waits for exit, then drains with the given deadline.
func run(t testing.TB, spec Spec, drain time.Duration, argv ...string) (Summary, error, *Capture) {
	t.Helper()
	s := openTest(t, spec)
	cmd := launch(t, s, argv...)
	_ = cmd.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	sum, err := s.Done(ctx)
	c, oerr := OpenCapture(s.Dir())
	if oerr != nil {
		t.Fatal(oerr)
	}
	return sum, err, c
}

func rawOf(t testing.TB, c *Capture) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.Dir, "output.bytes"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func streamOf(t testing.TB, c *Capture, name string) string {
	t.Helper()
	var b bytes.Buffer
	if err := c.WriteStream(&b, name); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func plainOf(t testing.TB, c *Capture, opt PlainOptions) string {
	t.Helper()
	var b bytes.Buffer
	if err := c.Plain(&b, opt); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func renderedOf(t testing.TB, c *Capture) (string, RenderInfo) {
	t.Helper()
	var b bytes.Buffer
	info, err := c.Rendered(&b, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return b.String(), info
}

func events(t testing.TB, c *Capture) []Event {
	t.Helper()
	var evs []Event
	if err := c.Replay(func(ev Event, _ []byte) error { evs = append(evs, ev); return nil }); err != nil {
		t.Fatal(err)
	}
	return evs
}

func fixture(name string) string { return filepath.Join("testdata", name) }

func need(t testing.TB, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
}
