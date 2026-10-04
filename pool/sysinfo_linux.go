package pool

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

// usableCPUs returns the CPUs this process may run on (sched affinity),
// limited by any cgroup v2 cpu.max quota on its cgroup or an ancestor.
func usableCPUs() int {
	n := runtime.NumCPU()
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err == nil && set.Count() > 0 {
		n = set.Count()
	}
	for _, d := range cgroupDirs() {
		b, err := os.ReadFile(filepath.Join(d, "cpu.max"))
		if err != nil {
			continue
		}
		f := strings.Fields(string(b))
		if len(f) != 2 || f[0] == "max" {
			continue
		}
		quota, err1 := strconv.ParseInt(f[0], 10, 64)
		period, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
			continue
		}
		n = min(n, max(1, int((quota+period-1)/period)))
	}
	return max(1, n)
}

// totalMemory returns MemTotal, limited by any cgroup v2 memory.max on this
// process's cgroup or an ancestor. 0 if unknown.
func totalMemory() int64 {
	var total int64
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 2 && f[0] == "MemTotal:" {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					total = kb << 10
				}
				break
			}
		}
	}
	for _, d := range cgroupDirs() {
		b, err := os.ReadFile(filepath.Join(d, "memory.max"))
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			continue
		}
		if lim, err := strconv.ParseInt(s, 10, 64); err == nil && lim > 0 && (total == 0 || lim < total) {
			total = lim
		}
	}
	return total
}

// cgroupDirs returns this process's cgroup v2 directory and its ancestors
// (excluding the hierarchy root, which carries no limits). cgroup v1 is not
// consulted.
func cgroupDirs() []string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil
	}
	var rel string
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			rel, found = p, true
			break
		}
	}
	if !found {
		return nil
	}
	var dirs []string
	for d := filepath.Join(cgroupRoot, filepath.Clean("/"+rel)); d != cgroupRoot && strings.HasPrefix(d, cgroupRoot+"/"); d = filepath.Dir(d) {
		dirs = append(dirs, d)
	}
	if len(dirs) == 0 {
		// Inside a cgroup namespace the process's cgroup appears as the
		// root; its limit files (if any) are still meaningful there.
		dirs = append(dirs, cgroupRoot)
	}
	return dirs
}

// procStart returns the start time of pid in clock ticks since boot
// (/proc/<pid>/stat field 22), or 0 if unknown.
func procStart(pid int) uint64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(b[i+1:]))
	// f[0] is field 3 (state); field 22 is f[19].
	if len(f) < 20 {
		return 0
	}
	v, _ := strconv.ParseUint(f[19], 10, 64)
	return v
}
