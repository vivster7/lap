//go:build !unix

package pool

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("pool: flock is not supported on this platform")

func tryFlock(*os.File) (bool, error) { return false, errUnsupported }

func ensurePrivateDir(dir string) error { return os.MkdirAll(dir, 0o700) }
