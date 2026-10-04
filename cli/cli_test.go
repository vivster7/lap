package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivster7/lap"
)

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644)
	return dir
}

func cfg() lap.Config {
	return lap.Config{Name: "dev", NoPool: true, Nice: -1, Cleanup: 300 * time.Millisecond, Tasks: []lap.Task{
		{Name: "ok", Groups: []string{"lint"}, Variants: []lap.Variant{{Name: "default", Cmd: lap.Shell("echo all good")}}},
		{Name: "bad", Groups: []string{"lint"}, Variants: []lap.Variant{{Name: "default", Cmd: lap.Shell("echo 'x.txt:1: broken'; exit 1")}}, Classify: lap.ClassifyFileLine()},
	}}
}

func TestRunLogsWhyStats(t *testing.T) {
	dir := repo(t)
	t.Chdir(dir)
	var out, errb bytes.Buffer
	if code := Run(context.Background(), cfg(), []string{"--color", "never"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errb.String())
	}
	for _, want := range []string{"✓ ok", "✗ bad", "x.txt:1: broken", "dev in"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("run output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := Run(context.Background(), cfg(), []string{"logs", "ok"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "all good") {
		t.Errorf("logs: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Run(context.Background(), cfg(), []string{"why"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "findings") {
		t.Errorf("why: %d %q", code, out.String())
	}
	out.Reset()
	if code := Run(context.Background(), cfg(), []string{"stats"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "bad") {
		t.Errorf("stats: %d %q", code, out.String())
	}
	out.Reset()
	if code := Run(context.Background(), cfg(), []string{"ok", "--json"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"exit_code": 0`) {
		t.Errorf("json select: %d %q", code, out.String())
	}
	if code := Run(context.Background(), cfg(), []string{"nope"}, &out, &errb); code != 3 {
		t.Errorf("unknown selection: exit %d", code)
	}
}
