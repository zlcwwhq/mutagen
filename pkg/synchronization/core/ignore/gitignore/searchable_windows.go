package gitignore

import (
	"errors"
	"os"
	"syscall"
)

// isSearchable determines whether or not Git can search the entry at a path, as
// Git determines it for the entries of a Git directory. Git for Windows emulates
// access(2), whose X_OK check it reduces to a check of existence, following
// symbolic links.
func isSearchable(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// errorInvalidName is Windows' ERROR_INVALID_NAME, which indicates that a path
// can't name anything, such as because it contains a line feed.
const errorInvalidName = syscall.Errno(123)

// isInvalidName determines whether or not an error indicates that a path can't
// name anything on this system.
func isInvalidName(err error) bool {
	return errors.Is(err, errorInvalidName)
}
