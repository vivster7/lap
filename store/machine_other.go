//go:build !linux && !darwin

package store

func totalMemory() int64 { return 0 }

func onBattery() *bool { return nil }
