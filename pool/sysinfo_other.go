//go:build !linux && !darwin

package pool

import "runtime"

func usableCPUs() int { return max(1, runtime.NumCPU()) }

func totalMemory() int64 { return 0 }

func procStart(int) uint64 { return 0 }
