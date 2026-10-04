// Package pool implements a machine-wide, per-user, cooperative resource pool
// for lap runs. It is daemonless: every CPU slot and every memory quantum is a
// token file in a private runtime directory, and holding a token means holding
// an exclusive flock(2) on that file.
//
// The pool is shared by every lap invocation of one user on one machine,
// across repositories and worktrees, so concurrent runs do not each assume
// they own the whole laptop. It is an admission control, not OS enforcement:
// processes outside lap are invisible to it.
//
// # Layout
//
// The directory (see [Config.Dir]) contains:
//
//	pool.json   canonical size: version, cpu, memory, quantum, created
//	arb.lock    short arbitration lock serialising acquisition attempts
//	cpu-<i>     one token per CPU slot, i in [0, cpu)
//	mem-<i>     one token per memory quantum, i in [0, memory/quantum)
//
// The size is canonical per machine and user: the first [Open] writes
// pool.json atomically and every later Open uses it, whatever its own Config
// asks for. A run that wants less should request less; it cannot resize the
// pool or create a second, incompatible one. [Reset] removes pool.json when no
// token is held.
//
// # Release and the crash gap
//
// A lease is released by closing its token descriptors, never by LOCK_UN:
// LOCK_UN acts on the open file description and would also release the lock
// for any duplicate that leaked into a child (see proc/SPIKE.md, Q6). Token
// descriptors are close-on-exec. Callers should close a lease only after the
// supervised work is verified stopped.
//
// If the holding process dies without cleanup (SIGKILL, crash), the kernel
// closes its descriptors and the tokens become free immediately, even though
// descendants of that process may still be running. A free token is therefore
// not evidence that no orphaned work exists. This is a known, documented gap
// of v0.
//
// # Defaults (experimental)
//
// When pool.json does not exist yet, the size comes from, in order: the
// LAP_POOL_CPU / LAP_POOL_MEMORY environment variables, the Config fields, and
// machine-derived defaults. The defaults are experimental starting points to
// be measured, not a contract:
//
//   - CPU: max(1, n - max(1, n/4)) where n is the number of CPUs usable by this
//     process (scheduler affinity on Linux, further limited by a cgroup v2
//     cpu.max quota when present).
//   - Memory: 50% of physical memory (Linux MemTotal, darwin hw.memsize), where
//     physical memory is first limited by a cgroup v2 memory.max when present.
//   - Quantum: 512 MiB.
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrBusy reports that the requested resources (or a lock) are currently held
// by someone else. Retry later; do not hold partial resources while waiting.
var ErrBusy = errors.New("pool: resources busy")

// ErrTooLarge reports a request that can never be satisfied by this pool.
var ErrTooLarge = errors.New("pool: request exceeds pool size")

// DefaultQuantum is the default memory token size (512 MiB).
const DefaultQuantum int64 = 512 << 20

// formatVersion is the pool.json / directory layout version. A new layout
// must use a new directory (pool-v2) so old and new binaries never share it.
const formatVersion = 1

const (
	configFile = "pool.json"
	arbFile    = "arb.lock"
	// arbTimeout bounds how long TryAcquire waits for the arbitration lock.
	// The lock is held only for a handful of nonblocking flock calls.
	arbTimeout = 2 * time.Second
)

// Config describes where the pool lives and, if it does not exist yet, how
// large to make it. Size fields are ignored when pool.json already exists.
type Config struct {
	Dir         string // default: $XDG_RUNTIME_DIR/lap/pool-v1, else os.TempDir()/lap-<uid>/pool-v1 (dir mode 0700)
	CPU         int    // pool CPU slots; 0 => default
	MemoryBytes int64  // pool memory; 0 => default
	Quantum     int64  // memory token size; 0 => 512 MiB
}

// Pool is an opened resource pool. It holds no descriptors itself and is safe
// for concurrent use.
type Pool struct {
	dir       string
	cpu       int
	memTokens int
	quantum   int64
	created   time.Time
}

// Holder identifies who holds tokens, for diagnostics.
type Holder struct {
	PID      int    `json:"pid"` // 0 => the current process
	Worktree string `json:"worktree,omitempty"`
	Task     string `json:"task,omitempty"`
	RunID    string `json:"run_id,omitempty"`
}

// Request asks for CPU slots and memory. Memory is rounded up to whole quanta.
type Request struct {
	CPU    int
	Memory int64
	Holder Holder
}

// HolderInfo describes one currently held token.
type HolderInfo struct {
	Token  string // token file name, e.g. "cpu-3"
	Holder Holder
	Since  time.Time
	// PIDStart is the holder process start time (Linux: clock ticks since
	// boot; darwin: microseconds since the epoch), recorded at acquisition so
	// a reused PID can be told apart. 0 when unknown.
	PIDStart uint64
}

// tokenMeta is the JSON written into a held token file.
type tokenMeta struct {
	Holder   Holder    `json:"holder"`
	Since    time.Time `json:"since"`
	PIDStart uint64    `json:"pid_start,omitempty"`
}

type poolFile struct {
	Version int       `json:"version"`
	CPU     int       `json:"cpu"`
	Memory  int64     `json:"memory"`
	Quantum int64     `json:"quantum"`
	Created time.Time `json:"created"`
}

// DefaultDir returns the default pool directory for the current user:
// $XDG_RUNTIME_DIR/lap/pool-v1 when XDG_RUNTIME_DIR is set, otherwise
// os.TempDir()/lap-<uid>/pool-v1.
func DefaultDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "lap", "pool-v1")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("lap-%d", os.Getuid()), "pool-v1")
}

// Open opens the pool in cfg.Dir, creating the directory and the canonical
// pool.json if needed. If pool.json already exists, its size is used and the
// size fields of cfg are ignored.
func Open(cfg Config) (*Pool, error) {
	if cfg.CPU < 0 || cfg.MemoryBytes < 0 || cfg.Quantum < 0 {
		return nil, fmt.Errorf("pool: negative size in config %+v", cfg)
	}
	dir := cfg.Dir
	if dir == "" {
		dir = DefaultDir()
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	pf, err := readPoolFile(dir)
	if errors.Is(err, fs.ErrNotExist) {
		var want poolFile
		want, err = initialSize(cfg)
		if err != nil {
			return nil, err
		}
		pf, err = createPoolFile(dir, want)
	}
	if err != nil {
		return nil, err
	}
	return &Pool{
		dir:       dir,
		cpu:       pf.CPU,
		memTokens: int(pf.Memory / pf.Quantum),
		quantum:   pf.Quantum,
		created:   pf.Created,
	}, nil
}

// Dir returns the absolute pool directory.
func (p *Pool) Dir() string { return p.dir }

// Size returns the canonical pool size. Memory is a whole number of quanta.
func (p *Pool) Size() (cpu int, memory int64) {
	return p.cpu, int64(p.memTokens) * p.quantum
}

// Quantum returns the memory token size in bytes.
func (p *Pool) Quantum() int64 { return p.quantum }

// quanta converts a memory request to a token count, rounding up.
func (p *Pool) quanta(mem int64) int {
	if mem <= 0 {
		return 0
	}
	return int((mem + p.quantum - 1) / p.quantum)
}

// Lease is a set of held tokens. Close releases them.
type Lease struct {
	mu     sync.Mutex
	files  []*os.File
	names  []string
	cpu    int
	memory int64
	closed bool
}

// CPU returns the number of CPU slots held.
func (l *Lease) CPU() int { return l.cpu }

// Memory returns the memory held in bytes (the request rounded up to quanta).
func (l *Lease) Memory() int64 { return l.memory }

// Tokens returns the names of the token files held, e.g. ["cpu-0", "mem-2"].
func (l *Lease) Tokens() []string { return append([]string(nil), l.names...) }

// Close releases every token by closing its descriptor (never LOCK_UN). It is
// idempotent; only the first call can return an error.
func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	var first error
	for _, f := range l.files {
		if err := f.Close(); err != nil && first == nil {
			first = fmt.Errorf("pool: release %s: %w", f.Name(), err)
		}
	}
	l.files = nil
	return first
}

// TryAcquire atomically acquires the whole request or nothing. It never
// blocks on resources: if they are not all free it returns ErrBusy (the
// arbitration lock may be waited on briefly). A request that can never fit
// returns ErrTooLarge. A request for no CPU and no memory succeeds with an
// empty lease. Lowest-index free tokens are preferred.
func (p *Pool) TryAcquire(req Request) (*Lease, error) {
	if req.CPU < 0 || req.Memory < 0 {
		return nil, fmt.Errorf("pool: negative request (cpu=%d memory=%d)", req.CPU, req.Memory)
	}
	nmem := p.quanta(req.Memory)
	if req.CPU > p.cpu || nmem > p.memTokens {
		return nil, fmt.Errorf("%w: want cpu=%d memory=%d (%d quanta), pool has cpu=%d memory=%d",
			ErrTooLarge, req.CPU, req.Memory, nmem, p.cpu, int64(p.memTokens)*p.quantum)
	}
	l := &Lease{cpu: req.CPU, memory: int64(nmem) * p.quantum}
	if req.CPU == 0 && nmem == 0 {
		return l, nil
	}

	arb, err := p.lockArb()
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		closeFile(arb) // releases the arbitration lock
		if !ok {
			l.Close()
		}
	}()

	if err := p.grab(l, "cpu", p.cpu, req.CPU); err != nil {
		return nil, err
	}
	if err := p.grab(l, "mem", p.memTokens, nmem); err != nil {
		return nil, err
	}

	h := req.Holder
	if h.PID == 0 {
		h.PID = os.Getpid()
	}
	meta, err := json.Marshal(tokenMeta{Holder: h, Since: time.Now().UTC(), PIDStart: procStart(h.PID)})
	if err != nil {
		return nil, fmt.Errorf("pool: holder metadata: %w", err)
	}
	for _, f := range l.files {
		// Metadata is diagnostic only; a write failure does not fail the lease.
		_ = writeMeta(f, meta)
	}
	ok = true
	return l, nil
}

// grab adds want free tokens of the given kind (indices [0,n)) to l, lowest
// index first. The caller holds the arbitration lock.
func (p *Pool) grab(l *Lease, kind string, n, want int) error {
	got := 0
	for i := 0; i < n && got < want; i++ {
		name := fmt.Sprintf("%s-%d", kind, i)
		f, err := os.OpenFile(filepath.Join(p.dir, name), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("pool: open token: %w", err)
		}
		locked, err := tryFlock(f)
		if err != nil {
			closeFile(f)
			return fmt.Errorf("pool: lock %s: %w", name, err)
		}
		if !locked {
			closeFile(f)
			continue
		}
		l.files = append(l.files, f)
		l.names = append(l.names, name)
		got++
	}
	if got < want {
		return fmt.Errorf("%w: %s %d of %d free", ErrBusy, kind, got, want)
	}
	return nil
}

// lockArb takes the arbitration lock, polling for up to arbTimeout.
func (p *Pool) lockArb() (*os.File, error) {
	ctx, cancel := context.WithTimeout(context.Background(), arbTimeout)
	defer cancel()
	f, err := lockPoll(ctx, filepath.Join(p.dir, arbFile), time.Millisecond)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: arbitration lock timed out", ErrBusy)
		}
		return nil, err
	}
	return f, nil
}

// probe reports, for each token of a kind, whether it is currently held.
// The caller holds the arbitration lock, so the brief probe lock cannot make
// a concurrent TryAcquire fail spuriously.
func (p *Pool) probe(kind string, n int, fn func(name string, f *os.File)) error {
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%d", kind, i)
		f, err := os.Open(filepath.Join(p.dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue // never created: free
		}
		if err != nil {
			return fmt.Errorf("pool: open token: %w", err)
		}
		locked, err := tryFlock(f)
		if err != nil {
			closeFile(f)
			return fmt.Errorf("pool: probe %s: %w", name, err)
		}
		if !locked {
			fn(name, f)
		}
		closeFile(f) // also drops our probe lock if we got it
	}
	return nil
}

// Holders returns the currently held tokens with their recorded metadata,
// CPU tokens first, each in index order. Metadata is best effort: a token that
// was just acquired may still show its previous holder or none.
func (p *Pool) Holders() ([]HolderInfo, error) {
	arb, err := p.lockArb()
	if err != nil {
		return nil, err
	}
	defer closeFile(arb)
	var out []HolderInfo
	add := func(name string, f *os.File) {
		hi := HolderInfo{Token: name}
		var m tokenMeta
		if b, err := readAllAt(f); err == nil && json.Unmarshal(b, &m) == nil {
			hi.Holder, hi.Since, hi.PIDStart = m.Holder, m.Since, m.PIDStart
		}
		out = append(out, hi)
	}
	if err := p.probe("cpu", p.cpu, add); err != nil {
		return nil, err
	}
	if err := p.probe("mem", p.memTokens, add); err != nil {
		return nil, err
	}
	return out, nil
}

// Free returns a best-effort snapshot of free CPU slots and free memory. It
// may be stale by the time it returns; use TryAcquire to actually reserve.
// On error it reports nothing free.
func (p *Pool) Free() (cpu int, memory int64) {
	arb, err := p.lockArb()
	if err != nil {
		return 0, 0
	}
	defer closeFile(arb)
	heldCPU, heldMem := 0, 0
	if p.probe("cpu", p.cpu, func(string, *os.File) { heldCPU++ }) != nil {
		return 0, 0
	}
	if p.probe("mem", p.memTokens, func(string, *os.File) { heldMem++ }) != nil {
		return 0, 0
	}
	return p.cpu - heldCPU, int64(p.memTokens-heldMem) * p.quantum
}

// Reset removes pool.json from dir so the next Open recreates the pool with a
// fresh size. It fails with ErrBusy if any token in dir is held. Token files
// are left in place (active token files must never be unlinked). Pools opened
// before the reset keep their old size; Reset is for tests and admin use.
func Reset(dir string) error {
	if dir == "" {
		dir = DefaultDir()
	}
	p := &Pool{dir: dir}
	arb, err := p.lockArb()
	if err != nil {
		return err
	}
	defer closeFile(arb)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "cpu-") && !strings.HasPrefix(n, "mem-") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		locked, err := tryFlock(f)
		closeFile(f)
		if err != nil {
			return fmt.Errorf("pool: probe %s: %w", n, err)
		}
		if !locked {
			return fmt.Errorf("%w: token %s is held", ErrBusy, n)
		}
	}
	if err := os.Remove(filepath.Join(dir, configFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pool: %w", err)
	}
	return nil
}

// initialSize computes the size for a new pool: env > Config > defaults.
func initialSize(cfg Config) (poolFile, error) {
	pf := poolFile{Version: formatVersion, CPU: cfg.CPU, Memory: cfg.MemoryBytes, Quantum: cfg.Quantum}
	if s := os.Getenv("LAP_POOL_CPU"); s != "" {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			return pf, fmt.Errorf("pool: invalid LAP_POOL_CPU %q", s)
		}
		pf.CPU = n
	}
	if s := os.Getenv("LAP_POOL_MEMORY"); s != "" {
		n, err := ParseBytes(s)
		if err != nil || n < 1 {
			return pf, fmt.Errorf("pool: invalid LAP_POOL_MEMORY %q", s)
		}
		pf.Memory = n
	}
	if pf.Quantum == 0 {
		pf.Quantum = DefaultQuantum
	}
	if pf.CPU == 0 {
		pf.CPU = DefaultCPU()
	}
	if pf.Memory == 0 {
		pf.Memory = DefaultMemory()
	}
	// Whole quanta, at least one.
	pf.Memory = max(1, pf.Memory/pf.Quantum) * pf.Quantum
	pf.Created = time.Now().UTC()
	return pf, nil
}

// DefaultCPU returns the experimental default CPU slot count:
// max(1, n - max(1, n/4)) for n usable CPUs.
func DefaultCPU() int {
	n := usableCPUs()
	return max(1, n-max(1, n/4))
}

// DefaultMemory returns the experimental default pool memory: 50% of physical
// memory, where physical memory is limited by a cgroup v2 memory.max if any.
// It falls back to 4 GiB when memory cannot be determined.
func DefaultMemory() int64 {
	total := totalMemory()
	if total <= 0 {
		return 4 << 30
	}
	return total / 2
}

// ParseBytes parses a byte count: a plain integer, or one with a binary
// suffix K, M, G or T (optionally followed by "B" or "iB"), case-insensitive.
func ParseBytes(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, "IB")
	t = strings.TrimSuffix(t, "B")
	mult := int64(1)
	if t != "" {
		switch t[len(t)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			t = strings.TrimSpace(t[:len(t)-1])
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("pool: invalid byte count %q", s)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("pool: byte count %q overflows", s)
	}
	return n * mult, nil
}

func readPoolFile(dir string) (poolFile, error) {
	var pf poolFile
	path := filepath.Join(dir, configFile)
	// Normally pool.json appears atomically (hard link of a complete file).
	// The O_EXCL fallback can expose a partial file briefly; retry parsing.
	deadline := time.Now().Add(time.Second)
	for {
		b, err := os.ReadFile(path)
		if err != nil {
			return pf, err
		}
		err = json.Unmarshal(b, &pf)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return pf, fmt.Errorf("pool: corrupt %s: %w", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pf.Version != formatVersion {
		return pf, fmt.Errorf("pool: %s has version %d, want %d", path, pf.Version, formatVersion)
	}
	if pf.CPU < 1 || pf.Quantum < 1 || pf.Memory < pf.Quantum {
		return pf, fmt.Errorf("pool: invalid size in %s: %+v", path, pf)
	}
	return pf, nil
}

// createPoolFile publishes want as pool.json unless one already exists, in
// which case the existing (canonical) one is returned. Concurrent creators
// agree because publication is a hard link, which fails if the name exists.
func createPoolFile(dir string, want poolFile) (poolFile, error) {
	b, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		return want, err
	}
	b = append(b, '\n')
	path := filepath.Join(dir, configFile)
	tmp, err := os.CreateTemp(dir, ".pool-*.json")
	if err != nil {
		return want, fmt.Errorf("pool: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return want, fmt.Errorf("pool: write %s: %w", tmpName, werr)
	}
	err = os.Link(tmpName, path)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		// Filesystem without hard links: fall back to O_EXCL (readers retry
		// on a partially written file).
		var f *os.File
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.Write(b)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				return want, fmt.Errorf("pool: write %s: %w", path, werr)
			}
		}
	}
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return want, fmt.Errorf("pool: create %s: %w", path, err)
	}
	return readPoolFile(dir)
}

func writeMeta(f *os.File, b []byte) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt(b, 0)
	return err
}

func readAllAt(f *os.File) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b := make([]byte, st.Size())
	n, err := f.ReadAt(b, 0)
	if n == len(b) {
		err = nil
	}
	return b[:n], err
}

func closeFile(f *os.File) { _ = f.Close() }
