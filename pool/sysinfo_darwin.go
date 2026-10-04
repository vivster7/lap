package pool

import (
	"runtime"

	"golang.org/x/sys/unix"
)

func usableCPUs() int { return max(1, runtime.NumCPU()) }

// totalMemory returns hw.memsize, or 0 if unknown.
func totalMemory() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v)
}

// procStart returns the start time of pid in microseconds since the epoch,
// or 0 if unknown.
func procStart(pid int) uint64 {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return 0
	}
	t := k.Proc.P_starttime
	return uint64(t.Sec)*1e6 + uint64(t.Usec)
}
