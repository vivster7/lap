package term

import (
	"os"
	"syscall"
)

// pollableMaster returns the PTY master as a file registered with the Go
// runtime poller, so read deadlines work and Close interrupts a blocked Read.
//
// creack/pty v1.1.24 opens /dev/ptmx pollable but then calls Fd() (through
// its ioctl helper for TIOCGPTN/TIOCSPTLCK), which puts the descriptor in
// blocking mode permanently. A blocking master cannot be interrupted at the
// drain deadline: the reader goroutine and the fd would stay alive until the
// last slave holder exits. See TestCreackMasterIsBlocking.
func pollableMaster(m *os.File) (*os.File, error) {
	fd, err := syscall.Dup(int(m.Fd()))
	if err != nil {
		return nil, err
	}
	m.Close()
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), nil
}

const masterPollable = true
