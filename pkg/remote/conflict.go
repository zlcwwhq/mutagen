package remote

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/identifier"
)

// ConflictRoot describes a conflict listed by ListConflicts.
type ConflictRoot struct {
	// Path is the slash-separated conflict root relative to the
	// synchronization root.
	Path string
	// Resolvable indicates whether both endpoints have a regular file at the
	// conflict root, which is the only kind of conflict that remote resolves.
	Resolvable bool
}

// conflictsMessage is a message of a conflict listing, which consists of one
// message per conflict followed by a message with Done set. Hops that are
// unable to reach the daemon send a status instead, which decodes compatibly.
type conflictsMessage struct {
	// Error is the flush or routing error, if any, which ends the listing.
	Error string `json:",omitempty"`
	// Conflict is the listed conflict, if any.
	Conflict *ConflictRoot `json:",omitempty"`
	// Done indicates the end of a complete listing.
	Done bool `json:",omitempty"`
}

// RespondWithConflicts sends the outcome of a conflicts submission and closes
// the connection. Conflicts are sent in separate messages, so the listing isn't
// bounded by the message size limit. It returns any error encountered sending
// the response.
func RespondWithConflicts(connection io.ReadWriteCloser, conflicts []ConflictRoot, err error) error {
	defer connection.Close()
	if err != nil {
		return writeMessage(connection, &conflictsMessage{Error: err.Error()})
	}
	writer := bufio.NewWriter(connection)
	for c := range conflicts {
		if err := writeMessage(writer, &conflictsMessage{Conflict: &conflicts[c]}); err != nil {
			return err
		}
	}
	if err := writeMessage(writer, &conflictsMessage{Done: true}); err != nil {
		return err
	}
	return writer.Flush()
}

// ListConflicts waits for the synchronization cycle requested by a conflicts
// submission (see Dial) to complete and returns all of the session's
// conflicts, sorted by root path. It owns and closes the connection.
func ListConflicts(ctx context.Context, connection io.ReadWriteCloser) ([]ConflictRoot, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	reader := bufio.NewReader(connection)
	var conflicts []ConflictRoot
	for {
		var message conflictsMessage
		if err := readMessage(reader, &message); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("unable to receive conflicts: %w", err)
		} else if message.Error != "" {
			return nil, errors.New(message.Error)
		} else if message.Done {
			return conflicts, nil
		} else if message.Conflict == nil {
			return nil, errors.New("invalid conflict listing")
		}
		conflicts = append(conflicts, *message.Conflict)
	}
}

// Replacement describes a conditional replacement of a file (see ReplaceFile).
type Replacement struct {
	// Path is the slash-separated path of the file relative to the root.
	Path string
	// Expected lists the regular files that the replacement may supersede.
	// Resolutions list both checked-out copies, since synchronization may copy
	// either one to the other endpoint before both are replaced.
	Expected []*Entry
	// Result is the replacement regular file, or a missing entry to delete the
	// file. Its data, if any, is ignored.
	Result *Entry
}

// errStale indicates that a replacement would overwrite unseen changes.
var errStale = errors.New("file changed since it was checked out")

// supersedes determines whether a replacement may supersede a file: one of its
// expected files, or one with the result's contents but different
// executability. Executability isn't otherwise compared, since filesystems that
// don't preserve it (such as those on Windows) report it regardless of how a
// file was written or synchronized, and retries would fail indefinitely.
func (r *Replacement) supersedes(current *Entry) bool {
	return current.Kind == EntryFile && (SameContents(current, r.Result) ||
		slices.ContainsFunc(r.Expected, func(expected *Entry) bool {
			return SameContents(current, expected)
		}))
}

// check reads the file that a replacement replaces and determines whether the
// replacement is complete or may supersede the file, in which case it returns
// the file's information as read. It fails if neither is the case.
func (r *Replacement) check(ctx context.Context, root *os.Root, path string) (bool, os.FileInfo, error) {
	current, info, err := copyFile(ctx, root, path, io.Discard)
	if err != nil {
		return false, nil, err
	} else if SameFile(current, r.Result) {
		return true, nil, nil
	} else if !r.supersedes(current) {
		return false, nil, errStale
	}
	return false, info, nil
}

// CheckFile determines whether a replacement (see ReplaceFile) may replace the
// file that it describes or would leave it as is, failing if the file changed
// since it was checked out. Resolutions check one copy before replacing the
// other, so that a copy changed beforehand doesn't leave only one replaced.
func CheckFile(ctx context.Context, directory string, replacement *Replacement) error {
	root, path, err := openFile(directory, replacement.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	_, _, err = replacement.check(ctx, root, path)
	return err
}

// SameContents determines whether two entries describe regular files with the
// same contents, or are both missing.
func SameContents(a, b *Entry) bool {
	if a == nil || b == nil || a.Kind != b.Kind {
		return false
	} else if a.Kind == EntryMissing {
		return true
	}
	return a.Kind == EntryFile && a.Size == b.Size && a.Digest == b.Digest
}

// SameFile determines whether two entries describe the same regular file
// contents and executability, or are both missing.
func SameFile(a, b *Entry) bool {
	return SameContents(a, b) && a.Executable == b.Executable
}

// unchanged determines whether two observations of a path describe the same
// file, with the same size, modification time, and mode.
func unchanged(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) &&
		a.Mode() == b.Mode()
}

// openFile opens a root for operations on a file at a slash-separated path
// beneath it, returning the corresponding native relative path. Symbolic links
// and other non-directories are refused as ancestors, while os.Root confines
// resolution if ancestors are replaced concurrently.
func openFile(directory, path string) (*os.Root, string, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, "", err
	}
	native := filepath.FromSlash(path)
	for parent := filepath.Dir(native); parent != "."; parent = filepath.Dir(parent) {
		if info, err := root.Lstat(parent); err != nil {
			root.Close()
			return nil, "", err
		} else if !info.IsDir() {
			root.Close()
			return nil, "", fmt.Errorf("%s is not a directory", filepath.ToSlash(parent))
		}
	}
	return root, native, nil
}

// copyFile hashes the regular file at a path, writing its contents to output,
// and returns its entry and its information as read. A missing file yields a
// missing entry (and no information). Final symbolic links aren't followed.
func copyFile(ctx context.Context, root *os.Root, path string, output io.Writer) (*Entry, os.FileInfo, error) {
	info, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Entry{Kind: EntryMissing}, nil, nil
	} else if err != nil {
		return nil, nil, err
	} else if !info.Mode().IsRegular() {
		return nil, nil, errors.New("not a regular file")
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	if opened, err := file.Stat(); err != nil {
		return nil, nil, err
	} else if !os.SameFile(info, opened) {
		return nil, nil, errors.New("file replaced while opening")
	}
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(output, hasher), &entryReader{ctx: ctx, reader: file})
	if err != nil {
		return nil, nil, err
	}

	// Editors may save by renaming a new file over the path, which leaves the
	// opened file intact, or may modify the file in place while it's read, so
	// check that the path still refers to the file as read.
	after, err := root.Lstat(path)
	if err != nil || !unchanged(info, after) || info.Size() != size {
		return nil, nil, errors.New("file changed while being read")
	}
	return &Entry{
		Kind:       EntryFile,
		Size:       size,
		Digest:     fmt.Sprintf("%x", hasher.Sum(nil)),
		Executable: info.Mode()&0111 != 0,
	}, after, nil
}

// CopyFile writes the complete contents of the regular file at a slash-separated
// path relative to a root to output and returns its entry, without retaining
// data. A missing file yields a missing entry.
func CopyFile(ctx context.Context, directory, path string, output io.Writer) (*Entry, error) {
	root, path, err := openFile(directory, path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entry, _, err := copyFile(ctx, root, path, output)
	return entry, err
}

// temporaryName generates a unique name for a temporary file next to a path,
// which synchronization ignores.
func temporaryName(path string) (string, error) {
	name, err := identifier.New("temp")
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), filesystem.TemporaryNamePrefix+name), nil
}

// place moves a file to a path in the same directory at which nothing exists.
// It fails with an error matching fs.ErrExist rather than replace a file
// created there concurrently, which linking never does. Not every filesystem
// supports hard links, so it otherwise renames the file if that's possible
// without replacing files. Checking that the path is free and then renaming
// would leave a window in which a concurrently saved file is replaced, so it
// fails with an error matching errors.ErrUnsupported instead.
func place(root *os.Root, from, to string) error {
	err := root.Link(from, to)
	if err == nil {
		// A remaining name is harmless, since synchronization ignores it and
		// it refers to the same file.
		root.Remove(from)
		return nil
	} else if errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if renameErr := renameNoReplace(root, from, to); !errors.Is(renameErr, errors.ErrUnsupported) {
		return renameErr
	}
	return fmt.Errorf("%w: the filesystem supports neither hard links nor renaming without replacing files, which replacing files safely requires (%v)",
		errors.ErrUnsupported, errors.Unwrap(err))
}

// placeable checks that files can be placed in the directory containing a path
// (see place), using a temporary file.
func placeable(root *os.Root, path string) error {
	probe, err := temporaryName(path)
	if err != nil {
		return err
	}
	file, err := root.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(probe)
	if err := file.Close(); err != nil {
		return err
	}
	placed, err := temporaryName(path)
	if err != nil {
		return err
	} else if err := place(root, probe, placed); err != nil {
		return err
	}
	return root.Remove(placed)
}

// supersede replaces the file at a path, which must be the file described by
// information read when checking it, with a staged file, or deletes it if staged
// is empty. Checking the file at the path and then replacing it would leave a
// window in which a concurrently saved file is overwritten, so the file is
// first moved aside, which atomically captures whatever is at the path, and
// is only deleted once it's verified. Otherwise, it's moved back, and neither
// it nor the staged file overwrites a file saved at the path meanwhile, which
// requires that files can be placed (see place), so that's checked first. The
// path is thus briefly missing, which synchronization may observe like any
// other intermediate state of a resolution. Writes through a file opened before
// it was moved aside can't be detected once it's verified, just as they can't
// when renaming over it.
func supersede(root *os.Root, path string, checked os.FileInfo, staged string) error {
	if err := placeable(root, path); err != nil {
		return err
	}
	aside, err := temporaryName(path)
	if err != nil {
		return err
	}
	if err := root.Rename(path, aside); errors.Is(err, fs.ErrNotExist) {
		return errStale
	} else if err != nil {
		return err
	}
	if captured, err := root.Lstat(aside); err != nil {
		return restore(root, aside, path, err)
	} else if !unchanged(checked, captured) {
		return restore(root, aside, path, errStale)
	} else if staged != "" {
		if err := place(root, staged, path); errors.Is(err, fs.ErrExist) {
			// A file saved at the path after the checked file was moved aside
			// supersedes it, just as it would have without the replacement.
			root.Remove(aside)
			return errStale
		} else if err != nil {
			return restore(root, aside, path, err)
		}
	}
	return root.Remove(aside)
}

// restore moves a file that was moved aside back to its path and returns the
// error that prevented its replacement. If a file was saved at the path
// meanwhile, it's left in place, and the error says where the other one is.
func restore(root *os.Root, aside, path string, err error) error {
	if restoreErr := place(root, aside, path); restoreErr != nil {
		return fmt.Errorf("%w; it was moved aside to %s and couldn't be moved back: %v",
			err, filepath.ToSlash(aside), restoreErr)
	}
	return err
}

// replace supersedes the checked file at a path with a staged file, or deletes
// it if staged is empty (see supersede). Synchronization may propagate the
// result meanwhile, which fails the replacement as stale, so the file is checked
// again, and the replacement is complete if the file has the result. A file
// left aside is reported regardless, since its error only wraps errStale.
func (r *Replacement) replace(ctx context.Context, root *os.Root, path string, checked os.FileInfo, staged string) error {
	err := supersede(root, path, checked, staged)
	if err == errStale {
		if done, _, checkErr := r.check(ctx, root, path); checkErr == nil && done {
			return nil
		}
	}
	return err
}

// ReplaceFile replaces the file described by a replacement with contents read
// from input, if the file still matches one of its expected entries.
// Replacement is idempotent: a file that already matches the result, including
// one written concurrently, is left as is (and input isn't necessarily read).
// New contents are staged and verified next to the file and then take its
// place (see supersede), keeping its permissions apart from executability.
func ReplaceFile(ctx context.Context, directory string, replacement *Replacement, input io.Reader) error {
	result := replacement.Result
	if len(replacement.Expected) == 0 || slices.ContainsFunc(replacement.Expected, func(expected *Entry) bool {
		return expected == nil || expected.Kind != EntryFile
	}) || result == nil || (result.Kind != EntryFile && result.Kind != EntryMissing) || result.Size < 0 {
		return errors.New("invalid replacement")
	}
	root, path, err := openFile(directory, replacement.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	done, info, err := replacement.check(ctx, root, path)
	if err != nil || done {
		return err
	} else if result.Kind == EntryMissing {
		return replacement.replace(ctx, root, path, info, "")
	}

	// Stage the new contents.
	mode := info.Mode().Perm() &^ 0111
	if result.Executable {
		mode |= (mode & 0444) >> 2
	}
	temporary, err := temporaryName(path)
	if err != nil {
		return err
	}
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hasher), &entryReader{ctx: ctx, reader: io.LimitReader(input, result.Size+1)})
	if err == nil && (size != result.Size || fmt.Sprintf("%x", hasher.Sum(nil)) != result.Digest) {
		err = errors.New("replacement contents are incomplete or corrupt")
	}
	if err == nil {
		err = file.Chmod(mode)
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}

	// Staging may take a while, during which synchronization may also have
	// propagated the result, so check the file again immediately before
	// replacing it.
	if done, info, err = replacement.check(ctx, root, path); err != nil || done {
		return err
	}
	return replacement.replace(ctx, root, path, info, temporary)
}

// Copy requests the complete contents of a regular file at a slash-separated
// path, relative to the opposite endpoint's synchronization root, over an
// established route (see Dial) and writes them to output. It owns and closes
// the connection. A missing file yields a missing entry.
func Copy(ctx context.Context, connection io.ReadWriteCloser, path string, output io.Writer) (*Entry, error) {
	response, err := execute(ctx, connection, &Request{Copy: path}, strings.NewReader(""), output, io.Discard)
	if err != nil {
		return nil, err
	} else if response.Error != "" {
		return nil, errors.New(response.Error)
	} else if response.Entry == nil {
		return nil, errors.New("no entry received")
	}
	return response.Entry, nil
}

// Replace performs a replacement (see ReplaceFile) on the opposite endpoint
// over an established route (see Dial), sending input as the new contents. It
// owns and closes the connection.
func Replace(ctx context.Context, connection io.ReadWriteCloser, replacement *Replacement, input io.Reader) error {
	response, err := execute(ctx, connection, &Request{Replace: replacement}, input, io.Discard, io.Discard)
	if err != nil {
		return err
	} else if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}
