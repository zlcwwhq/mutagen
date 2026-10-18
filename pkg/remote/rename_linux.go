package remote

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// renameAt renames a file to a path in the same directory using renameat2 with
// the specified flags. It fails with errors.ErrUnsupported if the kernel or
// filesystem doesn't support the flags.
func renameAt(root *os.Root, from, to string, flags uint) error {
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
			err = unix.Renameat2(int(descriptor), filepath.Base(from), int(descriptor), filepath.Base(to), flags)
			if err != unix.EINTR {
				return
			}
		}
	})
	if controlErr != nil {
		return controlErr
	} else if err == unix.EINVAL || err == unix.ENOSYS {
		// Filesystems that don't support a flag fail with EINVAL, which these
		// invocations can't otherwise cause.
		return errors.ErrUnsupported
	}
	return err
}

// renameNoReplace renames a file to a path in the same directory at which
// nothing exists, failing with an error matching fs.ErrExist rather than
// replace a file created there concurrently. It fails with an error matching
// errors.ErrUnsupported if the kernel or filesystem doesn't support this.
func renameNoReplace(root *os.Root, from, to string) error {
	return renameAt(root, from, to, unix.RENAME_NOREPLACE)
}

// exchange atomically exchanges two files in the same directory, so that
// neither path is ever missing. It fails with an error matching
// errors.ErrUnsupported if the kernel or filesystem doesn't support this.
func exchange(root *os.Root, a, b string) error {
	return renameAt(root, a, b, unix.RENAME_EXCHANGE)
}
