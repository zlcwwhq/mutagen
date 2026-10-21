package remote

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/filesystem/locking"
)

// replacementsDirectoryName is the name of the Mutagen data subdirectory in
// which replacements lock the entries that they replace and record the
// sequence numbers of the replacements admitted for them (see
// lockReplacement).
const replacementsDirectoryName = "replacements"

// ErrSuperseded indicates that a replacement was refused, without changing its
// entry, because a replacement with a later sequence number was admitted for
// the entry first (see Replacement.Sequence), such as one made by a retry of a
// resolution whose client stopped waiting for the refused replacement.
var ErrSuperseded = errors.New("a later apply of this conflict, through either endpoint, reached the copy first")

// supersededError is a superseded replacement reported by the opposite
// endpoint, which matches ErrSuperseded.
type supersededError struct {
	message string
}

func (e *supersededError) Error() string {
	return e.message
}

func (e *supersededError) Is(target error) bool {
	return target == ErrSuperseded
}

// replacementLocks excludes the replacements of an entry that this process
// makes from each other, which their file locks (see lockReplacement) don't do
// on POSIX systems, where a process's record locks don't conflict with each
// other, and where closing any of a process's descriptors for a file releases
// all of its locks on it. It maps the lock files of the entries being replaced
// to channels that are closed once they're unlocked.
var replacementLocks struct {
	sync.Mutex
	held map[string]chan struct{}
}

// replacementLock is a lock on an entry for a replacement (see
// lockReplacement).
type replacementLock struct {
	// record is the path of the file that records the sequence number of the
	// latest replacement admitted for the entry (see admit).
	record string
	// unlock releases the lock.
	unlock func()
}

// lockReplacement locks the entry at a slash-separated path relative to a
// normalized root (a dot for the root itself) for a replacement, waiting for
// other replacements of the entry to finish, unless the context is cancelled
// first. Locks are file locks in the Mutagen data directory, so that they also
// exclude the replacements that other processes on the system make: those of
// successive executors of an endpoint, since an executor that a reconnection
// replaced may still be finishing a replacement, and those that resolutions'
// clients make of their local copies, which share the data directory with the
// executors of their endpoints, since they locate them through its command
// registry. Lock files are identified by the root and path, and they're kept,
// since removing one that another process is about to lock would let both
// hold their locks.
//
// Waiting can be cancelled, but a lock that's been acquired, even after its
// context was cancelled, is only released once its replacement has finished,
// since the replacement may already be writing its result: a replacement whose
// client stopped waiting for it still excludes later ones until it's done.
func lockReplacement(ctx context.Context, directory, path string) (*replacementLock, error) {
	base, err := filesystem.Mutagen(true, replacementsDirectoryName)
	if err != nil {
		return nil, fmt.Errorf("unable to create replacements directory: %w", err)
	}
	digest := sha256.Sum256([]byte(directory + "\x00" + path))
	name := filepath.Join(base, fmt.Sprintf("%x", digest[:16]))

	// Wait for this process's other replacements of the entry.
	for {
		replacementLocks.Lock()
		unlocked, held := replacementLocks.held[name]
		if !held {
			if replacementLocks.held == nil {
				replacementLocks.held = make(map[string]chan struct{})
			}
			replacementLocks.held[name] = make(chan struct{})
			replacementLocks.Unlock()
			break
		}
		replacementLocks.Unlock()
		select {
		case <-unlocked:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	release := func() {
		replacementLocks.Lock()
		close(replacementLocks.held[name])
		delete(replacementLocks.held, name)
		replacementLocks.Unlock()
	}

	// Wait for other processes' replacements of the entry. Waiting for a file
	// lock can't be interrupted, so if the context is cancelled first, the
	// lock is released once it's acquired, and the lock file is closed before
	// this process's other replacements of the entry may proceed.
	locker, err := locking.NewLocker(name+".lock", 0600)
	if err != nil {
		release()
		return nil, fmt.Errorf("unable to lock the copy for replacement: %w", err)
	} else if locker.Lock(false) != nil {
		locked := make(chan error, 1)
		go func() {
			locked <- locker.Lock(true)
		}()
		select {
		case err = <-locked:
		case <-ctx.Done():
			go func() {
				if <-locked == nil {
					locker.Unlock()
				}
				locker.Close()
				release()
			}()
			return nil, ctx.Err()
		}
		if err != nil {
			locker.Close()
			release()
			return nil, fmt.Errorf("unable to lock the copy for replacement: %w", err)
		}
	}
	return &replacementLock{
		record: name + ".json",
		unlock: func() {
			locker.Unlock()
			locker.Close()
			release()
		},
	}, nil
}

// replacementRecord records the latest replacement admitted for an entry (see
// replacementLock.admit).
type replacementRecord struct {
	// Root and Path identify the entry, as for lockReplacement, for reference.
	Root, Path string
	// Sequence is the replacement's sequence number.
	Sequence uint64
}

// admit admits a replacement of the locked entry at a slash-separated path
// relative to a normalized root (see lockReplacement) with the specified
// sequence number, failing with ErrSuperseded if a replacement with a later
// one was admitted for the entry before. Otherwise, it durably records the
// sequence number, before the replacement checks or changes the entry, so that
// a replacement with an earlier one is refused afterward, even if this one
// fails, or finds that the entry already has its result.
func (l *replacementLock) admit(directory, path string, sequence uint64) error {
	var record replacementRecord
	if data, err := os.ReadFile(l.record); err == nil {
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("the replacement record in %s is corrupt (%v); remove it", l.record, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unable to read replacement record: %w", err)
	}
	if sequence < record.Sequence {
		return ErrSuperseded
	} else if sequence == record.Sequence {
		return nil
	}
	data, err := json.Marshal(&replacementRecord{Root: directory, Path: path, Sequence: sequence})
	if err != nil {
		return fmt.Errorf("unable to encode replacement record: %w", err)
	} else if err := WriteFileDurably(l.record, data); err != nil {
		return fmt.Errorf("unable to save replacement record: %w", err)
	}
	return nil
}

// WriteFileDurably atomically replaces the file at a path with a private file
// containing data, like filesystem.WriteFileAtomic, but flushes the new file
// before renaming it over the path and flushes the directory afterward (see
// SyncDirectory), so that the file has either its previous or its new contents
// after a power loss or an operating system crash, rather than possibly
// neither, as renaming an unflushed file can leave it.
func WriteFileDurably(path string, data []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, filesystem.TemporaryNamePrefix+"durable-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	} else if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return SyncDirectory(directory)
}
