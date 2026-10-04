package adapters

import (
	"reflect"
	"testing"

	"github.com/vivster7/lap"
)

func TestDirsOf(t *testing.T) {
	got := dirsOf([]string{"a/b/c.go", "a/b/d.go", "main.go", "x/y.go"})
	want := []string{".", "./a/b", "./x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCommandsHonorModeAndScope(t *testing.T) {
	inv := lap.Invocation{Root: t.TempDir(), Mode: lap.CheckOnly, Files: []string{"a.py"}}
	argv, _ := RuffFormat().Variants[0].Cmd(inv)
	if !reflect.DeepEqual(argv, []string{"ruff", "format", "--check", "--diff", "a.py"}) {
		t.Errorf("ruff format check: %v", argv)
	}
	inv.Mode, inv.All, inv.Files = lap.Fix, true, nil
	argv, _ = Gofmt().Variants[0].Cmd(inv)
	if !reflect.DeepEqual(argv, []string{"gofmt", "-l", "-w", "."}) {
		t.Errorf("gofmt fix all: %v", argv)
	}
	argv, _ = GoTest().Variants[1].Cmd(inv)
	if argv != nil {
		t.Errorf("changed variant in full scope should be skipped: %v", argv)
	}
}

func TestRegexes(t *testing.T) {
	if m := ruffRE.FindStringSubmatch("pkg/mod.py:12:5: F401 `os` imported but unused"); m == nil || m[4] != "F401" {
		t.Errorf("ruff: %v", m)
	}
	if m := tscRE.FindStringSubmatch("src/a.ts(3,7): error TS2322: Type 'number' is not assignable"); m == nil || m[4] != "TS2322" {
		t.Errorf("tsc: %v", m)
	}
}
