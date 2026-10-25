//go:build !windows && !aix

package remote

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// logIdentity identifies the file that the specified information describes by
// its device and inode numbers, so that a log that was replaced at its path
// can be recognized. It's empty if they're unknown.
func logIdentity(info os.FileInfo) string {
	if metadata, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", metadata.Dev, metadata.Ino)
	}
	return ""
}

// openJobLog opens a log for reading without blocking if its path has been
// replaced by a FIFO. The opened descriptor must refer to a regular file;
// checking it after opening also covers replacements racing with the open.
func openJobLog(path string) (*os.File, error) {
	var descriptor int
	var err error
	for {
		descriptor, err = unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(descriptor), path)
	if info, err := file.Stat(); err != nil {
		file.Close()
		return nil, err
	} else if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return file, nil
}
