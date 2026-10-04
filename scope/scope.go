// Package scope selects the files a lap run should consider, from git.
//
// In changed mode (the default) the scope is everything that differs from the
// merge-base of HEAD and the repository's default branch: committed branch
// changes, staged and unstaged changes, and untracked files that are not
// ignored. In full mode ([Options.All]) it is every tracked file plus every
// untracked, non-ignored file.
//
// The base is resolved locally and never fetched: origin/HEAD if set, else the
// first of origin/main, origin/master, main, master that exists. The tracking
// upstream (@{upstream}) is deliberately not used because it usually points at
// the feature branch itself. If nothing resolves, [Resolve] returns
// [ErrNoBase]; the caller must pass an explicit base or use full scope.
//
// Selection is not execution scope: deleted and renamed-away paths are kept in
// [Scope.Deleted] so they can still trigger checks (see [Scope.Touches]) even
// though they cannot be passed to file-based tools. Submodules and nested
// repositories are reported as single paths and never recursed into.
//
// All paths are repo-relative and slash-separated. Git is invoked a constant
// number of times per Resolve (no per-file calls), with -z output so any byte
// in a path name is handled, and with GIT_OPTIONAL_LOCKS=0 so it does not
// contend with the user's concurrent git commands for the index lock.
package scope

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/bmatcuk/doublestar/v4"
)

// ErrNoBase reports that no base revision could be resolved.
var ErrNoBase = errors.New("scope: cannot resolve a base revision; pass an explicit base or use full scope")

// errNotDir: a path component is a regular file, so the path does not exist.
var errNotDir = syscall.ENOTDIR

// Options controls Resolve.
type Options struct {
	Root string // any dir inside the repo; resolved to toplevel
	Base string // explicit base revision; "" => auto
	All  bool   // full scope: Files = all tracked + untracked-not-ignored files
}

// Scope is a resolved file selection.
type Scope struct {
	Root    string   // absolute repo toplevel
	All     bool     // full scope
	Base    string   // resolved merge-base commit ("" when All)
	BaseRef string   // what it was resolved from, e.g. "origin/main"
	Files   []string // repo-relative, slash-separated, sorted, existing files: committed-since-base + staged + unstaged + untracked-not-ignored
	Deleted []string // repo-relative paths deleted since base (incl. rename sources); in full scope, tracked paths missing from the worktree
}

// autoBaseCandidates are tried, in order, when origin/HEAD is not set.
var autoBaseCandidates = []struct{ ref, name string }{
	{"refs/remotes/origin/main", "origin/main"},
	{"refs/remotes/origin/master", "origin/master"},
	{"refs/heads/main", "main"},
	{"refs/heads/master", "master"},
}

// Resolve computes the scope for opt.
func Resolve(ctx context.Context, opt Options) (*Scope, error) {
	dir := opt.Root
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("scope: %w", err)
		}
	}
	root, err := toplevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	s := &Scope{Root: root, All: opt.All}

	if opt.All {
		out, err := git(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if err != nil {
			return nil, err
		}
		paths := sortedUnique(splitZ(out))
		s.Files, s.Deleted = partitionExisting(root, paths)
		return s, nil
	}

	s.Base, s.BaseRef, err = resolveBase(ctx, root, opt.Base)
	if err != nil {
		return nil, err
	}

	// The two listings are independent; run them concurrently.
	var (
		diffOut, untrackedOut []byte
		diffErr, untrackedErr error
		wg                    sync.WaitGroup
	)
	wg.Go(func() {
		// Working tree vs base: covers committed, staged and unstaged changes
		// to tracked (indexed) files.
		diffOut, diffErr = git(ctx, root, "diff", "--name-status", "-z", "--find-renames",
			"--no-ext-diff", "--no-textconv", "--no-color", "--no-relative", s.Base, "--")
	})
	wg.Go(func() {
		untrackedOut, untrackedErr = git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	})
	wg.Wait()
	if diffErr != nil {
		return nil, diffErr
	}
	if untrackedErr != nil {
		return nil, untrackedErr
	}

	changed, deleted, err := parseNameStatus(diffOut)
	if err != nil {
		return nil, err
	}
	for _, p := range splitZ(untrackedOut) {
		// Nested repositories are listed as "dir/"; report them as a path.
		changed = append(changed, strings.TrimSuffix(p, "/"))
	}
	files, missing := partitionExisting(root, sortedUnique(changed))
	s.Files = files
	// A path is deleted only if it is not present now (e.g. a file removed
	// from the index but still on disk is reported as untracked, not deleted).
	present := make(map[string]struct{}, len(files))
	for _, f := range files {
		present[f] = struct{}{}
	}
	var del []string
	for _, p := range append(deleted, missing...) {
		if _, ok := present[p]; !ok {
			del = append(del, p)
		}
	}
	s.Deleted = sortedUnique(del)
	return s, nil
}

// resolveBase returns the merge-base of HEAD with the explicit base or the
// auto-detected default branch, and the name it was resolved from.
func resolveBase(ctx context.Context, root, explicit string) (base, ref string, err error) {
	if explicit != "" {
		if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "--end-of-options", explicit+"^{commit}"); err != nil {
			return "", "", fmt.Errorf("%w (%q is not a commit)", ErrNoBase, explicit)
		}
		ref = explicit
	} else {
		ref, err = autoBase(ctx, root)
		if err != nil {
			return "", "", err
		}
	}
	out, err := git(ctx, root, "merge-base", "--end-of-options", ref, "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", fmt.Errorf("%w (no merge-base of %s and HEAD)", ErrNoBase, ref)
	}
	return strings.TrimSpace(string(out)), ref, nil
}

func autoBase(ctx context.Context, root string) (string, error) {
	if out, err := git(ctx, root, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		full := strings.TrimSpace(string(out))
		if commitExists(ctx, root, full) {
			return strings.TrimPrefix(full, "refs/remotes/"), nil
		}
	}
	for _, c := range autoBaseCandidates {
		if commitExists(ctx, root, c.ref) {
			return c.name, nil
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", ErrNoBase
}

func commitExists(ctx context.Context, root, ref string) bool {
	_, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	return err == nil
}

// parseNameStatus parses `git diff --name-status -z` output.
func parseNameStatus(out []byte) (changed, deleted []string, err error) {
	f := splitZ(out)
	for i := 0; i < len(f); {
		status := f[i]
		i++
		if status == "" {
			return nil, nil, fmt.Errorf("scope: malformed diff output")
		}
		switch status[0] {
		case 'R', 'C': // score-suffixed; two paths: source, destination
			if i+1 >= len(f) {
				return nil, nil, fmt.Errorf("scope: truncated diff output after %q", status)
			}
			src, dst := f[i], f[i+1]
			i += 2
			if status[0] == 'R' {
				deleted = append(deleted, src)
			}
			changed = append(changed, dst)
		default: // A, M, T, U, X, D: one path
			if i >= len(f) {
				return nil, nil, fmt.Errorf("scope: truncated diff output after %q", status)
			}
			p := f[i]
			i++
			if status[0] == 'D' {
				deleted = append(deleted, p)
			} else {
				changed = append(changed, p)
			}
		}
	}
	return changed, deleted, nil
}

// Match returns the Files matching any of globs, in order. Globs use
// doublestar syntax ("**" spans directories). A glob without a "/" matches
// the base name at any depth: "*.py" is treated as "**/*.py". A glob with a
// "/" is matched against the whole repo-relative path; a leading "/" is
// stripped (it only anchors). Invalid globs match nothing (see ValidateGlobs).
// Empty globs return nil.
func (s *Scope) Match(globs []string) []string {
	pats := normalizeGlobs(globs)
	if len(pats) == 0 {
		return nil
	}
	var out []string
	for _, f := range s.Files {
		if matchAny(pats, f) {
			out = append(out, f)
		}
	}
	return out
}

// Touches reports whether any path in Files or Deleted matches any glob
// (same syntax as Match).
func (s *Scope) Touches(globs []string) bool {
	pats := normalizeGlobs(globs)
	if len(pats) == 0 {
		return false
	}
	for _, list := range [][]string{s.Files, s.Deleted} {
		for _, f := range list {
			if matchAny(pats, f) {
				return true
			}
		}
	}
	return false
}

// Empty reports whether nothing was selected: no files and no deletions.
func (s *Scope) Empty() bool { return len(s.Files) == 0 && len(s.Deleted) == 0 }

// ValidateGlobs reports the first invalid glob, if any.
func ValidateGlobs(globs []string) error {
	for _, g := range globs {
		if !doublestar.ValidatePattern(normalizeGlob(g)) {
			return fmt.Errorf("scope: invalid glob %q", g)
		}
	}
	return nil
}

func normalizeGlob(g string) string {
	if strings.HasPrefix(g, "/") {
		return strings.TrimLeft(g, "/")
	}
	g = strings.TrimPrefix(g, "./")
	if !strings.Contains(g, "/") {
		return "**/" + g
	}
	return g
}

func normalizeGlobs(globs []string) []string {
	var out []string
	for _, g := range globs {
		if g == "" {
			continue
		}
		if p := normalizeGlob(g); doublestar.ValidatePattern(p) {
			out = append(out, p)
		}
	}
	return out
}

func matchAny(pats []string, path string) bool {
	for _, p := range pats {
		if doublestar.MatchUnvalidated(p, path) {
			return true
		}
	}
	return false
}

// GitDirs returns the absolute git directory of the worktree containing root
// and the absolute common directory shared by all its worktrees. For the main
// worktree they are equal; for a linked worktree (git worktree add) gitDir is
// .git/worktrees/<name> and commonDir is the main .git.
func GitDirs(root string) (gitDir, commonDir string, err error) {
	out, err := git(context.Background(), root, "rev-parse", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("scope: unexpected rev-parse output %q", out)
	}
	gitDir, commonDir = lines[0], lines[1]
	if !filepath.IsAbs(commonDir) {
		// Relative to the directory git ran in.
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", "", fmt.Errorf("scope: %w", err)
		}
		commonDir = filepath.Join(abs, commonDir)
	}
	return filepath.Clean(gitDir), filepath.Clean(commonDir), nil
}

// Toplevel returns the absolute top-level directory of the worktree
// containing dir.
func Toplevel(dir string) (string, error) {
	return toplevel(context.Background(), dir)
}

func toplevel(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimRight(string(out), "\n")
	if top == "" {
		return "", fmt.Errorf("scope: %s is not inside a git worktree", dir)
	}
	return filepath.Clean(top), nil
}

// Fingerprint hashes files (repo-relative, slash-separated) under root. The
// value is the sha256 hex of a regular file's contents; for a symlink, of
// "symlink\x00"+target (links are not followed); for a directory (e.g. a
// submodule), of "dir\x00". A missing path maps to "". Files are hashed in
// parallel.
func Fingerprint(root string, files []string) (map[string]string, error) {
	sums := make([]string, len(files))
	errs := make([]error, len(files))
	parallelFor(len(files), 64, func(i int) {
		sums[i], errs[i] = hashPath(filepath.Join(root, filepath.FromSlash(files[i])))
	})
	out := make(map[string]string, len(files))
	for i, f := range files {
		if errs[i] != nil {
			return nil, fmt.Errorf("scope: fingerprint %s: %w", f, errs[i])
		}
		out[f] = sums[i]
	}
	return out, nil
}

func hashPath(path string) (string, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotDir) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	h := sha256.New()
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		io.WriteString(h, "symlink\x00"+target)
	case st.IsDir():
		io.WriteString(h, "dir\x00")
	default:
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// partitionExisting splits repo-relative paths into those present on disk
// (any file type, symlinks not followed) and those missing, keeping order.
func partitionExisting(root string, paths []string) (exist, missing []string) {
	ok := make([]bool, len(paths))
	parallelFor(len(paths), 2048, func(i int) {
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(paths[i])))
		ok[i] = err == nil
	})
	exist = make([]string, 0, len(paths))
	for i, p := range paths {
		if ok[i] {
			exist = append(exist, p)
		} else {
			missing = append(missing, p)
		}
	}
	return exist, missing
}

// parallelFor runs fn(i) for i in [0,n), across goroutines once n exceeds
// serialBelow.
func parallelFor(n, serialBelow int, fn func(int)) {
	if n < serialBelow {
		for i := range n {
			fn(i)
		}
		return
	}
	workers := min(runtime.GOMAXPROCS(0), 16, n)
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += chunk {
		hi := min(lo+chunk, n)
		wg.Go(func() {
			for i := lo; i < hi; i++ {
				fn(i)
			}
		})
	}
	wg.Wait()
}

func sortedUnique(paths []string) []string {
	slices.Sort(paths)
	return slices.Compact(paths)
}

func splitZ(b []byte) []string {
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

// git runs git in dir and returns stdout.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("scope: git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("scope: git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
