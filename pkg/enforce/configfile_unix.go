//go:build darwin || linux

package enforce

import (
	"os"

	"golang.org/x/sys/unix"
)

// openConfigFilePlatform opens path without allowing a FIFO or device substituted at
// the final path component to block the open. readConfigUncached validates the
// resulting descriptor before attempting to read it.
func openConfigFilePlatform(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
