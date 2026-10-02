//go:build linux

package proc

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// reader drains one capture endpoint, standing in for package term.
type reader struct {
	mu    sync.Mutex
	lines []string
	eof   chan struct{}
	cond  chan struct{} // pinged on each new line
}

func newReader(f *os.File) *reader {
	r := &reader{eof: make(chan struct{}), cond: make(chan struct{}, 1)}
	go func() {
		defer close(r.eof)
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			r.mu.Lock()
			r.lines = append(r.lines, strings.TrimRight(sc.Text(), "\r"))
			r.mu.Unlock()
			select {
			case r.cond <- struct{}{}:
			default:
			}
		}
		// pipe: EOF; pty master: EIO once every slave fd is closed.
	}()
	return r
}

// pid waits for a "NAME <pid>" line.
func (r *reader) pid(t *testing.T, name string) int {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		r.mu.Lock()
		for _, l := range r.lines {
			f := strings.Fields(l)
			if len(f) >= 2 && f[0] == name {
				r.mu.Unlock()
				n, _ := strconv.Atoi(f[1])
				return n
			}
			if len(f) > 0 && strings.HasSuffix(f[0], "-failed") {
				r.mu.Unlock()
				t.Fatalf("helper: %s", l)
			}
		}
		r.mu.Unlock()
		select {
		case <-r.cond:
		case <-r.eof:
			select {
			case <-r.cond:
			default:
				r.mu.Lock()
				ls := r.lines
				r.mu.Unlock()
				t.Fatalf("EOF before %q; lines=%q", name, ls)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q", name)
		}
	}
}

type tree struct {
	p    *Process
	r    *reader
	pids map[string]int
}

// startPipe launches plan in NewGroup mode with stdout/stderr on a fresh pipe.
func startPipe(t *testing.T, spec Spec, names ...string) *tree {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	spec.Stdout, spec.Stderr = wr, wr
	spec.Session = NewGroup
	p, err := Start(spec)
	wr.Close() // child ends belong to the child now
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rd.Close() })
	return finishStart(t, p, newReader(rd), names)
}

// startPTY launches plan in NewSessionWithCtty mode on a fresh PTY.
func startPTY(t *testing.T, spec Spec, names ...string) *tree {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	spec.Stdin, spec.Stdout, spec.Stderr = slave, slave, slave
	spec.Session = NewSessionWithCtty
	p, err := Start(spec)
	slave.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	return finishStart(t, p, newReader(master), names)
}

func finishStart(t *testing.T, p *Process, r *reader, names []string) *tree {
	tr := &tree{p: p, r: r, pids: map[string]int{}}
	starts := map[int]uint64{}
	t.Cleanup(func() {
		// Never leak: KILL anything we saw that is still the same process.
		for _, pid := range tr.pids {
			if m, err := readStat(pid); err == nil && m.Start == starts[pid] {
				unix.Kill(pid, syscall.SIGKILL)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		p.Terminate(ctx, 0)
	})
	for _, n := range names {
		pid := r.pid(t, n)
		tr.pids[n] = pid
		if m, err := readStat(pid); err == nil {
			starts[pid] = m.Start
		}
	}
	return tr
}

func alive(pid int) bool {
	m, err := readStat(pid)
	return err == nil && m.live()
}

// waitFor polls cond up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func terminate(t *testing.T, p *Process, grace time.Duration) CleanupReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := p.Terminate(ctx, grace)
	t.Logf("cleanup: verified=%v term=%v kill=%v elapsed=%v exit=%v reaped=%v members=%v escaped=%v survivors=%v err=%v",
		r.Verified, r.SentTerm, r.SentKill, r.Elapsed(), r.Exit, r.Reaped, r.Members, r.Escaped, r.Survivors, r.Err)
	return r
}

func hasMember(ms []Member, pid int) (Member, bool) {
	for _, m := range ms {
		if m.Pid == pid {
			return m, true
		}
	}
	return Member{}, false
}
