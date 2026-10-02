package run

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivster7/lap/term"
)

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
}

func plain(t *testing.T, dir string) string {
	t.Helper()
	c, err := term.OpenCapture(dir)
	if err != nil {
		t.Fatalf("OpenCapture: %v", err)
	}
	var b bytes.Buffer
	if err := c.Plain(&b, term.PlainOptions{}); err != nil {
		t.Fatalf("Plain: %v", err)
	}
	return b.String()
}

func command(t *testing.T, mode term.Mode, argv ...string) Command {
	return Command{
		Argv:    argv,
		Capture: term.Spec{Mode: mode, Dir: filepath.Join(t.TempDir(), "capture")},
	}
}

// The child sees a terminal in PTY mode and not in pipe mode; output is
// captured completely and cleanup is verified.
func TestTTYDetectionAndCleanCompletion(t *testing.T) {
	needPython(t)
	for _, tc := range []struct {
		mode term.Mode
		want string
	}{
		{term.PTY, "tty=True"},
		{term.Pipes, "tty=False"},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			c := command(t, tc.mode, "python3", "-c", "import sys; print('tty=%s' % sys.stdout.isatty())")
			res, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if !res.OK() {
				t.Fatalf("not OK: exited=%v exit=%v verified=%v captureErr=%v", res.Exited, res.Exit, res.Cleanup.Verified, res.CaptureErr)
			}
			if got := plain(t, c.Capture.Dir); !strings.Contains(got, tc.want) {
				t.Fatalf("plain view %q does not contain %q", got, tc.want)
			}
		})
	}
}

// A deadline kills a process tree that ignores SIGTERM and SIGHUP; output
// written before the deadline is kept, and nothing survives.
func TestDeadlineKillsResistantTree(t *testing.T) {
	needPython(t)
	script := `
import os, signal, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
signal.signal(signal.SIGHUP, signal.SIG_IGN)
if os.fork() == 0:
    print("grandchild started", flush=True)
    time.sleep(30)
    sys.exit(0)
print("child started", flush=True)
time.sleep(30)
`
	for _, mode := range []term.Mode{term.PTY, term.Pipes} {
		t.Run(mode.String(), func(t *testing.T) {
			c := command(t, mode, "python3", "-c", script)
			c.TermGrace = 200 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			res, err := Run(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			if !res.TimedOut || res.Exited {
				t.Fatalf("want timeout, got exited=%v timedOut=%v", res.Exited, res.TimedOut)
			}
			if !res.Cleanup.SentKill {
				t.Errorf("expected SIGKILL escalation for a TERM-ignoring tree")
			}
			if !res.Cleanup.Verified || len(res.Cleanup.Survivors) > 0 {
				t.Fatalf("cleanup not verified: survivors=%v err=%v", res.Cleanup.Survivors, res.Cleanup.Err)
			}
			if res.Elapsed > 3*time.Second {
				t.Errorf("run took %v; deadline 700ms + term grace 200ms", res.Elapsed)
			}
			got := plain(t, c.Capture.Dir)
			for _, want := range []string{"child started", "grandchild started"} {
				if !strings.Contains(got, want) {
					t.Errorf("plain view %q missing %q", got, want)
				}
			}
		})
	}
}

// In pipe mode a background job keeps stdout open after the direct child
// exits. The run must not wait for it: after the tail grace the group is
// terminated, and the capture then completes.
func TestBackgroundJobHoldingPipe(t *testing.T) {
	c := command(t, term.Pipes, "sh", "-c", "sleep 30 & echo parent-done")
	res, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Exited || res.Exit.Code != 0 {
		t.Fatalf("want clean exit, got exited=%v exit=%v", res.Exited, res.Exit)
	}
	if !res.TailHeld {
		t.Errorf("expected the background job to hold the pipe past the tail grace")
	}
	if !res.Cleanup.Verified {
		t.Fatalf("cleanup not verified: survivors=%v", res.Cleanup.Survivors)
	}
	if res.CaptureErr != nil {
		t.Errorf("capture should complete once the holder is killed: %v", res.CaptureErr)
	}
	if res.Elapsed > 2*time.Second {
		t.Errorf("run took %v; should not wait for the background job", res.Elapsed)
	}
	if got := plain(t, c.Capture.Dir); !strings.Contains(got, "parent-done") {
		t.Errorf("plain view %q missing parent-done", got)
	}
}

func TestMergeEnvOverridesWithoutDuplicates(t *testing.T) {
	got := mergeEnv([]string{"A=1", "TERM=dumb", "B=2"}, []string{"TERM=xterm-256color"})
	want := []string{"A=1", "B=2", "TERM=xterm-256color"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}
