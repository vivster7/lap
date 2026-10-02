//go:build !linux

package proc

import "os"

func platformHelper(name string, inner []string) {
	say("%s-failed unsupported on this platform", name)
	os.Exit(3)
}
