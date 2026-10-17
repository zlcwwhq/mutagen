package remote

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames a file to a path in the same directory at which
// nothing exists, failing with an error matching fs.ErrExist rather than
// replace a file created there concurrently. It fails with an error matching
// errors.ErrUnsupported if the kernel or filesystem doesn't support this.
func renameNoReplace(root *os.Root, from, to string) error {
	directory, err := root.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer directory.Close()
	control, err := directory.SyscallConn()
	if err != nil {
		return err
	}
	controlErr := control.Control(func(descriptor uintptr) {
		for {
			err = unix.Renameat2(int(descriptor), filepath.Base(from), int(descriptor), filepath.Base(to), unix.RENAME_NOREPLACE)
			if err != unix.EINTR {
				return
			}
		}
	})
	if controlErr != nil {
		return controlErr
	} else if err == unix.EINVAL || err == unix.ENOSYS {
		// Filesystems that don't support RENAME_NOREPLACE fail with EINVAL,
		// which this invocation can't otherwise cause.
		return errors.ErrUnsupported
	}
	return err
}
