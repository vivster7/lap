package store

import (
	"os"
	"runtime"
)

// Machine describes the host a run executed on. Zero values mean unknown.
type Machine struct {
	Hostname    string `json:"hostname,omitempty"`
	OS          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	CPUs        int    `json:"cpus,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
	OnBattery   *bool  `json:"on_battery,omitempty"`
}

// DetectMachine describes this host. MemoryBytes is 0 if unknown; OnBattery
// is best effort (Linux /sys/class/power_supply) and nil if unknown.
func DetectMachine() Machine {
	h, _ := os.Hostname()
	return Machine{
		Hostname:    h,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		CPUs:        runtime.NumCPU(),
		MemoryBytes: totalMemory(),
		OnBattery:   onBattery(),
	}
}
