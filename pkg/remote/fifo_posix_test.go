//go:build !windows

package remote

import (
	"syscall"
)

// mkfifo creates a named pipe.
func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0600)
}
