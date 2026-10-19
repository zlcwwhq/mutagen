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
	"runtime"
	"slices"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

// ConflictRoot describes a conflict listed by ListConflicts.
type ConflictRoot struct {
	// Path is the slash-separated conflict root relative to the
	// synchronization root, which is empty for the root itself.
	Path string
	// Files indicates whether both endpoints have a regular file at the
	// conflict root, in which case remote can write a result, such as a merge
	// of both copies, to both endpoints.
	Files bool `json:",omitempty"`
	// AlphaTakable and BetaTakable indicate whether remote can resolve the
	// conflict by taking alpha's or beta's copy unchanged. For conflicts
	// between two files, the copy is written to the other endpoint, while for
	// others, the other copy is deleted, so that synchronization propagates
	// the taken copy.
	AlphaTakable bool `json:",omitempty"`
	BetaTakable  bool `json:",omitempty"`
	// AlphaDigest and BetaDigest are the hexadecimal digests of alpha's and
	// beta's files, for conflicts between two files, as the synchronization
	// cycle that listed the conflict saw them. Copies downloaded afterward are
	// compared with them, since synchronization may have moved on to newer
	// copies with another common ancestor (see BaseDigest) meanwhile.
	AlphaDigest string `json:",omitempty"`
	BetaDigest  string `json:",omitempty"`
	// BaseDigest is the hexadecimal digest of the file at the conflict root
	// when the endpoints last agreed, if there was one, which identifies the
	// copies' common ancestor as of the same cycle. Mutagen doesn't retain its
	// contents, but they can be found by comparing digests.
	BaseDigest string `json:",omitempty"`
	// Algorithm is the session's hashing algorithm, which computed the digests
	// of conflicts between two files, in its textual form.
	Algorithm string `json:",omitempty"`
}

// Resolvable indicates whether remote can resolve a conflict in some way.
func (c *ConflictRoot) Resolvable() bool {
	return c.Files || c.AlphaTakable || c.BetaTakable
}

// Listing is a complete listing of a session's conflicts and problems as of a
// synchronization cycle.
type Listing struct {
	// Conflicts are the conflicts, sorted by root path.
	Conflicts []ConflictRoot
	// Problems are the problems that the endpoints encountered.
	Problems []Problem
}

// conflictsMessage is a message of a conflict listing, which consists of one
// message per conflict or problem followed by a message with Done set. Hops
// that are unable to reach the daemon send a status instead, which decodes
// compatibly.
type conflictsMessage struct {
	// Error is the flush or routing error, if any, which ends the listing.
	Error string `json:",omitempty"`
	// Conflict is the listed conflict, if any.
	Conflict *ConflictRoot `json:",omitempty"`
	// Problem is the listed problem, if any.
	Problem *Problem `json:",omitempty"`
	// Done indicates the end of a complete listing.
	Done bool `json:",omitempty"`
}

// RespondWithConflicts sends the outcome of a conflicts submission and closes
// the connection. Conflicts and problems are sent in separate messages, so the
// listing isn't bounded by the message size limit. It returns any error
// encountered sending the response.
func RespondWithConflicts(connection io.ReadWriteCloser, listing *Listing, err error) error {
	defer connection.Close()
	if err != nil {
		return writeMessage(connection, &conflictsMessage{Error: err.Error()})
	}
	writer := bufio.NewWriter(connection)
	for c := range listing.Conflicts {
		if err := writeMessage(writer, &conflictsMessage{Conflict: &listing.Conflicts[c]}); err != nil {
			return err
		}
	}
	for p := range listing.Problems {
		if err := writeMessage(writer, &conflictsMessage{Problem: &listing.Problems[p]}); err != nil {
			return err
		}
	}
	if err := writeMessage(writer, &conflictsMessage{Done: true}); err != nil {
		return err
	}
	return writer.Flush()
}

// ListConflicts waits for the synchronization cycle requested by a conflicts
// submission (see Dial) to complete and returns the session's conflicts and
// problems. It owns and closes the connection.
func ListConflicts(ctx context.Context, connection io.ReadWriteCloser) (*Listing, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	reader := bufio.NewReader(connection)
	listing := &Listing{}
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
			return listing, nil
		} else if message.Conflict != nil {
			listing.Conflicts = append(listing.Conflicts, *message.Conflict)
		} else if message.Problem != nil {
			listing.Problems = append(listing.Problems, *message.Problem)
		} else {
			return nil, errors.New("invalid conflict listing")
		}
	}
}

// Replacement describes a conditional replacement or deletion of an entry
// (see ReplaceFile).
type Replacement struct {
	// Path is the slash-separated path of the entry relative to the root, or a
	// dot for the root itself.
	Path string
	// Expected lists the entries that the replacement may supersede, as
	// described by their snapshots (see Snapshot). Resolutions writing a result
	// list both checked-out copies, since synchronization may copy either one
	// to the other endpoint before both are replaced. Entries other than
	// regular files may only be deleted.
	Expected []*Entry
	// Result is the replacement regular file, or a missing entry to delete the
	// entry. Its data, if any, is ignored.
	Result *Entry
}

// ErrStale indicates that a replacement would overwrite unseen changes.
var ErrStale = errors.New("the copy changed since it was checked out")

// ErrLeft indicates that a replacement failed and left an entry at a temporary
// name next to its path, which synchronization ignores, such as a copy that
// couldn't be moved back or a file saved while it was being replaced. Its
// errors say where the entry is. They don't match ErrStale, even if a stale
// copy caused the failure, since the entry may be the only copy of content
// saved meanwhile, which callers mustn't consider superseded.
var ErrLeft = errors.New("an entry was left at a temporary name")

// staleError is a stale replacement reported by the opposite endpoint, which
// matches ErrStale.
type staleError struct {
	message string
}

func (e *staleError) Error() string {
	return e.message
}

func (e *staleError) Is(target error) bool {
	return target == ErrStale
}

// leftError is a failed replacement that left an entry at a temporary name,
// which matches ErrLeft. The error that caused the failure is only described,
// so that a stale copy causing it doesn't make it match ErrStale.
type leftError struct {
	message string
}

func (e *leftError) Error() string {
	return e.message
}

func (e *leftError) Is(target error) bool {
	return target == ErrLeft
}

// maximumSupersedeAttempts bounds the attempts to supersede an entry that's
// replaced concurrently by one that the replacement may also supersede, such
// as the other checked-out copy propagated by synchronization.
const maximumSupersedeAttempts = 3

// Roles of the temporary files used by replacements (see temporaryName).
const (
	// roleStaged names a replacement's staged result, which holds the
	// superseded entry after they're exchanged.
	roleStaged = "staged"
	// roleAside names a superseded entry that's been moved aside.
	roleAside = "aside"
	// roleProbe and rolePlaced name the probes used by placeable.
	roleProbe, rolePlaced = "probe", "placed"
)

// temporaryName computes the name of a replacement's temporary file with a
// role next to a path, which synchronization ignores. Names are derived from
// the path's name, so that files left by an interrupted replacement are found
// by later ones (see cleanUp).
func temporaryName(path, role string) string {
	digest := sha256.Sum256([]byte(filepath.Base(path)))
	return filepath.Join(filepath.Dir(path), fmt.Sprintf("%sconflict-%s-%x", filesystem.TemporaryNamePrefix, role, digest[:8]))
}

// ensureValid checks that a replacement is well-formed.
func (r *Replacement) ensureValid() error {
	result := r.Result
	if len(r.Expected) == 0 || result == nil || (result.Kind != EntryFile && result.Kind != EntryMissing) || result.Size < 0 {
		return errors.New("invalid replacement")
	}
	for _, expected := range r.Expected {
		if expected == nil || (expected.Kind != EntryFile && (result.Kind != EntryMissing ||
			(expected.Kind != EntryDirectory && expected.Kind != EntrySymbolicLink && expected.Kind != EntryOther) ||
			(expected.Kind == EntryDirectory && expected.Digest == ""))) {
			return errors.New("invalid replacement")
		}
	}
	return nil
}

// supersedes determines whether a replacement may supersede an entry: one of
// its expected entries, or a file with the result's contents but different
// executability. Executability isn't otherwise compared, since filesystems that
// don't preserve it (such as those on Windows) report it regardless of how a
// file was written or synchronized, and retries would fail indefinitely.
func (r *Replacement) supersedes(current *Entry) bool {
	if current.Kind == EntryMissing {
		return false
	} else if r.Result.Kind == EntryFile && current.Kind == EntryFile && SameContents(current, r.Result) {
		return true
	}
	return slices.ContainsFunc(r.Expected, func(expected *Entry) bool {
		return SameContents(current, expected)
	})
}

// executabilityPreserved indicates whether replacements can set executability.
// Files on Windows are never read as executable, so a file with the result's
// contents is all that a replacement can achieve there.
const executabilityPreserved = runtime.GOOS != "windows"

// check reads the entry that a replacement replaces and determines whether the
// replacement is complete or may supersede it, in which case it returns the
// entry as read. It fails with ErrStale if neither is the case.
func (r *Replacement) check(ctx context.Context, root *os.Root, path string) (bool, *Entry, error) {
	// Entries of kinds that the replacement can't supersede are stale without
	// reading them, which may be expensive for directories.
	if info, err := root.Lstat(path); err == nil && !slices.ContainsFunc(r.Expected, func(expected *Entry) bool {
		return expected.Kind == entryKind(info.Mode())
	}) {
		return false, nil, ErrStale
	}
	current, err := snapshot(ctx, root, path, nil)
	if err != nil {
		return false, nil, err
	} else if SameFile(current, r.Result) || (!executabilityPreserved && SameContents(current, r.Result)) {
		return true, nil, nil
	} else if !r.supersedes(current) {
		return false, nil, ErrStale
	}
	return false, current, nil
}

// entryKind returns the entry kind corresponding to a file mode.
func entryKind(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return EntryFile
	case mode.IsDir():
		return EntryDirectory
	case mode&fs.ModeSymlink != 0:
		return EntrySymbolicLink
	}
	return EntryOther
}

// CheckFile determines whether a replacement (see ReplaceFile) may replace the
// entry that it describes or would leave it as is, failing with ErrStale if the
// entry changed since it was checked out. Like a replacement, it removes files
// left by interrupted replacements, and fails if it can't. Resolutions check
// one copy before replacing the other, so that a copy changed beforehand, or a
// leftover file, doesn't leave only one replaced.
func CheckFile(ctx context.Context, directory string, replacement *Replacement) error {
	if err := replacement.ensureValid(); err != nil {
		return err
	}
	root, path, _, err := openFile(directory, replacement.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := replacement.cleanUp(ctx, root, path); err != nil {
		return err
	}
	_, _, err = replacement.check(ctx, root, path)
	return err
}

// cleanUp recovers files left at a path's temporary names by interrupted
// replacements. If a file replacement left the path missing, its moved-aside
// entry is restored before any staged result is removed, so that the
// replacement can be retried. Staged and moved-aside entries are removed only
// if they're the replacement's own, holding its result or an entry that it
// may supersede, since others may be the only remaining copy of content saved
// while an entry was being replaced. Those fail the replacement, naming their
// location.
func (r *Replacement) cleanUp(ctx context.Context, root *os.Root, path string) error {
	for _, role := range []string{roleProbe, rolePlaced} {
		name := temporaryName(path, role)
		if info, err := root.Lstat(name); err == nil && (info.IsDir() || (info.Mode().IsRegular() && info.Size() == 0)) {
			// Probes are empty, so removing a directory fails if it isn't one.
			root.Remove(name)
		}
	}
	for _, role := range []string{roleAside, roleStaged} {
		name := temporaryName(path, role)
		left, err := snapshot(ctx, root, name, nil)
		if err != nil {
			return fmt.Errorf("unable to read %s, left by an interrupted replacement: %w", filepath.ToSlash(name), err)
		} else if left.Kind == EntryMissing {
			continue
		} else if !SameContents(left, r.Result) && !r.supersedes(left) {
			return fmt.Errorf("%s was left by an interrupted replacement, but isn't a copy that it would replace; move it back to %s or remove it",
				filepath.ToSlash(name), filepath.ToSlash(path))
		}
		if role == roleAside && r.Result.Kind == EntryFile {
			if _, err := root.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				// Restore without replacing a copy saved at the path meanwhile.
				// Keep both leftovers if restoration fails.
				if err := place(root, name, path); err != nil {
					return fmt.Errorf("unable to restore %s, left by an interrupted replacement: %w", filepath.ToSlash(name), err)
				}
				continue
			} else if err != nil {
				return fmt.Errorf("unable to check %s before recovering an interrupted replacement: %w", filepath.ToSlash(path), err)
			}
		}
		if err := removeEntry(root, name, left.Kind); err != nil {
			return fmt.Errorf("unable to remove %s, left by an interrupted replacement: %w", filepath.ToSlash(name), err)
		}
	}
	return nil
}

// removeEntry removes an entry of the specified kind, including a directory's
// contents.
func removeEntry(root *os.Root, path, kind string) error {
	if kind == EntryDirectory {
		return root.RemoveAll(path)
	}
	return root.Remove(path)
}

// SameContents determines whether two entries describe the same contents:
// regular files with the same contents, directories with the same snapshot
// digest, symbolic links with the same target, or both missing. Other entries
// have no contents.
func SameContents(a, b *Entry) bool {
	if a == nil || b == nil || a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case EntryFile:
		return a.Size == b.Size && a.Digest == b.Digest
	case EntryDirectory:
		return a.Digest != "" && a.Digest == b.Digest
	case EntrySymbolicLink:
		return a.Target == b.Target
	}
	return true
}

// SameFile determines whether two entries describe the same regular file
// contents and executability, or are both missing.
func SameFile(a, b *Entry) bool {
	return SameContents(a, b) && (a.Kind == EntryMissing || (a.Kind == EntryFile && a.Executable == b.Executable))
}

// unchanged determines whether two observations of a path describe the same
// file, with the same size, modification time, and mode.
func unchanged(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) &&
		a.Mode() == b.Mode()
}

// openFile opens a root for operations on an entry at a slash-separated path
// beneath it, returning the corresponding native relative path. Symbolic links
// and other non-directories are refused as ancestors, while os.Root confines
// resolution if ancestors are replaced concurrently. An empty path or a dot
// refers to the root itself, which is operated on in its parent directory, as
// indicated by the returned flag.
func openFile(directory, path string) (*os.Root, string, bool, error) {
	if path == "" || path == "." {
		root, err := os.OpenRoot(filepath.Dir(directory))
		if err != nil {
			return nil, "", false, err
		}
		return root, filepath.Base(directory), true, nil
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, "", false, err
	}
	native := filepath.FromSlash(path)
	for parent := filepath.Dir(native); parent != "."; parent = filepath.Dir(parent) {
		if info, err := root.Lstat(parent); err != nil {
			root.Close()
			return nil, "", false, err
		} else if !info.IsDir() {
			root.Close()
			return nil, "", false, fmt.Errorf("%s is not a directory", filepath.ToSlash(parent))
		}
	}
	return root, native, false, nil
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
// path relative to a root (see openFile) to output and returns its entry,
// without retaining data. A missing file yields a missing entry.
func CopyFile(ctx context.Context, directory, path string, output io.Writer) (*Entry, error) {
	root, path, _, err := openFile(directory, path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entry, _, err := copyFile(ctx, root, path, output)
	return entry, err
}

// place moves an entry to a path in the same directory at which nothing
// exists. It fails with an error matching fs.ErrExist rather than replace an
// entry created there concurrently, which linking never does. Directories
// can't be linked, and not every filesystem supports hard links, so it
// otherwise renames the entry if that's possible without replacing entries.
// Checking that the path is free and then renaming would leave a window in
// which a concurrently saved file is replaced, so it fails with an error
// matching errors.ErrUnsupported instead.
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
	return fmt.Errorf("%w: the filesystem supports neither hard links nor renaming without replacing files, which replacing and deleting copies safely requires (%v)",
		errors.ErrUnsupported, errors.Unwrap(err))
}

// placeable checks that entries of a kind can be placed in the directory
// containing a path (see place), using a temporary entry.
func placeable(root *os.Root, path string, directory bool) error {
	probe, placed := temporaryName(path, roleProbe), temporaryName(path, rolePlaced)
	if directory {
		if err := root.Mkdir(probe, 0700); err != nil {
			return err
		}
	} else if file, err := root.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err != nil {
		return err
	} else if err := file.Close(); err != nil {
		root.Remove(probe)
		return err
	}
	defer root.Remove(probe)
	if err := place(root, probe, placed); err != nil {
		return err
	}
	return root.Remove(placed)
}

// exchangeFiles exchanges files (see exchange). Tests replace it to exercise
// filesystems that don't support exchanges.
var exchangeFiles = exchange

// stagedFile is a replacement's staged result. While it's exchanged into the
// path, it may be written in place (such as by an editor that saves without
// renaming), so once it's exchanged back, it's only reused or removed if it
// still holds nothing but the result (see verifyExchanged), and it's otherwise
// kept.
type stagedFile struct {
	// name is the file's name.
	name string
	// info identifies the file.
	info os.FileInfo
	// result describes the file's contents, which are the replacement's result.
	result *Entry
	// kept indicates that the file may hold content saved while it was at the
	// path, so it mustn't be removed or reused.
	kept bool
}

// intact determines whether the staged file's name still refers to the staged
// file and the file still holds nothing but the result, which identifying the
// file doesn't establish.
func (s *stagedFile) intact(ctx context.Context, root *os.Root) (bool, error) {
	if info, err := root.Lstat(s.name); err != nil || !os.SameFile(info, s.info) {
		return false, err
	}
	current, err := snapshot(ctx, root, s.name, nil)
	if err != nil {
		return false, err
	}
	return SameContents(current, s.result), nil
}

// remove removes the staged file, unless it's kept or its name refers to
// another file, such as one exchanged with it (see verifyExchanged).
func (s *stagedFile) remove(root *os.Root) {
	if s.kept {
		return
	} else if info, err := root.Lstat(s.name); err == nil && os.SameFile(info, s.info) {
		root.Remove(s.name)
	}
}

// supersede replaces the entry at a path, which must have been checked and
// found to be the specified one, with a staged file, or deletes it if staged is
// nil. Checking the entry and then replacing it would leave a window in which a
// concurrently saved file is overwritten, so the entry is first captured
// atomically and only removed once it's verified by reading it again, which
// also detects changes that its information doesn't reveal. Otherwise, it's
// restored, without overwriting an entry saved meanwhile.
//
// Where supported, files are exchanged with the staged file, so the path is
// never missing; synchronization observes nothing but the result, which
// replacing a root requires. Otherwise, and for deletions, the entry is moved
// aside, which requires that entries can be placed (see place). The path is
// thus missing until the staged file is placed, which synchronization may
// observe like any other intermediate state of a resolution, and in which it
// may place another entry; that entry is then checked and superseded in turn
// (see replace). Writes through a file opened before it was captured can't be
// detected once it's verified, just as they can't when renaming over it.
func supersede(ctx context.Context, root *os.Root, path string, checked *Entry, staged *stagedFile, isRoot bool) error {
	if staged != nil {
		if err := exchangeFiles(root, staged.name, path); err == nil {
			return verifyExchanged(ctx, root, path, checked, staged)
		} else if errors.Is(err, fs.ErrNotExist) {
			return ErrStale
		} else if !errors.Is(err, errors.ErrUnsupported) {
			return err
		} else if isRoot {
			return fmt.Errorf("%w: replacing a synchronization root that is a file requires a filesystem that can exchange files atomically",
				errors.ErrUnsupported)
		}
	}
	if err := placeable(root, path, checked.Kind == EntryDirectory); err != nil {
		return err
	}
	aside := temporaryName(path, roleAside)
	err := renameNoReplace(root, path, aside)
	if errors.Is(err, errors.ErrUnsupported) {
		// Leftovers were removed, so nothing should exist at the name.
		err = root.Rename(path, aside)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return ErrStale
	} else if err != nil {
		return err
	}
	captured, err := snapshot(ctx, root, aside, nil)
	if err != nil {
		return restore(root, aside, path, err)
	} else if !SameContents(captured, checked) {
		return restore(root, aside, path, ErrStale)
	} else if staged != nil {
		if err := place(root, staged.name, path); errors.Is(err, fs.ErrExist) {
			// An entry saved at the path after the checked one was moved aside
			// supersedes it, just as it would have if saved after the
			// replacement, and the checked one was verified.
			removeEntry(root, aside, captured.Kind)
			return ErrStale
		} else if err != nil {
			return restore(root, aside, path, err)
		}
	}
	return removeEntry(root, aside, captured.Kind)
}

// verifyExchanged completes a replacement whose staged file was exchanged with
// the checked file, which now has the staged file's name, by verifying and
// removing the checked file. If it changed, it's exchanged back, and the staged
// file is left for the caller to reuse or remove once it's verified to be
// intact: while it was at the path, it may have been written in place, or
// another file may have been saved over it, either of which holds content
// saved meanwhile. Such a file is kept at the staged file's name, and the
// replacement fails with an error matching ErrLeft that says where it is.
func verifyExchanged(ctx context.Context, root *os.Root, path string, checked *Entry, staged *stagedFile) error {
	captured, err := snapshot(ctx, root, staged.name, nil)
	if err == nil && SameContents(captured, checked) {
		return removeEntry(root, staged.name, captured.Kind)
	} else if err == nil {
		err = ErrStale
	}
	if exchangeErr := exchangeFiles(root, staged.name, path); exchangeErr != nil {
		return &leftError{fmt.Sprintf("%v; the copy was moved to %s and couldn't be moved back: %v",
			err, filepath.ToSlash(staged.name), exchangeErr)}
	}
	// The staged file's name now refers to whatever was at the path, which is
	// the staged file unless another file was saved there meanwhile.
	if intact, verifyErr := staged.intact(ctx, root); verifyErr != nil {
		staged.kept = true
		return &leftError{fmt.Sprintf("%v; a file that may have been saved while it was being replaced was moved to %s and couldn't be verified: %v",
			err, filepath.ToSlash(staged.name), verifyErr)}
	} else if !intact {
		staged.kept = true
		return &leftError{fmt.Sprintf("%v; a file saved while it was being replaced was moved to %s",
			err, filepath.ToSlash(staged.name))}
	}
	return err
}

// restore moves an entry that was moved aside back to its path and returns the
// error that prevented its replacement. If an entry was saved at the path
// meanwhile, it's left in place, and the replacement fails with an error
// matching ErrLeft that says where the other one is.
func restore(root *os.Root, aside, path string, err error) error {
	if restoreErr := place(root, aside, path); restoreErr != nil {
		return &leftError{fmt.Sprintf("%v; the copy was moved aside to %s and couldn't be moved back: %v",
			err, filepath.ToSlash(aside), restoreErr)}
	}
	return err
}

// replace supersedes the checked entry at a path with a staged file, or
// deletes it if staged is nil (see supersede). Synchronization may propagate
// the result or another expected entry meanwhile, which fails the replacement
// as stale, so the entry is checked again: the replacement is complete if it
// has the result, and is attempted again if the new entry may be superseded.
// Failures that leave entries elsewhere are reported regardless, since they
// fail with ErrLeft rather than ErrStale, which also keeps a staged file that
// may hold content saved meanwhile from being reused.
func (r *Replacement) replace(ctx context.Context, root *os.Root, path string, checked *Entry, staged *stagedFile, isRoot bool) error {
	for attempt := 1; ; attempt++ {
		err := supersede(ctx, root, path, checked, staged, isRoot)
		if err != ErrStale {
			return err
		}
		done, current, checkErr := r.check(ctx, root, path)
		if checkErr == nil && done {
			return nil
		} else if checkErr != nil || attempt == maximumSupersedeAttempts {
			return err
		}
		checked = current
	}
}

// ReplaceFile replaces the entry described by a replacement with contents read
// from input, or deletes it, if the entry still matches one of its expected
// entries. Replacement is idempotent: an entry that already matches the result,
// including one written concurrently, is left as is (and input isn't
// necessarily read). New contents are staged and verified next to the entry and
// then take its place (see supersede), keeping its permissions apart from
// executability and, as far as permitted, its owner and group (see
// preserveOwnership). Files left by interrupted replacements are removed first
// (see cleanUp). The root itself can be replaced if it's a file, but not deleted.
// Stale replacements fail with ErrStale, and those that leave an entry at a
// temporary name, such as a file saved while the entry was being replaced,
// fail with an error matching ErrLeft instead.
func ReplaceFile(ctx context.Context, directory string, replacement *Replacement, input io.Reader) error {
	if err := replacement.ensureValid(); err != nil {
		return err
	}
	root, path, isRoot, err := openFile(directory, replacement.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	result := replacement.Result
	if isRoot && result.Kind == EntryMissing {
		return errors.New("the synchronization root can't be deleted")
	} else if err := replacement.cleanUp(ctx, root, path); err != nil {
		return err
	}
	done, checked, err := replacement.check(ctx, root, path)
	if err != nil || done {
		return err
	} else if result.Kind == EntryMissing {
		return replacement.replace(ctx, root, path, checked, nil, isRoot)
	}

	// Stage the new contents.
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm() &^ 0111
	if result.Executable {
		mode |= (mode & 0444) >> 2
	}
	staged := &stagedFile{name: temporaryName(path, roleStaged), result: result}
	file, err := root.OpenFile(staged.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hasher), &entryReader{ctx: ctx, reader: io.LimitReader(input, result.Size+1)})
	if err == nil && (size != result.Size || fmt.Sprintf("%x", hasher.Sum(nil)) != result.Digest) {
		err = errors.New("replacement contents are incomplete or corrupt")
	}
	// Ownership is changed before permissions, since changing it may clear
	// permission bits.
	if err == nil {
		err = preserveOwnership(file, info)
	}
	if err == nil {
		err = file.Chmod(mode)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		staged.info, err = file.Stat()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		root.Remove(staged.name)
		return err
	}
	defer staged.remove(root)

	// Staging may take a while, during which synchronization may also have
	// propagated the result, so check the entry again immediately before
	// replacing it.
	if done, checked, err = replacement.check(ctx, root, path); err != nil || done {
		return err
	}
	return replacement.replace(ctx, root, path, checked, staged, isRoot)
}

// requestPath converts a slash-separated path relative to the root for file
// requests, which refer to the root itself with a dot.
func requestPath(path string) string {
	if path == "" {
		return "."
	}
	return path
}

// responseError converts an error reported by the executor, preserving whether
// a replacement was stale or left an entry at a temporary name.
func responseError(response *result) error {
	if response.Stale {
		return &staleError{response.Error}
	} else if response.Left {
		return &leftError{response.Error}
	}
	return errors.New(response.Error)
}

// Copy requests the complete contents of a regular file at a slash-separated
// path, relative to the opposite endpoint's synchronization root (empty for
// the root itself), over an established route (see Dial) and writes them to
// output. It owns and closes the connection. A missing file yields a missing
// entry.
func Copy(ctx context.Context, connection io.ReadWriteCloser, path string, output io.Writer) (*Entry, error) {
	response, err := execute(ctx, connection, &Request{Copy: requestPath(path)}, strings.NewReader(""), output, io.Discard)
	if err != nil {
		return nil, err
	} else if response.Error != "" {
		return nil, responseError(response)
	} else if response.Entry == nil {
		return nil, errors.New("no entry received")
	}
	return response.Entry, nil
}

// Archive requests an archive of the entry at a slash-separated path, relative
// to the opposite endpoint's synchronization root, over an established route
// (see Dial), writes it to output, and returns the entry's snapshot (see
// ArchiveEntry). It owns and closes the connection.
func Archive(ctx context.Context, connection io.ReadWriteCloser, path string, output io.Writer) (*Entry, error) {
	response, err := execute(ctx, connection, &Request{Archive: path}, strings.NewReader(""), output, io.Discard)
	if err != nil {
		return nil, err
	} else if response.Error != "" {
		return nil, responseError(response)
	} else if response.Entry == nil {
		return nil, errors.New("no entry received")
	}
	return response.Entry, nil
}

// Replace performs a replacement (see ReplaceFile) on the opposite endpoint
// over an established route (see Dial), sending input as the new contents. It
// owns and closes the connection. Stale replacements fail with an error
// matching ErrStale, and those that left an entry at a temporary name with one
// matching ErrLeft.
func Replace(ctx context.Context, connection io.ReadWriteCloser, replacement *Replacement, input io.Reader) error {
	replacement = &Replacement{Path: requestPath(replacement.Path), Expected: replacement.Expected, Result: replacement.Result}
	response, err := execute(ctx, connection, &Request{Replace: replacement}, input, io.Discard, io.Discard)
	if err != nil {
		return err
	} else if response.Error != "" {
		return responseError(response)
	}
	return nil
}
