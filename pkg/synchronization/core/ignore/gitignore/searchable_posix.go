//go:build !windows

package gitignore

import (
	"golang.org/x/sys/unix"
)

// isSearchable determines whether or not Git can search the entry at a path, as
// Git determines it for the entries of a Git directory, which is with access(2)
// and X_OK, following symbolic links.
func isSearchable(path string) bool {
	return unix.Access(path, unix.X_OK) == nil
}

// isInvalidName determines whether or not an error indicates that a path can't
// name anything on this system. On POSIX systems, any path can.
func isInvalidName(err error) bool {
	return false
}
