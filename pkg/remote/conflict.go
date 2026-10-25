package remote

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
)

// ConflictRoot describes a conflict listed by ListConflicts.
type ConflictRoot struct {
	// Path is the slash-separated conflict root relative to the
	// synchronization root, which is empty for the root itself.
	Path string
	// AlphaChanges and BetaChanges describe the actual changes on each
	// endpoint, bounded to fit the conflict's listing message.
	AlphaChanges []Change
	BetaChanges  []Change
	// ExcludedChanges counts changes omitted from this conflict's summary.
	ExcludedChanges uint64 `json:",omitempty"`
	// Files indicates whether both endpoints have a regular file at the
	// conflict root, in which case remote can write a result, such as a merge
	// of both copies, to both endpoints.
	Files bool `json:",omitempty"`
	// AlphaTakable and BetaTakable indicate whether remote can resolve the
	// conflict by taking alpha's or beta's copy unchanged, which it writes to
	// the other endpoint.
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

// Changes returns the conflict's change summary in the same form as a
// synchronization warning, retaining endpoint roles and omitted changes.
func (c *ConflictRoot) Changes() Conflict {
	return Conflict{Root: c.Path, Alpha: c.AlphaChanges, Beta: c.BetaChanges, ExcludedChanges: c.ExcludedChanges}
}

// boundConflict fits a conflict into its listing message without modifying
// the source. A directory conflict may contain thousands of changes. Halve
// the longest list first, preserving resolution metadata and counting every
// omitted change, including when a single change exceeds the message limit.
func boundConflict(conflict *ConflictRoot) (*ConflictRoot, error) {
	bounded := *conflict
	for {
		data, err := json.Marshal(&conflictsMessage{Conflict: &bounded})
		if err != nil {
			return nil, err
		} else if len(data) <= maximumMessageSize {
			return &bounded, nil
		}
		longest := &bounded.AlphaChanges
		if len(bounded.BetaChanges) > len(*longest) {
			longest = &bounded.BetaChanges
		}
		if len(*longest) == 0 {
			return nil, errors.New("conflict metadata exceeds size limit")
		}
		removed := (len(*longest) + 1) / 2
		bounded.ExcludedChanges += uint64(removed)
		*longest = (*longest)[:len(*longest)-removed]
	}
}

// Listing is a complete listing of a session's conflicts and problems as of a
// synchronization cycle, along with its checkouts.
type Listing struct {
	// Conflicts are the conflicts, sorted by root path.
	Conflicts []ConflictRoot
	// Problems are the problems that the endpoints encountered.
	Problems []Problem
	// Checkouts are the session's checkouts, sorted by path, which may include
	// paths that are no longer conflict roots, such as those of unfinished
	// resolutions.
	Checkouts []Checkout
}

// conflictsMessage is a message of a conflict listing, which consists of one
// message per conflict or problem followed by a message with Done set. Hops
// that are unable to reach the daemon send a status instead, which decodes
// compatibly. Progress messages precede the listing if the submission
// requested them (see progressMessage).
type conflictsMessage struct {
	// Error is the flush or routing error, if any, which ends the listing.
	Error string `json:",omitempty"`
	// ErrorKind classifies Error independently of its diagnostic text.
	ErrorKind ErrorKind `json:",omitempty"`
	// Conflict is the listed conflict, if any.
	Conflict *ConflictRoot `json:",omitempty"`
	// Problem is the listed problem, if any.
	Problem *Problem `json:",omitempty"`
	// Checkout is the listed checkout, if any.
	Checkout *Checkout `json:",omitempty"`
	// Done indicates the end of a complete listing.
	Done bool `json:",omitempty"`
	// Progress is set, and nothing else, by progress messages.
	Progress *Progress `json:",omitempty"`
}

// RespondWithConflicts sends the outcome of a conflicts submission and closes
// the connection. Conflicts, problems, and checkouts are sent in separate
// messages, so the listing isn't bounded by the message size limit. It returns
// any error encountered sending the response.
func RespondWithConflicts(connection io.ReadWriteCloser, listing *Listing, err error) error {
	defer connection.Close()
	if err != nil {
		return writeMessage(connection, &conflictsMessage{Error: err.Error(), ErrorKind: ClassifyError(err)})
	}
	writer := bufio.NewWriter(connection)
	for c := range listing.Conflicts {
		conflict, err := boundConflict(&listing.Conflicts[c])
		if err != nil {
			if err := writeMessage(writer, &conflictsMessage{Error: err.Error(), ErrorKind: ClassifyError(err)}); err != nil {
				return err
			}
			return writer.Flush()
		}
		if err := writeMessage(writer, &conflictsMessage{Conflict: conflict}); err != nil {
			return err
		}
	}
	for p := range listing.Problems {
		if err := writeMessage(writer, &conflictsMessage{Problem: &listing.Problems[p]}); err != nil {
			return err
		}
	}
	for c := range listing.Checkouts {
		if err := writeMessage(writer, &conflictsMessage{Checkout: &listing.Checkouts[c]}); err != nil {
			return err
		}
	}
	if err := writeMessage(writer, &conflictsMessage{Done: true}); err != nil {
		return err
	}
	return writer.Flush()
}

// ListConflicts waits for the synchronization cycle requested by a conflicts
// submission (see Dial) to complete and returns the session's conflicts,
// problems, and checkouts. It owns and closes the connection.
func ListConflicts(ctx context.Context, connection io.ReadWriteCloser) (*Listing, error) {
	return ListConflictsWithProgress(ctx, connection, nil)
}

// ListConflictsWithProgress is ListConflicts for a submission that may have
// requested progress messages (see Submission.Progress), which are passed to
// progress, if non-nil, as they arrive. Daemons and hops that predate progress
// messages send none.
func ListConflictsWithProgress(ctx context.Context, connection io.ReadWriteCloser, progress func(*Progress)) (*Listing, error) {
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
			return nil, ConnectionError(fmt.Errorf("unable to receive conflicts: %w", err))
		} else if message.Error != "" {
			return nil, receivedError(message.ErrorKind, errors.New(message.Error))
		} else if message.Done {
			return listing, nil
		} else if message.Conflict != nil {
			listing.Conflicts = append(listing.Conflicts, *message.Conflict)
		} else if message.Problem != nil {
			listing.Problems = append(listing.Problems, *message.Problem)
		} else if message.Checkout != nil {
			listing.Checkouts = append(listing.Checkouts, *message.Checkout)
		} else if message.Progress != nil {
			if progress != nil {
				progress(message.Progress)
			}
		} else {
			return nil, ConnectionError(errors.New("invalid conflict listing"))
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
	// described by their snapshots (see Snapshot). Resolutions list both
	// checked-out copies, which their workspaces keep, since synchronization
	// may copy either one to the other endpoint before both are replaced, and
	// the results that they applied before. Directories can't be superseded.
	Expected []*Entry
	// Result is the replacement regular file or symbolic link, or a missing
	// entry to delete the entry. Its data, if any, is ignored.
	Result *Entry
	// Sequence orders the replacement among the entry's replacements, which
	// are refused once a replacement with a later sequence number has been
	// admitted for the entry (see ReplaceFile). It's required. Resolutions use
	// the sequence number that the daemon issued to the claim that their apply
	// made of the conflict's checkout (see ClaimCheckout), which is later than
	// those of all earlier applies of the conflict, whether through the same
	// workspace, through one that replaced it, or through the opposite
	// endpoint.
	Sequence uint64 `json:",omitempty"`
}

// ErrStale indicates that a replacement would overwrite unseen changes.
var ErrStale = errors.New("the copy changed since it was checked out")

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

// ensureValid checks that a replacement is well-formed.
func (r *Replacement) ensureValid() error {
	result := r.Result
	if len(r.Expected) == 0 || r.Sequence == 0 || result == nil || result.Size < 0 ||
		(result.Kind != EntryFile && result.Kind != EntrySymbolicLink && result.Kind != EntryMissing) ||
		(result.Kind == EntrySymbolicLink && result.Target == "") {
		return errors.New("invalid replacement")
	}
	for _, expected := range r.Expected {
		if expected == nil || expected.Kind == EntryDirectory {
			return errors.New("invalid replacement")
		}
	}
	return nil
}

// executabilityPreserved indicates whether replacements can set executability.
// Files on Windows are never read as executable, so a file with the result's
// contents is all that a replacement can achieve there.
const executabilityPreserved = runtime.GOOS != "windows"

// check reads the entry that a replacement replaces and determines whether it
// already has the result, failing with ErrStale unless the replacement may
// supersede it: it's one of the expected entries, or a file with the result's
// contents but different executability. Executability isn't otherwise
// compared, since filesystems that don't preserve it (such as those on Windows)
// report it regardless of how a file was written or synchronized, and retries
// would fail indefinitely.
func (r *Replacement) check(ctx context.Context, root *os.Root, path string) (bool, error) {
	// Directories are stale without reading them, which may be expensive.
	if info, err := root.Lstat(path); err == nil && info.IsDir() {
		return false, ErrStale
	}
	current, err := snapshot(ctx, root, path, nil)
	if err != nil {
		return false, err
	} else if SameContents(current, r.Result) {
		done := current.Kind != EntryFile || !executabilityPreserved || current.Executable == r.Result.Executable
		return done, nil
	} else if !slices.ContainsFunc(r.Expected, func(expected *Entry) bool {
		return SameContents(current, expected)
	}) {
		return false, ErrStale
	}
	return false, nil
}

// CheckFile determines whether a replacement (see ReplaceFile) may replace the
// entry that it describes or would leave it as is, failing with ErrStale if the
// entry changed since it was checked out. Resolutions check the local copy
// before replacing the remote one, so that a local copy changed beforehand
// doesn't leave only the remote one replaced.
func CheckFile(ctx context.Context, directory string, replacement *Replacement) error {
	if err := replacement.ensureValid(); err != nil {
		return err
	}
	root, path, _, err := openFile(directory, replacement.Path)
	if err != nil {
		return err
	}
	defer root.Close()
	_, err = replacement.check(ctx, root, path)
	return err
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

// SyncDirectory flushes a directory to stable storage, so that the entries
// created, renamed, or removed in it survive a power loss or an operating
// system crash, which flushing files alone doesn't guarantee. Filesystems that
// don't support flushing directories are tolerated. It does nothing on
// Windows, where directory handles can't be flushed.
func SyncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return syncDirectory(os.Open(directory))
}

// syncParent flushes the directory containing an entry at a native path
// relative to a root (see SyncDirectory).
func syncParent(root *os.Root, path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return syncDirectory(root.Open(filepath.Dir(path)))
}

// syncDirectory flushes and closes an opened directory, returning any error
// encountered opening it (see SyncDirectory). Some filesystems fail to flush
// directories with EINVAL or an error indicating that it's unsupported.
func syncDirectory(directory *os.File, err error) error {
	if err != nil {
		return err
	}
	err = directory.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, errors.ErrUnsupported) {
		err = nil
	}
	return errors.Join(err, directory.Close())
}

// syncExisting flushes an entry that already has a replacement's result, as a
// replacement that writes it would (see ReplaceFile), since synchronization
// may have written it without flushing it: a regular file and the directory
// containing it. Files are only flushed where directories are (see
// SyncDirectory), since flushing a file on Windows requires write access,
// which an existing file might not grant.
func syncExisting(root *os.Root, path string, result *Entry) error {
	if runtime.GOOS == "windows" {
		return nil
	} else if result.Kind == EntryFile {
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		err = file.Sync()
		if err = errors.Join(err, file.Close()); err != nil {
			return err
		}
	}
	return syncParent(root, path)
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

// stageFile writes a file result, read from input, to a new file with the
// specified name and verifies its contents. The file gets the permissions of
// the regular file that it replaces, if any, apart from executability, and,
// as far as permitted, the owner and group of the entry that it replaces (see
// preserveOwnership).
func stageFile(ctx context.Context, root *os.Root, path, name string, result *Entry, input io.Reader) error {
	info, err := root.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	mode := fs.FileMode(0644)
	if info != nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm() &^ 0111
	}
	if result.Executable {
		mode |= (mode & 0444) >> 2
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
	if err == nil && info != nil {
		err = preserveOwnership(file, info)
	}
	if err == nil {
		err = file.Chmod(mode)
	}
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// ReplaceHook, if non-nil, is called by each replacement (see ReplaceFile)
// with the stage that it has reached, its root, and its path: "locking"
// immediately before it locks the entry, once it has staged its result, if
// needed, and "writing" once it has checked the entry for the last time,
// immediately before it replaces or deletes the entry. It exists so that tests
// can pause replacements to schedule them deterministically, and must be nil
// otherwise.
var ReplaceHook func(stage, directory, path string)

// ReplaceFile replaces the entry described by a replacement with its result,
// read from input for a file, or deletes it, if the entry is one of the
// replacement's expected entries, failing with ErrStale otherwise. An entry
// that already has the result, such as one that synchronization propagated, is
// left as is (and input isn't necessarily read). Results are staged next to
// the entry, under a temporary name that synchronization ignores, and renamed
// over it, so that the path holds either the entry or the complete result,
// even if the replacement is interrupted. The root itself can be replaced with
// a file, but not deleted. Once the entry has the result, whether written,
// deleted, or already present, the result and the directory containing it are
// flushed (see SyncDirectory), so that a replacement that succeeds survives a
// power loss or an operating system crash where directories can be flushed.
//
// Replacements of an entry are serialized and ordered by their sequence
// numbers, since a resolution's replacement may still be running after its
// client stopped waiting for it, such as on timeout, until the endpoint
// notices, while a retry or another resolution of the conflict replaces the
// entry. Checking the entry can't tell the earlier replacement apart from the
// later one, since replacements accept both checked-out copies, which the
// later one may have written. Once its result is staged, a replacement thus
// locks the entry (see lockReplacement), which excludes the entry's other
// replacements on this system, and fails with ErrSuperseded, without changing
// the entry, if a replacement with a later sequence number was admitted for
// it before. Otherwise, it's admitted: it records its sequence number (see
// replacementLock.admit), whatever its outcome, then checks the entry again,
// since it may have changed meanwhile, and replaces it and flushes the result
// before unlocking it, even if its context is cancelled meanwhile.
//
// Changes made by other means after that last check are replaced regardless:
// resolutions require that conflicting paths aren't changed while they're
// resolved, and only detect changes made before they apply a result.
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
	if isRoot && result.Kind != EntryFile {
		return errors.New("the synchronization root can only be replaced with a file")
	}

	// Stage a file or symbolic link result next to the entry before locking
	// it, since staging may take a while, unless the entry is stale or already
	// has the result. Stale replacements thus fail without reading their
	// input, but they're still admitted first, like those that find the result
	// present, so that no earlier replacement is admitted after them. Failures
	// to check the entry are left to the check after locking, since an earlier
	// replacement that's still running may be writing the entry meanwhile,
	// which makes reading it fail, until it unlocks the entry.
	var staged string
	defer func() {
		if staged != "" {
			root.Remove(staged)
		}
	}()
	stage := func() error {
		staged = filepath.Join(filepath.Dir(path), filesystem.TemporaryNamePrefix+"conflict-"+rand.Text())
		if result.Kind == EntrySymbolicLink {
			// Targets are slash-separated (see normalizeTarget), and Go
			// converts them to backslashes when creating links on Windows.
			return root.Symlink(result.Target, staged)
		}
		return stageFile(ctx, root, path, staged, result, input)
	}
	if done, err := replacement.check(ctx, root, path); err == nil && !done && result.Kind != EntryMissing {
		if err := stage(); err != nil {
			return err
		}
	}

	// Lock the entry and admit the replacement before checking the entry
	// again. An entry that was stale, had the result, or couldn't be checked
	// when it was first checked, but is now one of the expected entries, gets
	// its result staged now and is checked once more.
	if ReplaceHook != nil {
		ReplaceHook("locking", directory, replacement.Path)
	}
	lock, err := lockReplacement(ctx, directory, requestPath(replacement.Path))
	if err != nil {
		return err
	}
	defer lock.unlock()
	if err := lock.admit(directory, requestPath(replacement.Path), replacement.Sequence); err != nil {
		return err
	}
	done, err := replacement.check(ctx, root, path)
	if err == nil && !done && result.Kind != EntryMissing && staged == "" {
		if err = stage(); err == nil {
			done, err = replacement.check(ctx, root, path)
		}
	}
	if err != nil {
		return err
	} else if done {
		return syncExisting(root, path, result)
	}
	if ReplaceHook != nil {
		ReplaceHook("writing", directory, replacement.Path)
	}
	if result.Kind == EntryMissing {
		err = root.Remove(path)
	} else if err = root.Rename(staged, path); err == nil {
		staged = ""
	}
	if err != nil {
		return err
	}
	return syncParent(root, path)
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
// a replacement was stale or superseded.
func responseError(response *result) error {
	if response.Stale {
		return receivedError(response.ErrorKind, &staleError{response.Error})
	} else if response.Superseded {
		return receivedError(response.ErrorKind, &supersededError{response.Error})
	}
	return receivedError(response.ErrorKind, errors.New(response.Error))
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
		return nil, ConnectionError(errors.New("no entry received"))
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
		return nil, ConnectionError(errors.New("no entry received"))
	}
	return response.Entry, nil
}

// Replace performs a replacement (see ReplaceFile) on the opposite endpoint
// over an established route (see Dial), sending input as the new contents. It
// owns and closes the connection. Stale and superseded replacements, which
// don't change the entry, fail with errors matching ErrStale and ErrSuperseded.
// A replacement whose wait is cancelled may still change the entry.
func Replace(ctx context.Context, connection io.ReadWriteCloser, replacement *Replacement, input io.Reader) error {
	replacement = &Replacement{
		Path: requestPath(replacement.Path), Expected: replacement.Expected,
		Result: replacement.Result, Sequence: replacement.Sequence,
	}
	response, err := execute(ctx, connection, &Request{Replace: replacement}, input, io.Discard, io.Discard)
	if err != nil {
		return err
	} else if response.Error != "" {
		return responseError(response)
	}
	return nil
}
