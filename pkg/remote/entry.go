package remote

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"syscall"
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
// symbolic link. Cancellation interrupts reading and hashing file contents.
func ReadEntry(ctx context.Context, path string) (*Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		// A parent may be a file on this endpoint and a directory on the
		// other, in which case its children are missing on this endpoint.
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
		return &Entry{Kind: EntrySymbolicLink, Target: normalizeTarget(target)}, nil
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
	stop := context.AfterFunc(ctx, func() { file.Close() })
	defer stop()
	return readFileEntry(ctx, file, info.Size(), info.Mode()&0111 != 0)
}

// normalizeTarget converts a symbolic link target, as read, to the protocol's
// representation, which separates components with slashes, like
// synchronization does (see normalizeSymbolicLinkAndEnsurePortable in
// pkg/synchronization/core/symbolic_link.go). Windows reads targets with
// backslashes, which would be filename characters if a link with that target
// were created on a POSIX endpoint, so they're converted to slashes. Targets
// read on POSIX systems are left as they are, since backslashes are filename
// characters there. No conversion is needed when creating links, since Go
// converts slashes to backslashes when creating them on Windows.
func normalizeTarget(target string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(target, "\\", "/")
	}
	return target
}

// entryReader checks for cancellation between file reads. Wrapping the reader
// also prevents io.Copy from bypassing these checks via an optimized copy.
type entryReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *entryReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(buffer)
	if cancellation := r.ctx.Err(); cancellation != nil {
		return n, cancellation
	}
	return n, err
}

// readFileEntry reads and hashes file contents, retaining small files for
// textual comparison. The size hint may be stale if the file is being modified.
func readFileEntry(ctx context.Context, source io.Reader, size int64, executable bool) (*Entry, error) {
	reader := &entryReader{ctx: ctx, reader: source}
	entry := &Entry{Kind: EntryFile, Executable: executable}
	hasher := sha256.New()
	var err error
	if size <= MaximumComparedFileSize {
		if entry.Data, err = io.ReadAll(io.LimitReader(reader, MaximumComparedFileSize+1)); err != nil {
			return nil, err
		}
		hasher.Write(entry.Data)
		entry.Size = int64(len(entry.Data))
		if entry.Size > MaximumComparedFileSize {
			entry.Data = nil
			copied, err := io.Copy(hasher, reader)
			if err != nil {
				return nil, err
			}
			entry.Size += copied
		}
	} else if entry.Size, err = io.Copy(hasher, reader); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry.Digest = fmt.Sprintf("%x", hasher.Sum(nil))
	return entry, nil
}
