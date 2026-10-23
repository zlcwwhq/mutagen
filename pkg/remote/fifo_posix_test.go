//go:build !windows

package remote

import (
	"golang.org/x/sys/unix"
)

// mkfifo creates a named pipe. It uses the unix package, since the syscall
// package lacks Mkfifo on some systems, such as AIX.
func mkfifo(path string) error {
	return unix.Mkfifo(path, 0600)
}
