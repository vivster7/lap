//go:build unix && !linux

package term

import "os"

// pollableMaster leaves the master in blocking mode outside Linux. On darwin
// creack/pty opens /dev/ptmx blocking and kqueue support for PTY masters was
// not verified in this spike: a reader blocked past the drain deadline is
// abandoned (see SPIKE.md open issues).
func pollableMaster(m *os.File) (*os.File, error) { return m, nil }

const masterPollable = false
