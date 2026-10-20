package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/filesystem/locking"
	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

const (
	// workspacesDirectoryName is the name of the Mutagen data subdirectory
	// containing conflict workspaces.
	workspacesDirectoryName = "conflicts"
	// backupsDirectoryName is the name of the workspaces subdirectory that
	// keeps the workspaces of resolved and discarded conflicts.
	backupsDirectoryName = "backups"
	// backupRetention is how long the workspaces of resolved and discarded
	// conflicts are kept, so that the original copies can be recovered if a
	// resolution turns out to be wrong.
	backupRetention = 7 * 24 * time.Hour
	// stagingPrefix is the name prefix of the directories in which checkouts
	// prepare workspaces.
	stagingPrefix = ".checkout-"
	// stagingRetention is the age after which a staging directory is assumed
	// to have been left by an interrupted checkout.
	stagingRetention = 24 * time.Hour
	// metadataName, localName, remoteName, baseName, and mergedName are the
	// names of the files in a workspace.
	metadataName, localName, remoteName, baseName, mergedName = "workspace.json", "local", "remote", "base", "merged"
	// sideLocal and sideRemote identify the copies of a conflict.
	sideLocal, sideRemote = "local", "remote"
)

// errReported indicates that conflict operations failed for some paths, whose
// errors have been printed.
var errReported = errors.New("conflict operations failed")

// errDirectoryConflict indicates that a conflict requires manual resolution.
var errDirectoryConflict = errors.New("conflicts involving directories aren't supported; resolve this conflict manually")

// errResolvedOtherwise indicates that apply refused a workspace with nothing
// applied whose conflict is no longer listed (see workspace.applied).
var errResolvedOtherwise = errors.New("is no longer listed as a conflict, and nothing was applied, so it was resolved some other way; apply changed neither copy: check them, then discard the workspace")

// conflictOptions are the options for conflict resolution, which is separate
// from command execution and its flush policy.
type conflictOptions struct {
	// conflicts indicates that conflicts should be listed.
	conflicts bool
	// checkout, apply, and discard indicate the operation to perform on the
	// conflicting paths specified as arguments.
	checkout, apply, discard bool
	// take selects an original copy, local or remote, as the result.
	take string
	// delete selects deletion of both copies as the result.
	delete bool
	// executable explicitly specifies the result's executability.
	executable string
	// base is a candidate for the copies' common ancestor.
	base string
	// json requests JSON output.
	json bool
}

// active indicates whether a conflict operation was requested.
func (o *conflictOptions) active() bool {
	return o.conflicts || o.checkout || o.apply || o.discard
}

// register registers conflict options.
func (o *conflictOptions) register(flags *pflag.FlagSet) {
	flags.BoolVar(&o.conflicts, "conflicts", false, "Synchronize, then list all conflicts, how remote can resolve them, and unfinished resolutions")
	flags.BoolVar(&o.checkout, "conflict-checkout", false, "Download both copies of each conflicting path given as an argument into a workspace for resolution")
	flags.BoolVar(&o.apply, "conflict-apply", false, "Apply the resolution of each path given as an argument to both copies and verify it")
	flags.BoolVar(&o.discard, "conflict-discard", false, "Set aside the workspace of each path given as an argument without changing either copy")
	flags.StringVar(&o.take, "take", "", "With --conflict-apply, take the original `SIDE` (local or remote) instead of the merged file")
	flags.BoolVar(&o.delete, "delete", false, "With --conflict-apply, delete both copies instead")
	flags.StringVar(&o.executable, "executable", "", "With --conflict-apply, set whether the result is executable (`BOOL`: true or false)")
	flags.StringVar(&o.base, "base", "", "With --conflict-checkout of one path, merge using `FILE` as the copies' common ancestor, if it's verified to be")
	flags.BoolVar(&o.json, "json", false, "Print the results of conflict operations as JSON")
}

// validateConflicts checks the combination of conflict options.
func (o *options) validateConflicts() error {
	modes := 0
	for _, enabled := range []bool{o.conflicts, o.checkout, o.apply, o.discard} {
		if enabled {
			modes++
		}
	}
	paths := o.checkout || o.apply || o.discard
	if modes > 1 {
		return errors.New("--conflicts, --conflict-checkout, --conflict-apply, and --conflict-discard are mutually exclusive")
	} else if modes == 1 && (o.flush || o.noFlush || o.diff || o.directory != "" ||
		len(o.environment) != 0 || len(o.environmentFiles) != 0 || o.noPrologue) {
		return errors.New("conflict operations can't be combined with commands or command options")
	} else if o.conflicts && len(o.arguments) != 0 {
		return errors.New("--conflicts doesn't accept arguments")
	} else if paths && len(o.arguments) == 0 {
		return errors.New("no conflicting paths specified")
	} else if paths && slices.ContainsFunc(o.arguments, func(argument string) bool { return strings.HasPrefix(argument, "-") }) {
		return errors.New("options must precede paths (prefix paths starting with - with ./)")
	} else if o.json && modes == 0 {
		return errors.New("--json requires a conflict operation")
	} else if !o.apply && (o.take != "" || o.delete || o.executable != "") {
		return errors.New("--take, --delete, and --executable require --conflict-apply")
	} else if o.base != "" && (!o.checkout || len(o.arguments) != 1) {
		return errors.New("--base requires --conflict-checkout of a single path")
	} else if o.take != "" && o.take != sideLocal && o.take != sideRemote {
		return errors.New("--take must be local or remote")
	} else if o.delete && (o.take != "" || o.executable != "") {
		return errors.New("--delete can't be combined with --take or --executable")
	} else if o.executable != "" && o.executable != "true" && o.executable != "false" {
		return errors.New("--executable must be true or false")
	}
	return nil
}

// workspace is a checked-out conflict. Workspaces are stored in the Mutagen
// data directory on the submitting endpoint's system, outside of the
// synchronization root, and identified by the local path of the conflict root.
type workspace struct {
	// Session is the session identifier.
	Session string
	// Alpha indicates whether the local copy is alpha's.
	Alpha bool
	// Path is the slash-separated conflict root relative to the
	// synchronization root, which is empty for the root itself.
	Path string
	// Token labels conflict markers in the merged file.
	Token string
	// Local and Remote describe the checked-out copies (see remote.Snapshot).
	Local, Remote *remote.Entry
	// Take lists the copies that can be taken, for conflicts other than
	// between two files.
	Take []string `json:",omitempty"`
	// BaseSource describes where the copies' common ancestor in the
	// workspace was found, if it was.
	BaseSource string `json:",omitempty"`
	// Applied describes the results (files, symbolic links, or deletions) that
	// apply has written (or may have written) to either copy, which retries
	// may supersede, since they're the workspace's own, even if the merged
	// file has changed since.
	Applied []*remote.Entry `json:",omitempty"`
	// directory is the workspace directory.
	directory string
}

// file returns the path of a file in the workspace.
func (w *workspace) file(name string) string {
	return filepath.Join(w.directory, name)
}

// applied indicates whether apply has changed (or may have changed) either
// copy, which it records before doing so. The resolution is then unfinished
// until verified, even if its conflict is no longer listed, since
// synchronization may have propagated the remaining original copy over it.
// Otherwise, a conflict that's no longer listed was resolved some other way,
// such as by editing or deleting a copy, or by applying through another
// endpoint's workspace, and apply refuses to replace that resolution.
func (w *workspace) applied() bool {
	return len(w.Applied) > 0
}

// files indicates whether both copies are regular files, which can be merged.
func (w *workspace) files() bool {
	return w.Local.Kind == remote.EntryFile && w.Remote.Kind == remote.EntryFile
}

// hasDirectory indicates whether either checked-out copy is a directory.
func (w *workspace) hasDirectory() bool {
	return w.Local.Kind == remote.EntryDirectory || w.Remote.Kind == remote.EntryDirectory
}

// takable determines whether a copy can be taken.
func (w *workspace) takable(side string) bool {
	return w.files() || slices.Contains(w.Take, side)
}

// workspaceDirectory computes the workspaces directory and the directory of
// the workspace for a local path. The path's parent is resolved, so that
// differently spelled paths share a workspace.
func workspaceDirectory(local string) (string, string, error) {
	base, err := filesystem.Mutagen(true, workspacesDirectoryName)
	if err != nil {
		return "", "", fmt.Errorf("unable to create workspaces directory: %w", err)
	}
	parent := filepath.Dir(local)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	digest := sha256.Sum256([]byte(filepath.Join(parent, filepath.Base(local))))
	return base, filepath.Join(base, fmt.Sprintf("%x", digest[:16])), nil
}

// lockWorkspace locks the workspace for a local path, whether or not it exists,
// and returns a function that unlocks it. Operations on a workspace must not
// interleave: applies of different results would otherwise replace each
// other's results, since each accepts either checked-out copy, and the last to
// finish would remove the workspace. The lock file is kept beside the
// workspace, since one inside it wouldn't outlive the workspace's removal. It
// fails rather than wait for another operation to finish.
func lockWorkspace(local string) (func(), error) {
	_, directory, err := workspaceDirectory(local)
	if err != nil {
		return nil, err
	}
	locker, err := locking.NewLocker(directory+".lock", 0600)
	if err != nil {
		return nil, fmt.Errorf("unable to lock workspace: %w", err)
	} else if err := locker.Lock(false); err != nil {
		locker.Close()
		return nil, fmt.Errorf("another conflict operation on this path is in progress: %w", err)
	}
	return func() {
		locker.Unlock()
		locker.Close()
	}, nil
}

// save atomically writes the metadata of a workspace, then flushes the
// workspace directory (see remote.SyncDirectory), so that the metadata
// survives a power loss or an operating system crash, which apply relies on,
// since it records results before writing them. Checkouts save workspaces
// once their files are written and flushed, which this also makes durable.
func (w *workspace) save() error {
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(w.directory, ".workspace-")
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
	} else if err := os.Rename(file.Name(), w.file(metadataName)); err != nil {
		return err
	}
	return remote.SyncDirectory(w.directory)
}

// readWorkspace reads the workspace in a directory, failing with an error
// wrapping os.ErrNotExist if there's none.
func readWorkspace(directory string) (*workspace, error) {
	data, err := os.ReadFile(filepath.Join(directory, metadataName))
	if err != nil {
		return nil, err
	}
	w := &workspace{directory: directory}
	if err := json.Unmarshal(data, w); err != nil {
		return nil, fmt.Errorf("invalid workspace: %w", err)
	} else if w.Local == nil || w.Remote == nil {
		return nil, errors.New("invalid workspace: missing copies")
	} else if slices.ContainsFunc(w.Applied, func(applied *remote.Entry) bool {
		return applied == nil || (applied.Kind != remote.EntryFile &&
			applied.Kind != remote.EntrySymbolicLink && applied.Kind != remote.EntryMissing)
	}) {
		return nil, errors.New("invalid workspace: invalid applied result")
	}
	return w, nil
}

// loadWorkspace loads the workspace for a local path, failing with an error
// wrapping os.ErrNotExist if there's none.
func loadWorkspace(local string) (*workspace, error) {
	_, directory, err := workspaceDirectory(local)
	if err != nil {
		return nil, err
	}
	return readWorkspace(directory)
}

// matches determines whether a workspace was checked out through a target.
func (w *workspace) matches(target *remote.Target, path string) bool {
	return w.Session == target.Session && w.Alpha == target.Alpha && w.Path == path
}

// retire moves a workspace whose conflict was resolved or discarded into the
// backups directory, where it's kept for backupRetention (see
// pruneWorkspaces), and returns its new location. Backups are named by the
// time at which they were made, followed by the workspace's name and, if
// another backup of the workspace was made at the same time, a counter.
func (w *workspace) retire() (string, error) {
	base, err := filesystem.Mutagen(true, workspacesDirectoryName, backupsDirectoryName)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(w.directory))
	for attempt := 1; ; attempt++ {
		destination := filepath.Join(base, name)
		if attempt > 1 {
			destination += fmt.Sprintf("-%d", attempt)
		}
		// Backups aren't empty, so renaming fails rather than replace one.
		// The directories that the workspace left and entered are flushed
		// (see remote.SyncDirectory), so that the move survives a power loss
		// or an operating system crash.
		if err := os.Rename(w.directory, destination); err == nil {
			previous := filepath.Dir(w.directory)
			w.directory = destination
			if err := errors.Join(remote.SyncDirectory(base), remote.SyncDirectory(previous)); err != nil {
				return "", err
			}
			return destination, nil
		} else if _, statErr := os.Lstat(destination); statErr != nil || attempt == 10 {
			return "", err
		}
	}
}

// pruneWorkspaces removes retired workspaces older than backupRetention and
// staging directories left by interrupted checkouts. Failures are ignored,
// since pruning only reclaims space.
func pruneWorkspaces() {
	base, err := filesystem.Mutagen(false, workspacesDirectoryName)
	if err != nil {
		return
	}
	now := time.Now()
	backups := filepath.Join(base, backupsDirectoryName)
	if entries, err := os.ReadDir(backups); err == nil {
		for _, entry := range entries {
			prefix, _, _ := strings.Cut(entry.Name(), "-")
			if nanoseconds, err := strconv.ParseInt(prefix, 10, 64); err == nil && now.Sub(time.Unix(0, nanoseconds)) > backupRetention {
				os.RemoveAll(filepath.Join(backups, entry.Name()))
			}
		}
	}
	if entries, err := os.ReadDir(base); err == nil {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), stagingPrefix) {
				continue
			} else if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > stagingRetention {
				os.RemoveAll(filepath.Join(base, entry.Name()))
			}
		}
	}
}

// sessionWorkspaces lists the workspaces checked out through a target by
// their paths. Unreadable workspaces are skipped, since the listing is
// advisory.
func sessionWorkspaces(target *remote.Target) map[string]*workspace {
	workspaces := make(map[string]*workspace)
	base, err := filesystem.Mutagen(false, workspacesDirectoryName)
	if err != nil {
		return workspaces
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return workspaces
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == backupsDirectoryName || strings.HasPrefix(entry.Name(), stagingPrefix) {
			continue
		} else if w, err := readWorkspace(filepath.Join(base, entry.Name())); err == nil && w.Session == target.Session && w.Alpha == target.Alpha {
			workspaces[w.Path] = w
		}
	}
	return workspaces
}

// listConflicts synchronizes the session and lists all of its conflicts.
func listConflicts(ctx context.Context, directory, session string) (*remote.Listing, *remote.Target, error) {
	connection, target, err := remote.Dial(ctx, directory, session, remote.OperationConflicts)
	if err != nil {
		return nil, nil, err
	}
	listing, err := remote.ListConflicts(ctx, connection)
	return listing, target, err
}

// findConflict returns the listed conflict at a path, if any.
func findConflict(listing *remote.Listing, path string) *remote.ConflictRoot {
	for c := range listing.Conflicts {
		if listing.Conflicts[c].Path == path {
			return &listing.Conflicts[c]
		}
	}
	return nil
}

// dialOpposite opens a route to the opposite endpoint, which must be the one
// that the specified target identifies.
func dialOpposite(ctx context.Context, directory, session string, expected *remote.Target) (io.ReadWriteCloser, error) {
	connection, target, err := remote.Dial(ctx, directory, session, remote.OperationExecute)
	if err != nil {
		return nil, err
	} else if target.Session != expected.Session || target.Alpha != expected.Alpha {
		connection.Close()
		return nil, errors.New("the session was located differently while resolving the conflict")
	}
	return connection, nil
}

// outside verifies that a directory isn't inside a synchronization root, where
// its contents would be synchronized.
func outside(directory, root string) error {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(directory); err == nil {
		directory = resolved
	}
	if relative, err := filepath.Rel(root, directory); err == nil && !filepath.IsAbs(relative) && !escapes(relative) {
		return fmt.Errorf("conflict workspaces (%s) would be synchronized; set MUTAGEN_DATA_DIRECTORY to a directory outside the synchronization root", directory)
	}
	return nil
}

// copyLocalFile copies a local file's contents.
func copyLocalFile(destination, source string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	return writeLocalFile(destination, func(output io.Writer) error {
		_, err := io.Copy(output, input)
		return err
	})
}

// writeLocalFile creates a private file with contents written by a function.
func writeLocalFile(path string, write func(io.Writer) error) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = write(file)
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// digestWriter computes the size and digest of the data written to it.
type digestWriter struct {
	hash.Hash
	size int64
}

func (w *digestWriter) Write(data []byte) (int, error) {
	w.size += int64(len(data))
	return w.Hash.Write(data)
}

// errCorruptCopy indicates that a copy wasn't received as it was read.
var errCorruptCopy = errors.New("the copy was received incompletely or corrupted; check out again")

// writeCopy creates a private file with a copy written by a function that
// returns the copy's entry, as read at its source, and verifies that the data
// written has the entry's contents, so that merges aren't based on data
// corrupted in transit.
func writeCopy(path string, copy func(io.Writer) (*remote.Entry, error)) (*remote.Entry, error) {
	var entry *remote.Entry
	written := &digestWriter{Hash: sha256.New()}
	err := writeLocalFile(path, func(file io.Writer) (err error) {
		entry, err = copy(io.MultiWriter(file, written))
		return
	})
	if err != nil {
		return nil, err
	} else if entry.Kind == remote.EntryFile &&
		(written.size != entry.Size || fmt.Sprintf("%x", written.Sum(nil)) != entry.Digest) {
		return nil, errCorruptCopy
	}
	return entry, nil
}

// extractCopy extracts an archive of a copy, written by a function that
// returns the copy's snapshot as read at its source, to a path, and verifies
// that the archive describes the snapshot, so that resolutions aren't based on
// data corrupted in transit.
func extractCopy(ctx context.Context, path string, archive func(io.Writer) (*remote.Entry, error)) (*remote.Entry, error) {
	reader, writer := io.Pipe()
	type extraction struct {
		entry *remote.Entry
		err   error
	}
	extracted := make(chan extraction, 1)
	go func() {
		entry, err := remote.ExtractArchive(ctx, reader, path)
		if err != nil {
			reader.CloseWithError(err)
		} else {
			reader.Close()
		}
		extracted <- extraction{entry, err}
	}()
	entry, err := archive(writer)
	writer.CloseWithError(err)
	result := <-extracted
	if err != nil {
		return nil, err
	} else if result.err != nil {
		return nil, fmt.Errorf("unable to extract the copy: %w", result.err)
	} else if !remote.SameContents(entry, result.entry) || entry.Executable != result.entry.Executable {
		return nil, errCorruptCopy
	}
	return entry, nil
}

// outcome is the outcome of a conflict operation on a path. Its exported
// fields are printed as JSON, and its lines as text.
type outcome struct {
	// Path is the path as specified.
	Path string
	// Error describes why the operation failed, if it did.
	Error string `json:",omitempty"`
	// Local, Remote, Base, and Merged are the paths of a workspace's copies,
	// the copies' common ancestor (if found), and the merged file.
	Local  string `json:",omitempty"`
	Remote string `json:",omitempty"`
	Base   string `json:",omitempty"`
	Merged string `json:",omitempty"`
	// Files indicates whether both copies are files, which can be merged.
	Files bool `json:",omitempty"`
	// Binary indicates that the copies are binary files, for which no merged
	// file was created.
	Binary bool `json:",omitempty"`
	// Regions is the number of conflicting regions marked in the merged file.
	Regions int `json:",omitempty"`
	// Take lists the copies that apply can take.
	Take []string `json:",omitempty"`
	// Pending indicates that a workspace's conflict is no longer listed.
	Pending bool `json:",omitempty"`
	// Applied indicates that apply has changed (or may have changed) either
	// copy through the workspace, so that its resolution is unfinished until
	// verified. A pending workspace with nothing applied was resolved some
	// other way, and apply refuses it.
	Applied bool `json:",omitempty"`
	// Resolved indicates that apply resolved the conflict.
	Resolved bool `json:",omitempty"`
	// Discarded indicates that discard set the workspace aside.
	Discarded bool `json:",omitempty"`
	// Backup is the location where a resolved or discarded workspace is kept.
	Backup string `json:",omitempty"`
	// lines are the lines printed as text.
	lines []string
}

// printer prints the outcomes of conflict operations on paths, as text or as
// one line of JSON per path, and prints failures as errors.
type printer struct {
	// output is the output for outcomes.
	output io.Writer
	// errors is the output for failures.
	errors io.Writer
	// json indicates whether outcomes are printed as JSON.
	json bool
	// headings indicates whether text outcomes are preceded by their paths,
	// which distinguishes them when operating on several paths.
	headings bool
	// failed records whether any operation failed.
	failed bool
}

// print prints the outcome of an operation on a path.
func (p *printer) print(path string, result *outcome, err error) {
	if err != nil {
		p.failed = true
		fmt.Fprintf(p.errors, "remote: %s: %v\n", terminal.NeutralizeControlCharacters(path), err)
		result = &outcome{Path: path, Error: err.Error()}
		if !p.json {
			return
		}
	}
	if p.json {
		json.NewEncoder(p.output).Encode(result)
		return
	}
	indentation := ""
	if p.headings {
		fmt.Fprintf(p.output, "%s:\n", terminal.NeutralizeControlCharacters(path))
		indentation = "  "
	}
	for _, line := range result.lines {
		fmt.Fprintln(p.output, indentation+line)
	}
}

// retentionNote describes where a retired workspace is kept.
func retentionNote(backup string) string {
	return fmt.Sprintf("The workspace, with the original copies, is kept in %s for %d days.", backup, int(backupRetention.Hours()/24))
}

// runConflicts performs a conflict operation. Failures for individual paths
// are printed as they occur and reported with errReported, while other errors
// are returned.
func runConflicts(ctx context.Context, directory string, o *options, output, errorOutput io.Writer) error {
	pruneWorkspaces()
	if o.conflicts {
		return printConflicts(ctx, directory, o, output)
	}
	p := &printer{output: output, errors: errorOutput, json: o.json, headings: len(o.arguments) > 1}
	var err error
	if o.checkout {
		err = checkoutAll(ctx, directory, o, p)
	} else if o.apply {
		err = applyAll(ctx, directory, o, p)
	} else {
		for _, argument := range o.arguments {
			result, err := discard(argument)
			p.print(argument, result, err)
		}
	}
	if err != nil {
		return err
	} else if p.failed {
		return errReported
	}
	return nil
}

// listedConflict is a conflict as listed by --conflicts --json, with local
// paths and copies.
type listedConflict struct {
	// Path is the local path of the conflict root.
	Path string
	// Resolvable indicates whether remote can resolve the conflict.
	Resolvable bool
	// Files indicates whether both copies are files, which can be merged.
	Files bool `json:",omitempty"`
	// Take lists the copies that can be taken.
	Take []string `json:",omitempty"`
	// CheckedOut indicates whether a workspace exists for the conflict.
	CheckedOut bool `json:",omitempty"`
}

// pendingResolution is a workspace as listed by --conflicts --json whose
// conflict is no longer listed. If something was applied, its resolution is
// unfinished, and it's finished by applying again or set aside by discarding
// it. Otherwise, the conflict was resolved some other way, apply refuses it,
// and it's set aside by discarding it.
type pendingResolution struct {
	// Path is the local path of the conflict root.
	Path string
	// Applied indicates whether apply has changed (or may have changed) either
	// copy through the workspace (see workspace.applied).
	Applied bool `json:",omitempty"`
}

// localSides converts which endpoints' copies of a conflict can be taken to
// local and remote copies.
func localSides(conflict *remote.ConflictRoot, target *remote.Target) []string {
	local, opposite := conflict.AlphaTakable, conflict.BetaTakable
	if !target.Alpha {
		local, opposite = opposite, local
	}
	var sides []string
	if local {
		sides = append(sides, sideLocal)
	}
	if opposite {
		sides = append(sides, sideRemote)
	}
	return sides
}

// printConflicts lists conflicts and the workspaces whose conflicts are no
// longer listed: unfinished resolutions, whose conflicts may no longer be
// listed if synchronization propagated one copy over a partially applied
// resolution, and workspaces with nothing applied, whose conflicts were
// resolved some other way. Paths are local paths, which are relative to the
// working directory if it's inside the root, as other operations expect.
func printConflicts(ctx context.Context, directory string, o *options, output io.Writer) error {
	listing, target, err := listConflicts(ctx, directory, o.session)
	if err != nil {
		return err
	}
	l := newLocator(target)
	workspaces := sessionWorkspaces(target)
	conflicts := make([]listedConflict, len(listing.Conflicts))
	for c := range listing.Conflicts {
		conflict := &listing.Conflicts[c]
		conflicts[c] = listedConflict{
			Path: l.local(conflict.Path), Resolvable: conflict.Resolvable(), Files: conflict.Files,
			Take: localSides(conflict, target), CheckedOut: workspaces[conflict.Path] != nil,
		}
		delete(workspaces, conflict.Path)
	}
	var pending, unfinished, unapplied []string
	for path := range workspaces {
		pending = append(pending, path)
	}
	slices.Sort(pending)
	for _, path := range pending {
		if workspaces[path].applied() {
			unfinished = append(unfinished, path)
		} else {
			unapplied = append(unapplied, path)
		}
	}
	if o.json {
		listed := struct {
			Conflicts []listedConflict
			Pending   []pendingResolution
		}{Conflicts: conflicts, Pending: make([]pendingResolution, len(pending))}
		for p, path := range pending {
			listed.Pending[p] = pendingResolution{Path: l.local(path), Applied: workspaces[path].applied()}
		}
		return json.NewEncoder(output).Encode(&listed)
	} else if len(conflicts) == 0 && len(pending) == 0 {
		_, err := fmt.Fprintln(output, "No conflicts.")
		return err
	}
	for c, conflict := range conflicts {
		line := l.display(listing.Conflicts[c].Path)
		if !conflict.Resolvable {
			line += " (remote can't resolve it, since it involves a directory, the synchronization root, or content that can't be synchronized or read)"
		} else if !conflict.Files {
			line += fmt.Sprintf(" (not two files, so it can only be resolved with --take %s)", strings.Join(conflict.Take, " or --take "))
		}
		if conflict.CheckedOut {
			line += " (checked out)"
		}
		if _, err := fmt.Fprintln(output, line); err != nil {
			return err
		}
	}
	for _, section := range []struct {
		heading string
		paths   []string
	}{
		{"Applied, but no longer listed as conflicts (apply again to finish resolving, or discard):", unfinished},
		{"Checked out, but no longer listed as conflicts, with nothing applied (resolved some other way; discard, which changes neither copy):", unapplied},
	} {
		if len(section.paths) == 0 {
			continue
		}
		fmt.Fprintln(output, section.heading)
		for _, path := range section.paths {
			if _, err := fmt.Fprintln(output, "  "+l.display(path)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkoutAll checks out the specified conflicts after listing them.
func checkoutAll(ctx context.Context, directory string, o *options, p *printer) error {
	listing, target, err := listConflicts(ctx, directory, o.session)
	if err != nil {
		return err
	}
	for _, argument := range o.arguments {
		result, err := checkout(ctx, directory, o, listing, target, argument)
		p.print(argument, result, err)
	}
	return nil
}

// checkout downloads both copies of a conflict into its workspace. An existing
// workspace is reused, preserving edits to its merged file, even if its
// conflict is no longer listed, but not with a specified common ancestor,
// which only a new workspace's merged file can be based on.
func checkout(ctx context.Context, directory string, o *options, listing *remote.Listing, target *remote.Target, argument string) (*outcome, error) {
	local, err := filepath.Abs(argument)
	if err != nil {
		return nil, fmt.Errorf("unable to locate it: %w", err)
	}
	unlock, err := lockWorkspace(local)
	if err != nil {
		return nil, err
	}
	defer unlock()
	path, err := newLocator(target).resolve(local)
	if err != nil {
		return nil, err
	}
	conflict := findConflict(listing, path)
	if w, err := loadWorkspace(local); err == nil {
		if !w.matches(target, path) {
			return nil, errors.New("an unrelated workspace exists for this path; discard it first")
		} else if w.hasDirectory() {
			return nil, errDirectoryConflict
		} else if o.base != "" {
			// The merged file may have been edited, and merging with another
			// ancestor would replace it.
			return nil, errors.New("is already checked out, so --base can't be used; discard the workspace first (which changes neither copy), then check out again with --base")
		}
		return describe(w, argument, conflict == nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if conflict == nil {
		return nil, errors.New("isn't a conflict root (see remote --conflicts)")
	} else if !conflict.Resolvable() {
		return nil, errors.New("isn't a conflict that remote can resolve (see remote --conflicts)")
	} else if o.base != "" && !conflict.Files {
		return nil, errors.New("--base requires a conflict between two files")
	}
	base, final, err := workspaceDirectory(local)
	if err != nil {
		return nil, err
	} else if err := outside(base, target.Root); err != nil {
		return nil, err
	}

	// Prepare the workspace in a temporary directory and move it into place
	// once complete, so that interrupted checkouts leave no workspace.
	staging, err := os.MkdirTemp(base, stagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("unable to create workspace: %w", err)
	}
	defer os.RemoveAll(staging)
	w := &workspace{Session: target.Session, Alpha: target.Alpha, Path: path, Token: rand.Text()[:8], directory: staging}
	if conflict.Files {
		err = checkoutFiles(ctx, conflict, local, o.base, w, func(output io.Writer) (*remote.Entry, error) {
			return remote.CopyFile(ctx, target.Root, path, output)
		}, func(output io.Writer) (*remote.Entry, error) {
			connection, err := dialOpposite(ctx, directory, o.session, target)
			if err != nil {
				return nil, err
			}
			return remote.Copy(ctx, connection, path, output)
		})
	} else {
		w.Take = localSides(conflict, target)
		err = checkoutEntries(ctx, directory, o, target, w)
	}
	if err != nil {
		return nil, err
	} else if err := w.save(); err != nil {
		return nil, fmt.Errorf("unable to save workspace: %w", err)
	} else if err := os.Rename(staging, final); err != nil {
		return nil, fmt.Errorf("unable to save workspace: %w", err)
	}
	// The workspace's files and directory were flushed (see workspace.save),
	// so flush the workspaces directory that it was moved into, and that
	// directory's parent, in case the workspaces directory was just created,
	// so that the copies survive a power loss or an operating system crash
	// once checkout reports them.
	if err := errors.Join(remote.SyncDirectory(base), remote.SyncDirectory(filepath.Dir(base))); err != nil {
		return nil, fmt.Errorf("unable to save workspace: %w", err)
	}
	w.directory = final
	return describe(w, argument, false)
}

// errCopiesChanged indicates that the copies downloaded for a checkout aren't
// the ones that the conflict listing reported, so that the listed common
// ancestor might not be theirs.
var errCopiesChanged = errors.New("the copies changed since conflicts were listed; check out again")

// copier writes a copy of a conflicting entry to output and returns the entry
// as read at its source.
type copier func(output io.Writer) (*remote.Entry, error)

// checkoutFiles downloads both copies of a conflict between two files into a
// workspace using functions that copy the local and remote copies, along with
// their common ancestor, if it can be found, and prepares the merged file.
//
// The listed common ancestor is that of the listed copies, but synchronization
// doesn't pause meanwhile: copies that came to agree and then changed again
// have another ancestor, and merging them with the listed one would silently
// revert the changes that it doesn't contain. The copies are thus hashed with
// the session's hashing algorithm as they're downloaded and must be the listed
// ones. If they can't be compared, such as because this build doesn't support
// the algorithm, no ancestor can be verified either (see findBase).
func checkoutFiles(ctx context.Context, conflict *remote.ConflictRoot, local, explicitBase string, w *workspace, copyLocal, copyRemote copier) error {
	algorithm, unverifiable := listedAlgorithm(conflict)
	var hashers [2]hash.Hash
	if unverifiable == nil {
		hashers = [2]hash.Hash{algorithm.Factory()(), algorithm.Factory()()}
	}
	download := func(name string, source copier, hasher hash.Hash) (*remote.Entry, error) {
		return writeCopy(w.file(name), func(file io.Writer) (*remote.Entry, error) {
			if hasher != nil {
				file = io.MultiWriter(file, hasher)
			}
			return source(file)
		})
	}
	listed := [2]string{conflict.AlphaDigest, conflict.BetaDigest}
	if !w.Alpha {
		listed[0], listed[1] = listed[1], listed[0]
	}
	var err error
	if w.Local, err = download(localName, copyLocal, hashers[0]); err != nil {
		return fmt.Errorf("unable to copy the local copy: %w", err)
	} else if w.Remote, err = download(remoteName, copyRemote, hashers[1]); err != nil {
		return fmt.Errorf("unable to download the remote copy: %w", err)
	} else if w.Local.Kind != remote.EntryFile || w.Remote.Kind != remote.EntryFile {
		return errors.New("the copies are no longer both files; list conflicts again")
	} else if remote.SameFile(w.Local, w.Remote) {
		return errors.New("the copies are now identical; list conflicts again")
	} else if unverifiable == nil && (fmt.Sprintf("%x", hashers[0].Sum(nil)) != listed[0] || fmt.Sprintf("%x", hashers[1].Sum(nil)) != listed[1]) {
		return errCopiesChanged
	} else if w.BaseSource, err = findBase(ctx, conflict, local, explicitBase, w.file(baseName)); err != nil {
		return err
	} else if err := prepareMerged(w); err != nil {
		return fmt.Errorf("unable to create merged file: %w", err)
	}
	return nil
}

// checkoutEntries downloads both copies of a conflict other than between two
// files into a workspace.
func checkoutEntries(ctx context.Context, directory string, o *options, target *remote.Target, w *workspace) error {
	var err error
	w.Local, err = extractCopy(ctx, w.file(localName), func(output io.Writer) (*remote.Entry, error) {
		return remote.ArchiveEntry(ctx, target.Root, w.Path, output)
	})
	if err != nil {
		return fmt.Errorf("unable to copy the local copy: %w", err)
	}
	w.Remote, err = extractCopy(ctx, w.file(remoteName), func(output io.Writer) (*remote.Entry, error) {
		connection, err := dialOpposite(ctx, directory, o.session, target)
		if err != nil {
			return nil, err
		}
		return remote.Archive(ctx, connection, w.Path, output)
	})
	if err != nil {
		return fmt.Errorf("unable to download the remote copy: %w", err)
	} else if w.hasDirectory() {
		return errDirectoryConflict
	} else if w.files() {
		return errors.New("the copies are now both files; list conflicts again")
	}
	return nil
}

// describeKind describes the kind of a copy.
func describeKind(entry *remote.Entry) string {
	if entry.Kind == remote.EntryMissing {
		return "missing"
	}
	return entry.Kind
}

// pendingNote describes what remains to do for a workspace whose conflict is
// no longer listed.
func pendingNote(w *workspace) string {
	if w.applied() {
		return "The conflict is no longer listed: apply again to finish resolving it, or discard the workspace."
	}
	return "The conflict is no longer listed, and nothing was applied, so it was resolved some other way: discard the workspace, which changes neither copy (apply refuses it)."
}

// describe describes a workspace for its checkout.
func describe(w *workspace, argument string, pending bool) (*outcome, error) {
	result := &outcome{Path: argument, Local: w.file(localName), Remote: w.file(remoteName), Files: w.files(), Take: w.Take, Pending: pending, Applied: w.applied()}
	note := func(format string, arguments ...any) {
		result.lines = append(result.lines, fmt.Sprintf(format, arguments...))
	}
	if !w.files() {
		note("Local:  %s (%s)", result.Local, describeKind(w.Local))
		note("Remote: %s (%s)", result.Remote, describeKind(w.Remote))
		if pending {
			note("%s", pendingNote(w))
		}
		var options []string
		for _, side := range w.Take {
			options = append(options, "--take "+side)
		}
		note("The copies aren't both files, so they can't be merged. Apply with %s to write that copy, as checked out, to both endpoints.", strings.Join(options, " or "))
		return result, nil
	}
	result.Merged = w.file(mergedName)
	if w.BaseSource != "" {
		result.Base = w.file(baseName)
	}
	note("Local:  %s", result.Local)
	note("Remote: %s", result.Remote)
	if result.Base != "" {
		note("Base:   %s (%s)", result.Base, w.BaseSource)
	}
	note("Merged: %s", result.Merged)
	if pending {
		note("%s", pendingNote(w))
	}
	if _, err := os.Lstat(result.Merged); errors.Is(err, os.ErrNotExist) {
		result.Binary = true
		note("The copies are binary, so no merged file was created: create it, or use --take.")
	} else if w.Local.Digest != w.Remote.Digest {
		regions, err := countMarkers(result.Merged, markersFor(w.Token).local)
		if err != nil {
			return nil, err
		}
		result.Regions = regions
		if result.Base == "" {
			note("No common ancestor was found, so every region where the copies differ is marked (%d), and neither side's additions or deletions are known to be newer.", regions)
		} else if regions == 0 {
			note("Both copies' changes since their common ancestor were merged without overlapping: review the merged file, then apply it.")
		} else {
			note("%d regions changed differently on each side since their common ancestor are marked with the local, base, and remote versions.", regions)
		}
		if unterminated := unterminatedNote(w); unterminated != "" {
			note("%s", unterminated)
		}
	}
	if w.Local.Executable != w.Remote.Executable {
		note("The copies differ in executability: specify --executable or --take when applying.")
	}
	return result, nil
}

// resolution determines the result entry of a resolution and the workspace
// file providing its contents (for a file result). Copies that aren't both
// files can only be resolved by taking one of them as checked out.
func resolution(ctx context.Context, w *workspace, o *options) (*remote.Entry, string, error) {
	if !w.files() && (o.take == "" || o.executable != "") {
		return nil, "", errors.New("the copies aren't both files, so they can only be resolved with --take local or --take remote")
	} else if o.take != "" && !w.takable(o.take) {
		return nil, "", fmt.Errorf("the %s copy can't be taken for this conflict (see remote --conflicts)", o.take)
	} else if o.delete {
		if w.Path == "" {
			return nil, "", errors.New("the synchronization root can't be deleted")
		}
		return &remote.Entry{Kind: remote.EntryMissing}, "", nil
	}
	source, selected := w.file(mergedName), (*remote.Entry)(nil)
	if o.take == sideLocal {
		source, selected = w.file(localName), w.Local
	} else if o.take == sideRemote {
		source, selected = w.file(remoteName), w.Remote
	}
	if selected != nil && selected.Kind == remote.EntrySymbolicLink {
		return &remote.Entry{Kind: remote.EntrySymbolicLink, Target: selected.Target}, "", nil
	} else if selected != nil && selected.Kind != remote.EntryFile {
		return nil, "", fmt.Errorf("the %s copy can't be taken for this conflict (see remote --conflicts)", o.take)
	}
	result, err := remote.ReadEntry(ctx, source)
	if err != nil {
		return nil, "", err
	} else if result.Kind == remote.EntryMissing {
		return nil, "", fmt.Errorf("%s doesn't exist: create it, or use --take or --delete", source)
	} else if result.Kind != remote.EntryFile {
		return nil, "", fmt.Errorf("%s isn't a regular file", source)
	} else if selected != nil && (result.Size != selected.Size || result.Digest != selected.Digest) {
		if w.files() {
			return nil, "", fmt.Errorf("%s was modified; edit the merged file instead", source)
		}
		return nil, "", fmt.Errorf("%s was modified, but --take writes the copy as checked out", source)
	} else if selected == nil {
		if found, err := containsMarkers(source, w.Token); err != nil {
			return nil, "", err
		} else if found {
			return nil, "", errors.New("the merged file still contains conflict markers")
		}
	}
	result.Data = nil
	if o.executable != "" {
		result.Executable, _ = strconv.ParseBool(o.executable)
	} else if selected != nil {
		result.Executable = selected.Executable
	} else if w.Local.Executable == w.Remote.Executable {
		result.Executable = w.Local.Executable
	} else {
		return nil, "", errors.New("the copies differ in executability: specify --executable true or false")
	}
	return result, source, nil
}

// overlaps determines whether two slash-separated paths relative to the
// synchronization root are the same or one contains the other. An empty path
// refers to the root, which contains every path.
func overlaps(a, b string) bool {
	return a == b || a == "" || b == "" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// staleHint adds advice to an error caused by a copy that changed since it
// was checked out.
func staleHint(err error) error {
	if errors.Is(err, remote.ErrStale) {
		return fmt.Errorf("%w; discard the workspace and check out again", err)
	}
	return err
}

// application is a resolution whose copies have been updated, pending
// verification once synchronized.
type application struct {
	// argument is the path as specified.
	argument string
	// w is the workspace.
	w *workspace
	// result is the resolution's result.
	result *remote.Entry
	// target is the session's target.
	target *remote.Target
	// locator maps paths for display.
	locator *locator
}

// applyAll applies the resolutions of the specified conflicts, then
// synchronizes once and verifies them, synchronizing once more beforehand if
// any workspace has nothing applied (see apply). Workspaces remain locked
// until verified.
func applyAll(ctx context.Context, directory string, o *options, p *printer) error {
	var unlocks []func()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	// Results aren't written through workspaces with nothing applied whose
	// conflicts are no longer listed (see workspace.applied), which requires a
	// listing before writing them. It's made at most once, when first needed,
	// and isn't needed to finish resolutions applied before.
	var preliminary *remote.Listing
	var preliminaryErr error
	listed := func() (*remote.Listing, error) {
		if preliminary == nil && preliminaryErr == nil {
			preliminary, _, preliminaryErr = listConflicts(ctx, directory, o.session)
		}
		return preliminary, preliminaryErr
	}
	var applications []*application
	for _, argument := range o.arguments {
		a, unlock, err := apply(ctx, directory, o, argument, listed)
		if unlock != nil {
			unlocks = append(unlocks, unlock)
		}
		if err != nil {
			p.print(argument, nil, err)
		} else {
			applications = append(applications, a)
		}
	}
	if len(applications) == 0 {
		return nil
	}
	listing, _, listErr := listConflicts(ctx, directory, o.session)
	for _, a := range applications {
		if listErr != nil {
			p.print(a.argument, nil, fmt.Errorf("updated the copies, but unable to verify synchronization (retry to verify): %w", listErr))
			continue
		}
		result, err := a.verify(ctx, directory, o, listing)
		p.print(a.argument, result, err)
	}
	return nil
}

// apply writes the resolution of a conflict to both copies, the remote copy
// first. It returns a function that unlocks the workspace, if it was locked.
//
// Resolution requires that neither copy changes from checkout until apply
// completes, but synchronization doesn't pause meanwhile: once one copy has a
// result that's a deletion or matches what both copies last agreed on,
// synchronization may copy the other checked-out copy over it, removing the
// conflict, and propagates the result back once the other copy has it too. A
// copy is thus replaced if it's either checked-out copy, which the workspace
// keeps, or a result applied before, so that retries can finish a partially
// applied resolution, even with another result, whether or not its conflict
// is still listed. Copies that already have the result are left as they are,
// and any other copy fails the resolution as changed.
//
// A workspace with nothing applied is refused, however, if its conflict is no
// longer listed, using a listing obtained from listed (see workspace.applied):
// the conflict was then resolved some other way, such as through a workspace
// that the other endpoint checked out, possibly with a result that's one of
// the checked-out copies, which this workspace would otherwise accept and
// replace.
func apply(ctx context.Context, directory string, o *options, argument string, listed func() (*remote.Listing, error)) (*application, func(), error) {
	local, err := filepath.Abs(argument)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to locate it: %w", err)
	}
	unlock, err := lockWorkspace(local)
	if err != nil {
		return nil, nil, err
	}
	w, err := loadWorkspace(local)
	if errors.Is(err, os.ErrNotExist) {
		return nil, unlock, errors.New("isn't checked out (see remote --conflict-checkout)")
	} else if err != nil {
		return nil, unlock, err
	} else if w.hasDirectory() {
		return nil, unlock, errDirectoryConflict
	}
	result, source, err := resolution(ctx, w, o)
	if err != nil {
		return nil, unlock, err
	}
	connection, target, err := remote.Dial(ctx, directory, o.session, remote.OperationExecute)
	if err != nil {
		return nil, unlock, err
	}
	a := &application{argument: argument, w: w, result: result, target: target, locator: newLocator(target)}
	if path, err := a.locator.resolve(local); err != nil || !w.matches(target, path) {
		connection.Close()
		return nil, unlock, errors.New("the workspace was checked out through a different session or endpoint")
	} else if !w.applied() {
		if listing, err := listed(); err != nil {
			connection.Close()
			return nil, unlock, fmt.Errorf("unable to synchronize before applying: %w", err)
		} else if findConflict(listing, w.Path) == nil {
			connection.Close()
			return nil, unlock, errResolvedOtherwise
		}
	}
	return a, unlock, a.write(ctx, connection, source)
}

// write writes the result to both copies using a connection to the opposite
// endpoint, reading a file result's contents from a workspace file.
func (a *application) write(ctx context.Context, connection io.ReadWriteCloser, source string) error {
	w := a.w
	open := func() (io.ReadCloser, error) {
		if source == "" {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return os.Open(source)
	}
	expected := append([]*remote.Entry{w.Local, w.Remote}, w.Applied...)
	replacement := &remote.Replacement{Path: w.Path, Expected: expected, Result: a.result}
	// Check the local copy first, so that one changed since checkout doesn't
	// leave only the remote copy updated.
	if err := remote.CheckFile(ctx, a.target.Root, replacement); err != nil {
		connection.Close()
		return fmt.Errorf("unable to update the local copy: %w", staleHint(err))
	}
	// Record the result before writing it, since either write may succeed even
	// if apply fails, so that retries recognize it.
	if !slices.ContainsFunc(w.Applied, func(applied *remote.Entry) bool {
		return remote.SameContents(applied, a.result)
	}) {
		w.Applied = append(w.Applied, a.result)
		if err := w.save(); err != nil {
			connection.Close()
			return fmt.Errorf("unable to save workspace: %w", err)
		}
	}
	input, err := open()
	if err != nil {
		connection.Close()
		return err
	}
	err = remote.Replace(ctx, connection, replacement, input)
	input.Close()
	if err != nil {
		return fmt.Errorf("unable to update the remote copy: %w", staleHint(err))
	}
	if input, err = open(); err != nil {
		return err
	}
	err = remote.ReplaceFile(ctx, a.target.Root, replacement, input)
	input.Close()
	if err != nil {
		return fmt.Errorf("updated the remote copy, but unable to update the local copy: %w", staleHint(err))
	}
	return nil
}

// unsettled returns an error describing a conflict or a problem, in a listing
// made after a resolution was applied, that involves the conflict root,
// including one at an ancestor or a descendant, such as a conflict that a
// concurrent change created, if there's one. Problems may leave the copies
// unsynchronized without a conflict, such as a symbolic link whose target
// can't be synchronized portably, or content that an endpoint can't read or
// write.
func (a *application) unsettled(listing *remote.Listing) error {
	for _, conflict := range listing.Conflicts {
		if overlaps(conflict.Path, a.w.Path) {
			return fmt.Errorf("updated the copies, but %s conflicts again, so something changed meanwhile; discard the workspace and list conflicts again", a.locator.display(conflict.Path))
		}
	}
	for _, problem := range listing.Problems {
		if overlaps(problem.Path, a.w.Path) {
			side := sideRemote
			if problem.Alpha == a.target.Alpha {
				side = sideLocal
			}
			return fmt.Errorf("updated the copies, but synchronization reports a problem with %s on the %s endpoint (%s), so the resolution isn't verified; fix it, then retry, or discard the workspace",
				a.locator.display(problem.Path), side, terminal.NeutralizeControlCharacters(problem.Error))
		}
	}
	return nil
}

// verify verifies that synchronization resolved a conflict, using a listing
// made after the resolution was applied, and sets its workspace aside. No
// conflict or problem may involve the conflict root (see unsettled), and both
// copies must have the result.
func (a *application) verify(ctx context.Context, directory string, o *options, listing *remote.Listing) (*outcome, error) {
	w := a.w
	unverified := func(err error) error {
		return fmt.Errorf("updated the copies, but unable to verify synchronization (retry to verify): %w", err)
	}
	if err := a.unsettled(listing); err != nil {
		return nil, err
	}
	localEntry, err := remote.ReadEntry(ctx, filepath.Join(a.target.Root, filepath.FromSlash(w.Path)))
	if err != nil {
		return nil, unverified(err)
	}
	connection, err := dialOpposite(ctx, directory, o.session, a.target)
	if err != nil {
		return nil, unverified(err)
	}
	remoteEntry, err := remote.Fetch(ctx, connection, w.Path)
	if err != nil {
		return nil, unverified(err)
	}
	// Executability isn't compared, since not every filesystem preserves it.
	if !remote.SameContents(localEntry, a.result) || !remote.SameContents(remoteEntry, a.result) {
		return nil, errors.New("updated both copies, but after synchronizing they don't both have the result; retry to apply it again")
	}
	resolved := "Resolved; both copies now match the result."
	if !w.files() {
		resolved = fmt.Sprintf("Resolved; the %s copy was taken.", o.take)
	}
	backup, err := w.retire()
	if err != nil {
		return nil, fmt.Errorf("resolved, but unable to set the workspace aside: %w", err)
	}
	return &outcome{Path: a.argument, Resolved: true, Backup: backup, lines: []string{resolved, retentionNote(backup)}}, nil
}

// discard sets a workspace aside without changing either copy.
func discard(argument string) (*outcome, error) {
	local, err := filepath.Abs(argument)
	if err != nil {
		return nil, fmt.Errorf("unable to locate it: %w", err)
	}
	unlock, err := lockWorkspace(local)
	if err != nil {
		return nil, err
	}
	defer unlock()
	w, err := loadWorkspace(local)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("isn't checked out")
	} else if err != nil {
		return nil, err
	}
	backup, err := w.retire()
	if err != nil {
		return nil, fmt.Errorf("unable to set the workspace aside: %w", err)
	}
	return &outcome{Path: argument, Discarded: true, Backup: backup, lines: []string{
		"Discarded the workspace without changing either copy.", retentionNote(backup),
	}}, nil
}
