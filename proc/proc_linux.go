//go:build linux

package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const holderScanSupported = true

// handle is the Linux view of the direct child: its PID plus a pidfd, which
// makes signalling and waiting immune to PID reuse.
type handle struct {
	pid   int
	pidfd int // -1 if the kernel lacks pidfd support
}

func (p *Process) startOS(path string, files [3]*os.File) error {
	fds := make([]uintptr, 3)
	for i, f := range files {
		fd, err := rawFD(f)
		if err != nil {
			return err
		}
		fds[i] = fd
	}
	pidfd := -1
	sys := &syscall.SysProcAttr{PidFD: &pidfd, Pdeathsig: p.spec.Pdeathsig}
	switch p.spec.Session {
	case NewGroup:
		sys.Setpgid = true
	case NewSessionWithCtty:
		// Not Setpgid: after setsid the child is a session leader and
		// setpgid(0,0) fails with EPERM (see TestProbeSetsidPlusSetpgid).
		sys.Setsid = true
		sys.Setctty = true
		sys.Ctty = p.spec.CttyFD
	}
	pid, err := syscall.ForkExec(path, p.spec.Argv, &syscall.ProcAttr{
		Dir: p.spec.Dir, Env: p.spec.Env, Files: fds, Sys: sys,
	})
	runtime.KeepAlive(files)
	if err != nil {
		return err
	}
	p.pid = pid
	p.h = &handle{pid: pid, pidfd: pidfd}
	// The child cannot be reaped before Terminate, so its PID (and start
	// time) cannot be recycled under us: this read is race-free.
	if st, err := readStat(pid); err == nil {
		p.start = st.Start
	}
	return nil
}

// rawFD returns the descriptor without (*os.File).Fd's side effect of
// switching it to blocking mode. The child end should normally be blocking
// already; the caller's read end is untouched either way.
func rawFD(f *os.File) (uintptr, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return f.Fd(), nil
	}
	var fd uintptr
	if err := sc.Control(func(x uintptr) { fd = x }); err != nil {
		return 0, err
	}
	return fd, nil
}

// sigchldInfo mirrors the SIGCHLD layout of siginfo_t on Linux (64-bit):
// si_signo, si_errno, si_code, pad, si_pid, si_uid, si_status.
type sigchldInfo struct {
	Signo, Errno, Code int32
	_                  int32
	Pid                int32
	Uid                uint32
	Status             int32
	_                  [100]byte
}

func init() {
	if unsafe.Sizeof(sigchldInfo{}) != unsafe.Sizeof(unix.Siginfo{}) {
		panic("proc: sigchldInfo size mismatch")
	}
}

// waitExitNoReap blocks until the child exits, leaving it a zombie.
func (h *handle) waitExitNoReap() (ExitInfo, error) {
	var info sigchldInfo
	idType, id := unix.P_PIDFD, h.pidfd
	if h.pidfd < 0 {
		idType, id = unix.P_PID, h.pid
	}
	for {
		err := unix.Waitid(idType, id, (*unix.Siginfo)(unsafe.Pointer(&info)), unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return ExitInfo{Code: -1}, err
		}
		break
	}
	switch info.Code {
	case 1: // CLD_EXITED
		return ExitInfo{Code: int(info.Status)}, nil
	default: // CLD_KILLED, CLD_DUMPED
		return ExitInfo{Code: -1, Signal: syscall.Signal(info.Status)}, nil
	}
}

func (h *handle) reap() (*Rusage, error) {
	var ws unix.WaitStatus
	var ru unix.Rusage
	for {
		_, err := unix.Wait4(h.pid, &ws, 0, &ru)
		if err == unix.EINTR {
			continue
		}
		if h.pidfd >= 0 {
			unix.Close(h.pidfd)
			h.pidfd = -1
		}
		if err != nil {
			return nil, err
		}
		break
	}
	return &Rusage{
		User:        time.Duration(ru.Utime.Nano()),
		System:      time.Duration(ru.Stime.Nano()),
		MaxRSSBytes: ru.Maxrss * 1024, // Linux reports KiB
	}, nil
}

// signalGroup signals the process group whose ID is the direct child's PID.
// Safe from ID reuse because the child is not reaped until Terminate ends.
func (h *handle) signalGroup(sig syscall.Signal) error {
	return unix.Kill(-h.pid, sig)
}

var errStale = errors.New("proc: process identity changed")

// signalMember signals one process only if it is still the one scanned:
// pidfd_open pins the process, then the start time is re-checked, then the
// signal is sent through the pidfd. No PID-reuse window.
func signalMember(m Member, sig syscall.Signal) error {
	fd, err := unix.PidfdOpen(m.Pid, 0)
	if err != nil {
		if err == unix.ENOSYS {
			return signalMemberRacy(m, sig)
		}
		return err
	}
	defer unix.Close(fd)
	st, err := readStat(m.Pid)
	if err != nil {
		return err
	}
	if st.Start != m.Start {
		return errStale
	}
	return unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0)
}

func signalMemberRacy(m Member, sig syscall.Signal) error {
	st, err := readStat(m.Pid)
	if err != nil {
		return err
	}
	if st.Start != m.Start {
		return errStale
	}
	return unix.Kill(m.Pid, sig)
}

// listProcs scans /proc. Processes that vanish mid-scan are skipped.
func listProcs() ([]Member, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(names))
	for _, n := range names {
		if n[0] < '0' || n[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		m, err := readStat(pid)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// readStat reads /proc/<pid>/stat with open/read/close only (os.ReadFile
// adds fstat and a second read; this halves the cost of a full scan).
func readStat(pid int) (Member, error) {
	var buf [1024]byte
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid)+"/stat", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Member{}, err
	}
	n, err := unix.Read(fd, buf[:])
	unix.Close(fd)
	if err != nil {
		return Member{}, err
	}
	return parseStat(buf[:n])
}

// parseStat parses /proc/<pid>/stat. comm may contain spaces and ')' so the
// fields are located after the LAST ')'.
func parseStat(b []byte) (Member, error) {
	open := bytes.IndexByte(b, '(')
	closeIdx := bytes.LastIndexByte(b, ')')
	if open < 0 || closeIdx < open {
		return Member{}, fmt.Errorf("proc: malformed stat %q", b)
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b[:open])))
	if err != nil {
		return Member{}, err
	}
	f := bytes.Fields(b[closeIdx+1:])
	// f[0] is field 3 (state); field N is f[N-3].
	if len(f) < 20 {
		return Member{}, fmt.Errorf("proc: short stat %q", b)
	}
	atoi := func(x []byte) int { v, _ := strconv.Atoi(string(x)); return v }
	start, err := strconv.ParseUint(string(f[22-3]), 10, 64)
	if err != nil {
		return Member{}, err
	}
	return Member{
		Pid:   pid,
		Comm:  string(b[open+1 : closeIdx]),
		State: f[0][0],
		PPid:  atoi(f[4-3]),
		Pgid:  atoi(f[5-3]),
		Sid:   atoi(f[6-3]),
		Start: start,
	}, nil
}

// fileID identifies a stdio object as /proc/<pid>/fd links render it, e.g.
// "pipe:[123]" or "/dev/pts/4". /dev/null and regular files are excluded:
// sharing them says nothing about ownership.
type fileID string

func holderIDs(files []*os.File) []fileID {
	var ids []fileID
	seen := map[fileID]bool{}
	for _, f := range files {
		fd, err := rawFD(f)
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(fd)))
		runtime.KeepAlive(f)
		if err != nil {
			continue
		}
		id := fileID(link)
		if !isHolderCandidate(link) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func isHolderCandidate(link string) bool {
	return hasPrefix(link, "pipe:[") || hasPrefix(link, "socket:[") || hasPrefix(link, "/dev/pts/")
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// findHolders returns candidates with an fd pointing at any of ids.
// Unreadable fd directories (other users) are skipped.
func findHolders(cands []Member, ids []fileID) []Member {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[string(id)] = true
	}
	var out []Member
	for _, m := range cands {
		dir := "/proc/" + strconv.Itoa(m.Pid) + "/fd/"
		d, err := os.Open(dir)
		if err != nil {
			continue
		}
		names, _ := d.Readdirnames(-1)
		d.Close()
		for _, n := range names {
			if link, err := os.Readlink(dir + n); err == nil && want[link] {
				out = append(out, m)
				break
			}
		}
	}
	return out
}
