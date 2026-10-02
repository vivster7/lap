//go:build darwin

package proc

// UNTESTED: this file is compile-checked only (GOOS=darwin go vet). The
// structure mirrors proc_linux.go; the differences are:
//   - no pidfd: per-process signals re-check the start time and then kill(2),
//     leaving a small PID-reuse window;
//   - exit is observed without reaping through kqueue EVFILT_PROC/NOTE_EXIT;
//   - the process table comes from sysctl kern.proc.all, the session ID from
//     getsid(2) per process;
//   - no stdio-holder scan (would need libproc proc_pidinfo); reported as
//     Unverifiable.

import (
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const holderScanSupported = false

type handle struct {
	pid         int
	kq          int
	reapedEarly atomic.Bool
}

type fileID string

func holderIDs([]*os.File) []fileID           { return nil }
func findHolders([]Member, []fileID) []Member { return nil }
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

func (p *Process) startOS(path string, files [3]*os.File) error {
	fds := make([]uintptr, 3)
	for i, f := range files {
		fd, err := rawFD(f)
		if err != nil {
			return err
		}
		fds[i] = fd
	}
	sys := &syscall.SysProcAttr{}
	switch p.spec.Session {
	case NewGroup:
		sys.Setpgid = true
	case NewSessionWithCtty:
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
	p.h = &handle{pid: pid, kq: -1}
	if kq, err := unix.Kqueue(); err == nil {
		ev := unix.Kevent_t{}
		unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
		ev.Fflags = unix.NOTE_EXIT | unix.NOTE_EXITSTATUS
		if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err == nil {
			p.h.kq = kq
		} else {
			unix.Close(kq)
		}
	}
	if m, err := procByPid(pid); err == nil {
		p.start = m.Start
	}
	return nil
}

func (h *handle) waitExitNoReap() (ExitInfo, error) {
	if h.kq < 0 {
		// Registration failed (e.g. the child already exited): fall back to
		// a reaping wait. The pgid is then no longer pinned.
		var ws unix.WaitStatus
		_, err := unix.Wait4(h.pid, &ws, 0, nil)
		h.reapedEarly.Store(true)
		return statusInfo(ws), err
	}
	out := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(h.kq, nil, out, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return ExitInfo{Code: -1}, err
		}
		if n == 1 {
			break
		}
	}
	unix.Close(h.kq)
	return statusInfo(unix.WaitStatus(out[0].Data)), nil
}

func statusInfo(ws unix.WaitStatus) ExitInfo {
	if ws.Signaled() {
		return ExitInfo{Code: -1, Signal: syscall.Signal(ws.Signal())}
	}
	return ExitInfo{Code: ws.ExitStatus()}
}

func (h *handle) reap() (*Rusage, error) {
	if h.reapedEarly.Load() {
		return nil, errors.New("proc: already reaped without rusage")
	}
	var ws unix.WaitStatus
	var ru unix.Rusage
	for {
		_, err := unix.Wait4(h.pid, &ws, 0, &ru)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	return &Rusage{
		User:        time.Duration(ru.Utime.Nano()),
		System:      time.Duration(ru.Stime.Nano()),
		MaxRSSBytes: ru.Maxrss, // darwin reports bytes
	}, nil
}

func (h *handle) signalGroup(sig syscall.Signal) error {
	return unix.Kill(-h.pid, sig)
}

var errStale = errors.New("proc: process identity changed")

func signalMember(m Member, sig syscall.Signal) error {
	cur, err := procByPid(m.Pid)
	if err != nil {
		return err
	}
	if cur.Start != m.Start {
		return errStale
	}
	return unix.Kill(m.Pid, sig) // reuse window between check and kill
}

func procByPid(pid int) (Member, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Member{}, err
	}
	return fromKinfo(k), nil
}

func listProcs() ([]Member, error) {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(ks))
	for i := range ks {
		out = append(out, fromKinfo(&ks[i]))
	}
	return out, nil
}

func fromKinfo(k *unix.KinfoProc) Member {
	pid := int(k.Proc.P_pid)
	sid, err := unix.Getsid(pid)
	if err != nil {
		sid = -1
	}
	state := byte('S')
	switch k.Proc.P_stat {
	case 5: // SZOMB
		state = 'Z'
	case 4: // SSTOP
		state = 'T'
	case 2: // SRUN
		state = 'R'
	}
	comm := k.Proc.P_comm[:]
	for i, c := range comm {
		if c == 0 {
			comm = comm[:i]
			break
		}
	}
	return Member{
		Pid:   pid,
		PPid:  int(k.Eproc.Ppid),
		Pgid:  int(k.Eproc.Pgid),
		Sid:   sid,
		Start: uint64(k.Proc.P_starttime.Sec)*1e6 + uint64(k.Proc.P_starttime.Usec),
		State: state,
		Comm:  string(comm),
	}
}
