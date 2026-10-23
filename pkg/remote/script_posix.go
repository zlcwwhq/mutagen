//go:build !windows && !aix

package remote

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// lockScript opens the temporary file of a script run in the foreground and
// takes a shared lock on it, which the executor holds, by keeping the returned
// file open, until the script has exited and the file has been removed (see
// preparedCommand.cleanup), so that pruneScripts keeps the file while the
// script runs. Keeping it matters even though the interpreter has opened the
// file, since processes may read it again by its path while the script runs:
// Python's multiprocessing workers, for example, import the script as their
// main module when spawned, and tracebacks read lines of its source.
//
// The lock belongs to the returned file's open file description (see flock), so
// it conflicts with the exclusive locks that pruneScripts takes through other
// descriptions, including in this process, and the file is opened
// close-on-exec, so commands don't inherit it. The lock is taken without
// blocking, since only pruneScripts takes conflicting locks, and only on old
// files. If the file can't be opened or locked, then nil is returned, and the
// script runs without the lock. Where that's because the filesystem doesn't
// support locks, pruneScripts can't lock the file either, so it keeps the file.
func lockScript(path string) *os.File {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	if err := flock(file, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		file.Close()
		return nil
	}
	return file
}

// pruneScripts removes, from the specified directory, the files of scripts run
// in the foreground that are older than scriptRetention and no longer in use.
// Executors remove these files once their scripts exit, so this only removes
// those left behind, such as by an executor that was killed. Entries other than
// regular files named like script files are ignored.
//
// An executor holds a lock on its script's file until the script exits (see
// lockScript), and a file is only removed if it can be locked exclusively, and
// while the lock is held, so the file of a script remains for as long as the
// executor that runs it waits for it, however long the script runs. If the
// executor exits first, then its lock is released, since the script doesn't
// inherit it, and the file is removed once it's older than scriptRetention, as
// are the files of scripts run by executors of earlier versions, which don't
// lock them. Files that can't be opened or locked, such as on filesystems that
// don't support locks, are kept. Files are opened without blocking, in case one
// is replaced by a FIFO after being listed. Script files are created
// exclusively, so no other file can take a file's name between locking it and
// removing it.
func pruneScripts(directory string) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), scriptFilePrefix) {
			continue
		} else if info, err := entry.Info(); err != nil || time.Since(info.ModTime()) <= scriptRetention {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		if flock(file, unix.LOCK_EX|unix.LOCK_NB) == nil {
			os.Remove(path)
		}
		file.Close()
	}
}
