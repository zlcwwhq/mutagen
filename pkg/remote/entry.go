package remote

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// MaximumComparedFileSize is the size of the largest files whose contents
// ReadEntry includes, which bounds the files that can be compared line by line.
const MaximumComparedFileSize = 1024 * 1024

// Entry kinds reported by ReadEntry.
const (
	// EntryMissing indicates that no entry exists.
	EntryMissing = "missing"
	// EntryFile indicates a regular file.
	EntryFile = "file"
	// EntryDirectory indicates a directory.
	EntryDirectory = "directory"
	// EntrySymbolicLink indicates a symbolic link.
	EntrySymbolicLink = "symbolic link"
	// EntryOther indicates any other kind of entry.
	EntryOther = "other"
)

// Entry describes a filesystem entry for comparison with its counterpart on
// the opposite endpoint.
type Entry struct {
	// Kind is the entry kind.
	Kind string
	// Target is the target of a symbolic link.
	Target string `json:",omitempty"`
	// Size is the size of a file.
	Size int64 `json:",omitempty"`
	// Executable indicates whether a file is executable.
	Executable bool `json:",omitempty"`
	// Digest is the hexadecimal SHA-256 digest of a file's contents.
	Digest string `json:",omitempty"`
	// Data contains the contents of a file no larger than
	// MaximumComparedFileSize. It's transmitted separately from the entry.
	Data []byte `json:"-"`
}

// ReadEntry reads the entry at the specified path, without following a final
// symbolic link.
func ReadEntry(path string) (*Entry, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Entry{Kind: EntryMissing}, nil
	} else if err != nil {
		return nil, err
	}
	switch mode := info.Mode(); {
	case mode.IsDir():
		return &Entry{Kind: EntryDirectory}, nil
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		return &Entry{Kind: EntrySymbolicLink, Target: target}, nil
	case !mode.IsRegular():
		return &Entry{Kind: EntryOther}, nil
	}

	// Read small files in their entirety and only hash larger ones. Sizes are
	// recomputed in case the file changes while being read.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entry := &Entry{Kind: EntryFile, Executable: info.Mode()&0111 != 0}
	hasher := sha256.New()
	if info.Size() <= MaximumComparedFileSize {
		if entry.Data, err = io.ReadAll(io.LimitReader(file, MaximumComparedFileSize+1)); err != nil {
			return nil, err
		}
		hasher.Write(entry.Data)
		entry.Size = int64(len(entry.Data))
		if entry.Size > MaximumComparedFileSize {
			entry.Data = nil
			copied, err := io.Copy(hasher, file)
			if err != nil {
				return nil, err
			}
			entry.Size += copied
		}
	} else if entry.Size, err = io.Copy(hasher, file); err != nil {
		return nil, err
	}
	entry.Digest = fmt.Sprintf("%x", hasher.Sum(nil))
	return entry, nil
}
