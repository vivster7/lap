package scope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain isolates git from the user's and system configuration. The
// environment applies to the git commands Resolve runs, too.
func TestMain(m *testing.M) {
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES"} {
		os.Unsetenv(k)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "lap test",
		"GIT_AUTHOR_EMAIL":    "lap@example.invalid",
		"GIT_COMMITTER_NAME":  "lap test",
		"GIT_COMMITTER_EMAIL": "lap@example.invalid",
		"GIT_CONFIG_COUNT":    "2",
		"GIT_CONFIG_KEY_0":    "init.defaultBranch",
		"GIT_CONFIG_VALUE_0":  "main",
		"GIT_CONFIG_KEY_1":    "protocol.file.allow",
		"GIT_CONFIG_VALUE_1":  "always",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

func gitT(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t testing.TB, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realTemp returns a temp dir with symlinks resolved (macOS /var -> /private/var),
// matching what git reports.
func realTemp(t testing.TB) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type fixture struct {
	origin, work string
	branchPoint  string // main commit the feature branched from
}

// newFixture builds a bare "origin" and a clone-like work repo on branch
// feature with every kind of change. origin/main has advanced past the
// branch point, so only a merge-base comparison gives the right answer.
func newFixture(t *testing.T) fixture {
	t.Helper()
	tmp := realTemp(t)
	f := fixture{origin: filepath.Join(tmp, "origin.git"), work: filepath.Join(tmp, "work")}
	gitT(t, tmp, "init", "-q", "--bare", f.origin)
	gitT(t, tmp, "init", "-q", f.work)
	w := f.work
	write(t, w, ".gitignore", "*.log\nbuild/\n")
	write(t, w, "a.py", "a\n")
	write(t, w, "b.go", "package b\n")
	write(t, w, "old name.txt", "rename me\n")
	write(t, w, "gone.txt", "delete me\n")
	write(t, w, "lib/keep.py", "keep\n")
	write(t, w, "docs/readme.md", "docs\n")
	gitT(t, w, "add", "-A")
	gitT(t, w, "commit", "-q", "-m", "initial")
	f.branchPoint = gitT(t, w, "rev-parse", "HEAD")
	gitT(t, w, "remote", "add", "origin", f.origin)
	gitT(t, w, "push", "-q", "-u", "origin", "main")
	gitT(t, w, "remote", "set-head", "origin", "main")

	// Feature branch: committed changes.
	gitT(t, w, "checkout", "-q", "-b", "feature")
	write(t, w, "a.py", "a changed\n")
	write(t, w, "dir with space/new file.py", "new\n")
	gitT(t, w, "add", "-A")
	gitT(t, w, "commit", "-q", "-m", "feature work")

	// main advances on origin after the branch point.
	gitT(t, w, "checkout", "-q", "main")
	write(t, w, "main_only.txt", "main\n")
	gitT(t, w, "add", "-A")
	gitT(t, w, "commit", "-q", "-m", "main moves on")
	gitT(t, w, "push", "-q", "origin", "main")
	gitT(t, w, "checkout", "-q", "feature")

	// Staged, unstaged, untracked, ignored, deleted, renamed.
	write(t, w, "staged.py", "staged\n")
	gitT(t, w, "add", "staged.py")
	write(t, w, "b.go", "package b // unstaged\n")
	write(t, w, "untracked.txt", "u\n")
	write(t, w, "sp ace/un tracked.md", "u\n")
	write(t, w, "nl\nfile.py", "newline in name\n")
	write(t, w, "debug.log", "ignored\n")
	write(t, w, "build/out.js", "ignored\n")
	gitT(t, w, "rm", "-q", "gone.txt")
	if err := os.Remove(filepath.Join(w, "lib", "keep.py")); err != nil {
		t.Fatal(err)
	}
	gitT(t, w, "mv", "old name.txt", "new name.txt")
	return f
}

func resolve(t *testing.T, opt Options) *Scope {
	t.Helper()
	s, err := Resolve(context.Background(), opt)
	if err != nil {
		t.Fatalf("Resolve(%+v): %v", opt, err)
	}
	return s
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got  %q\n want %q", what, got, want)
	}
}

var (
	wantChanged = []string{
		"a.py", "b.go", "dir with space/new file.py", "new name.txt", "nl\nfile.py",
		"sp ace/un tracked.md", "staged.py", "untracked.txt",
	}
	wantDeleted = []string{"gone.txt", "lib/keep.py", "old name.txt"}
)

func TestResolveChanged(t *testing.T) {
	f := newFixture(t)
	s := resolve(t, Options{Root: filepath.Join(f.work, "docs")})
	if s.Root != f.work {
		t.Errorf("Root = %q, want %q", s.Root, f.work)
	}
	if s.All {
		t.Error("All = true")
	}
	if s.BaseRef != "origin/main" || s.Base != f.branchPoint {
		t.Errorf("base = %q from %q, want %q from origin/main", s.Base, s.BaseRef, f.branchPoint)
	}
	eq(t, "Files", s.Files, wantChanged)
	eq(t, "Deleted", s.Deleted, wantDeleted)
	if s.Empty() {
		t.Error("Empty() = true")
	}
}

func TestAutoBaseFallbacks(t *testing.T) {
	f := newFixture(t)

	// No origin/HEAD: falls back to origin/main.
	gitT(t, f.work, "remote", "set-head", "origin", "-d")
	s := resolve(t, Options{Root: f.work})
	if s.BaseRef != "origin/main" || s.Base != f.branchPoint {
		t.Errorf("without origin/HEAD: base %q from %q", s.Base, s.BaseRef)
	}

	// origin/HEAD pointing at another branch wins over the fallbacks.
	gitT(t, f.work, "update-ref", "refs/remotes/origin/develop", f.branchPoint)
	gitT(t, f.work, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	if s := resolve(t, Options{Root: f.work}); s.BaseRef != "origin/develop" {
		t.Errorf("origin/HEAD -> develop: BaseRef %q", s.BaseRef)
	}

	// No remote: local main.
	gitT(t, f.work, "remote", "remove", "origin")
	s = resolve(t, Options{Root: f.work})
	if s.BaseRef != "main" || s.Base != f.branchPoint {
		t.Errorf("without remote: base %q from %q", s.Base, s.BaseRef)
	}
	eq(t, "Files", s.Files, wantChanged)

	// An upstream is configured for the feature branch, but never used.
	gitT(t, f.work, "config", "branch.feature.remote", ".")
	gitT(t, f.work, "config", "branch.feature.merge", "refs/heads/feature")
	if s := resolve(t, Options{Root: f.work}); s.BaseRef != "main" {
		t.Errorf("with upstream: BaseRef %q", s.BaseRef)
	}
}

func TestExplicitBase(t *testing.T) {
	f := newFixture(t)
	s := resolve(t, Options{Root: f.work, Base: "HEAD"})
	if s.BaseRef != "HEAD" || s.Base != gitT(t, f.work, "rev-parse", "HEAD") {
		t.Errorf("base %q from %q", s.Base, s.BaseRef)
	}
	// Committed feature changes are excluded; uncommitted ones remain.
	eq(t, "Files", s.Files, []string{
		"b.go", "new name.txt", "nl\nfile.py", "sp ace/un tracked.md", "staged.py", "untracked.txt",
	})
	eq(t, "Deleted", s.Deleted, wantDeleted)

	if _, err := Resolve(context.Background(), Options{Root: f.work, Base: "no-such-ref"}); !errors.Is(err, ErrNoBase) {
		t.Errorf("bad explicit base: %v, want ErrNoBase", err)
	}
	if _, err := Resolve(context.Background(), Options{Root: f.work, Base: "--output=x"}); !errors.Is(err, ErrNoBase) {
		t.Errorf("option-like base: %v, want ErrNoBase", err)
	}
}

func TestOnDefaultBranchOnlyUncommitted(t *testing.T) {
	f := newFixture(t)
	gitT(t, f.work, "checkout", "-q", "-f", "main")
	gitT(t, f.work, "clean", "-q", "-fd")
	gitT(t, f.work, "reset", "-q", "--hard", "origin/main")
	write(t, f.work, "a.py", "edit on main\n")
	write(t, f.work, "fresh.txt", "x\n")
	s := resolve(t, Options{Root: f.work})
	if s.Base != gitT(t, f.work, "rev-parse", "HEAD") {
		t.Errorf("Base = %q, want HEAD", s.Base)
	}
	eq(t, "Files", s.Files, []string{"a.py", "fresh.txt"})
	eq(t, "Deleted", s.Deleted, nil)

	gitT(t, f.work, "checkout", "-q", "--", "a.py")
	os.Remove(filepath.Join(f.work, "fresh.txt"))
	if s := resolve(t, Options{Root: f.work}); !s.Empty() {
		t.Errorf("clean main: Files %q Deleted %q", s.Files, s.Deleted)
	}
}

func TestErrNoBase(t *testing.T) {
	dir := realTemp(t)
	gitT(t, dir, "init", "-q", "-b", "trunk")
	// No commits at all.
	if _, err := Resolve(context.Background(), Options{Root: dir}); !errors.Is(err, ErrNoBase) {
		t.Fatalf("empty repo: %v, want ErrNoBase", err)
	}
	write(t, dir, "x.txt", "x\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "x")
	// Commits, but no main/master/origin.
	if _, err := Resolve(context.Background(), Options{Root: dir}); !errors.Is(err, ErrNoBase) {
		t.Fatalf("trunk-only repo: %v, want ErrNoBase", err)
	}
	// Full scope still works.
	s := resolve(t, Options{Root: dir, All: true})
	eq(t, "Files", s.Files, []string{"x.txt"})
	if s.Base != "" || s.BaseRef != "" {
		t.Errorf("All: base %q/%q", s.Base, s.BaseRef)
	}
	// Not a repository at all.
	if _, err := Resolve(context.Background(), Options{Root: realTemp(t)}); err == nil || errors.Is(err, ErrNoBase) {
		t.Errorf("outside a repo: %v", err)
	}
}

func TestAll(t *testing.T) {
	f := newFixture(t)
	s := resolve(t, Options{Root: f.work, All: true, Base: "ignored"})
	if !s.All || s.Base != "" || s.BaseRef != "" {
		t.Errorf("All=%v base %q/%q", s.All, s.Base, s.BaseRef)
	}
	eq(t, "Files", s.Files, []string{
		".gitignore", "a.py", "b.go", "dir with space/new file.py", "docs/readme.md",
		"new name.txt", "nl\nfile.py", "sp ace/un tracked.md", "staged.py", "untracked.txt",
	})
	// Tracked but missing from the worktree.
	eq(t, "Deleted", s.Deleted, []string{"lib/keep.py"})
}

func TestSubmoduleIsAPath(t *testing.T) {
	f := newFixture(t)
	sub := filepath.Join(filepath.Dir(f.work), "subrepo")
	gitT(t, filepath.Dir(f.work), "init", "-q", sub)
	write(t, sub, "inner.py", "inner\n")
	gitT(t, sub, "add", "-A")
	gitT(t, sub, "commit", "-q", "-m", "sub")
	gitT(t, f.work, "submodule", "add", "-q", sub, "vendor/sub")

	s := resolve(t, Options{Root: f.work})
	if !slices.Contains(s.Files, "vendor/sub") || !slices.Contains(s.Files, ".gitmodules") {
		t.Errorf("submodule path missing: %q", s.Files)
	}
	if slices.Contains(s.Files, "vendor/sub/inner.py") {
		t.Error("recursed into submodule")
	}
	all := resolve(t, Options{Root: f.work, All: true})
	if !slices.Contains(all.Files, "vendor/sub") || slices.Contains(all.Files, "vendor/sub/inner.py") {
		t.Errorf("All with submodule: %q", all.Files)
	}
	if fp, err := Fingerprint(f.work, []string{"vendor/sub"}); err != nil || fp["vendor/sub"] == "" {
		t.Errorf("Fingerprint(submodule) = %v, %v", fp, err)
	}
}

func TestMatchAndTouches(t *testing.T) {
	s := &Scope{
		Files:   []string{"a.py", "dir with space/new file.py", "docs/readme.md", "src/app/main.go", "src/app/main_test.go"},
		Deleted: []string{"lib/keep.py", "Cargo.lock"},
	}
	for _, tc := range []struct {
		globs []string
		want  []string
	}{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"*.py"}, []string{"a.py", "dir with space/new file.py"}},
		{[]string{"**/*.py"}, []string{"a.py", "dir with space/new file.py"}},
		{[]string{"/a.py"}, []string{"a.py"}},
		{[]string{"/*.py"}, []string{"a.py"}},
		{[]string{"src/**/*_test.go"}, []string{"src/app/main_test.go"}},
		{[]string{"src/*.go"}, nil},
		{[]string{"*.md", "*.go"}, []string{"docs/readme.md", "src/app/main.go", "src/app/main_test.go"}},
		{[]string{"dir with space/**"}, []string{"dir with space/new file.py"}},
		{[]string{"*.{md,py}"}, []string{"a.py", "dir with space/new file.py", "docs/readme.md"}},
		{[]string{"main.go"}, []string{"src/app/main.go"}},
		{[]string{"[bad"}, nil},
		{[]string{"*.rs"}, nil},
	} {
		eq(t, fmt.Sprintf("Match(%q)", tc.globs), s.Match(tc.globs), tc.want)
	}
	for _, tc := range []struct {
		globs []string
		want  bool
	}{
		{nil, false},
		{[]string{"*.py"}, true},
		{[]string{"lib/**"}, true}, // only via Deleted
		{[]string{"Cargo.lock"}, true},
		{[]string{"*.rs"}, false},
	} {
		if got := s.Touches(tc.globs); got != tc.want {
			t.Errorf("Touches(%q) = %v, want %v", tc.globs, got, tc.want)
		}
	}
	if err := ValidateGlobs([]string{"*.py", "src/**"}); err != nil {
		t.Error(err)
	}
	if err := ValidateGlobs([]string{"[bad"}); err == nil {
		t.Error("ValidateGlobs accepted [bad")
	}
	if !(&Scope{}).Empty() || (&Scope{Deleted: []string{"x"}}).Empty() {
		t.Error("Empty() wrong")
	}
}

func TestWorktreeGitDirs(t *testing.T) {
	f := newFixture(t)
	wt := filepath.Join(filepath.Dir(f.work), "wt")
	gitT(t, f.work, "worktree", "add", "-q", "-b", "wt-branch", wt, "feature")
	write(t, wt, "pkg/x.py", "x\n")

	mainGit, mainCommon, err := GitDirs(f.work)
	if err != nil {
		t.Fatal(err)
	}
	wtGit, wtCommon, err := GitDirs(filepath.Join(wt, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.work, ".git")
	if mainGit != want || mainCommon != want {
		t.Errorf("main worktree: GitDirs = %q, %q; want both %q", mainGit, mainCommon, want)
	}
	if wtGit == mainGit || wtGit != filepath.Join(want, "worktrees", "wt") {
		t.Errorf("linked worktree gitDir = %q", wtGit)
	}
	if wtCommon != mainCommon {
		t.Errorf("linked worktree commonDir = %q, want %q", wtCommon, mainCommon)
	}
	if top, err := Toplevel(filepath.Join(wt, "pkg")); err != nil || top != wt {
		t.Errorf("Toplevel = %q, %v; want %q", top, err, wt)
	}
	s := resolve(t, Options{Root: filepath.Join(wt, "pkg")})
	if s.Root != wt || s.BaseRef != "origin/main" || s.Base != f.branchPoint {
		t.Errorf("worktree scope: root %q base %q from %q", s.Root, s.Base, s.BaseRef)
	}
	eq(t, "worktree Files", s.Files, []string{"a.py", "dir with space/new file.py", "pkg/x.py"})
}

func TestFingerprint(t *testing.T) {
	dir := realTemp(t)
	write(t, dir, "a b.txt", "hello\n")
	write(t, dir, "d/e.txt", "")
	if err := os.Symlink("a b.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	fp, err := Fingerprint(dir, []string{"a b.txt", "d/e.txt", "missing.txt", "a b.txt/below", "link", "d"})
	if err != nil {
		t.Fatal(err)
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	want := map[string]string{
		"a b.txt":       sum("hello\n"),
		"d/e.txt":       sum(""),
		"missing.txt":   "",
		"a b.txt/below": "",
		"link":          sum("symlink\x00a b.txt"),
		"d":             sum("dir\x00"),
	}
	for k, v := range want {
		if fp[k] != v {
			t.Errorf("Fingerprint[%q] = %q, want %q", k, fp[k], v)
		}
	}
	if len(fp) != len(want) {
		t.Errorf("Fingerprint has %d entries", len(fp))
	}

	// Parallel path gives the same answers.
	var many []string
	for i := range 200 {
		name := "m/" + strconv.Itoa(i)
		write(t, dir, name, name)
		many = append(many, name)
	}
	fp, err = Fingerprint(dir, many)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range many {
		if fp[n] != sum(n) {
			t.Fatalf("Fingerprint[%q] wrong", n)
		}
	}
}

// TestLargeRepoAllFast checks full scope stays fast on a big repository.
// Default size keeps the suite quick; LAP_SCOPE_LARGE=100000 runs the full
// acceptance size.
func TestLargeRepoAllFast(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	n := 20000
	if v := os.Getenv("LAP_SCOPE_LARGE"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	dir := realTemp(t)
	gitT(t, dir, "init", "-q")
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("d%03d", i%500), fmt.Sprintf("f%06d.txt", i))
		if i < 500 {
			os.MkdirAll(filepath.Dir(p), 0o755)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "big")
	write(t, dir, "d000/changed.txt", "x")

	start := time.Now()
	s := resolve(t, Options{Root: dir, All: true})
	all := time.Since(start)
	start = time.Now()
	c := resolve(t, Options{Root: dir, Base: "HEAD"})
	changed := time.Since(start)
	t.Logf("%d files: All %v, changed %v", len(s.Files), all, changed)
	if len(s.Files) != n+1 || !slices.Equal(c.Files, []string{"d000/changed.txt"}) {
		t.Fatalf("All found %d files, changed %q", len(s.Files), c.Files)
	}
	if all > time.Second || changed > time.Second {
		t.Errorf("too slow: All %v, changed %v", all, changed)
	}
}
