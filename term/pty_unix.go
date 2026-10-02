//go:build unix

package term

import (
	"os"

	"golang.org/x/sys/unix"
)

// setWinsize sets the terminal size without calling (*os.File).Fd, which
// would switch a pollable master back to blocking mode. creack/pty v1.1.24's
// Setsize and Open both call Fd().
func setWinsize(f *os.File, cols, rows uint16) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	err = rc.Control(func(fd uintptr) {
		ierr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	})
	if err != nil {
		return err
	}
	return ierr
}
