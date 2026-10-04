package store

import "golang.org/x/sys/unix"

func totalMemory() int64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(n)
}

// onBattery is unknown on darwin: querying IOKit or pmset is not worth the
// startup cost for an optional field.
func onBattery() *bool { return nil }
