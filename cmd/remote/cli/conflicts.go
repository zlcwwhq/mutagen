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

	"github.com/spf13/pflag"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/filesystem/locking"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

const (
	// workspacesDirectoryName is the name of the Mutagen data subdirectory
	// containing conflict workspaces.
	workspacesDirectoryName = "conflicts"
	// metadataName, localName, remoteName, and mergedName are the names of the
	// files in a workspace.
	metadataName, localName, remoteName, mergedName = "workspace.json", "local", "remote", "merged"
)

// conflictOptions are the options for conflict resolution, which is separate
// from command execution and its flush policy.
type conflictOptions struct {
	// conflicts indicates that conflicts should be listed.
	conflicts bool
	// checkout, apply, and discard are the local paths of conflicting files
	// for the corresponding operations.
	checkout, apply, discard string
	// take selects an original copy, local or remote, as the result.
	take string
	// delete selects deletion of both copies as the result.
	delete bool
	// executable explicitly specifies the result's executability.
	executable string
	// json requests JSON output.
	json bool
}

// active indicates whether a conflict operation was requested.
func (o *conflictOptions) active() bool {
	return o.conflicts || o.checkout != "" || o.apply != "" || o.discard != ""
}

// register registers conflict options.
func (o *conflictOptions) register(flags *pflag.FlagSet) {
	flags.BoolVar(&o.conflicts, "conflicts", false, "Synchronize, then list all conflicts and whether remote can resolve them")
	flags.StringVar(&o.checkout, "conflict-checkout", "", "Download both copies of the conflicting file at `PATH` into a workspace for merging")
	flags.StringVar(&o.apply, "conflict-apply", "", "Write the merged file for `PATH` to both copies and verify that the conflict is resolved")
	flags.StringVar(&o.discard, "conflict-discard", "", "Remove the workspace for `PATH` without changing either copy")
	flags.StringVar(&o.take, "take", "", "With --conflict-apply, use the original `SIDE` (local or remote) instead of the merged file")
	flags.BoolVar(&o.delete, "delete", false, "With --conflict-apply, delete both copies instead")
	flags.StringVar(&o.executable, "executable", "", "With --conflict-apply, set whether the result is executable (`BOOL`: true or false)")
	flags.BoolVar(&o.json, "json", false, "Print the result of a conflict operation as JSON")
}

// validateConflicts checks the combination of conflict options.
func (o *options) validateConflicts() error {
	modes := 0
	for _, enabled := range []bool{o.conflicts, o.checkout != "", o.apply != "", o.discard != ""} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("--conflicts, --conflict-checkout, --conflict-apply, and --conflict-discard are mutually exclusive")
	} else if modes == 1 && (o.flush || o.noFlush || o.diff || len(o.arguments) != 0 || o.directory != "" ||
		len(o.environment) != 0 || len(o.environmentFiles) != 0 || o.noPrologue) {
		return errors.New("conflict operations can't be combined with commands or command options")
	} else if o.json && modes == 0 {
		return errors.New("--json requires a conflict operation")
	} else if o.apply == "" && (o.take != "" || o.delete || o.executable != "") {
		return errors.New("--take, --delete, and --executable require --conflict-apply")
	} else if o.take != "" && o.take != "local" && o.take != "remote" {
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
// synchronization root, and identified by the local file's path.
type workspace struct {
	// Session is the session identifier.
	Session string
	// Alpha indicates whether the local copy is alpha's.
	Alpha bool
	// Path is the slash-separated conflict root relative to the
	// synchronization root.
	Path string
	// Token labels conflict markers in the merged file.
	Token string
	// Local and Remote describe the checked-out copies.
	Local, Remote *remote.Entry
	// Applied describes the file results that apply has written (or may have
	// written) to either copy, which retries may supersede, since they're the
	// workspace's own, even if the merged file has changed since.
	Applied []*remote.Entry `json:",omitempty"`
	// directory is the workspace directory.
	directory string
}

// file returns the path of a file in the workspace.
func (w *workspace) file(name string) string {
	return filepath.Join(w.directory, name)
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
		return nil, fmt.Errorf("another conflict operation on this file is in progress: %w", err)
	}
	return func() {
		locker.Unlock()
		locker.Close()
	}, nil
}

// save atomically writes the metadata of a workspace.
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
	}
	return os.Rename(file.Name(), w.file(metadataName))
}

// loadWorkspace loads the workspace for a local path, failing with an error
// wrapping os.ErrNotExist if there's none.
func loadWorkspace(local string) (*workspace, error) {
	_, directory, err := workspaceDirectory(local)
	if err != nil {
		return nil, err
	}
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
		return applied == nil || applied.Kind != remote.EntryFile
	}) {
		return nil, errors.New("invalid workspace: invalid applied result")
	}
	return w, nil
}

// matches determines whether a workspace was checked out through a target.
func (w *workspace) matches(target *remote.Target, path string) bool {
	return w.Session == target.Session && w.Alpha == target.Alpha && w.Path == path
}

// listConflicts synchronizes the session and lists all of its conflicts.
func listConflicts(ctx context.Context, directory, session string) ([]remote.ConflictRoot, *remote.Target, error) {
	connection, target, err := remote.Dial(ctx, directory, session, remote.OperationConflicts)
	if err != nil {
		return nil, nil, err
	}
	conflicts, err := remote.ListConflicts(ctx, connection)
	return conflicts, target, err
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
		return nil, errors.New("the copy was received incompletely or corrupted; check out again")
	}
	return entry, nil
}

// runConflicts performs a conflict operation.
func runConflicts(ctx context.Context, directory string, o *options, output io.Writer) error {
	if o.conflicts {
		return printConflicts(ctx, directory, o, output)
	}
	argument := o.checkout + o.apply + o.discard
	local, err := filepath.Abs(argument)
	if err != nil {
		return fmt.Errorf("unable to locate %s: %w", argument, err)
	}
	unlock, err := lockWorkspace(local)
	if err != nil {
		return err
	}
	defer unlock()
	if o.checkout != "" {
		return checkout(ctx, directory, o, local, output)
	} else if o.apply != "" {
		return apply(ctx, directory, o, local, output)
	}
	w, err := loadWorkspace(local)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s isn't checked out", argument)
	} else if err != nil {
		return err
	} else if err := os.RemoveAll(w.directory); err != nil {
		return fmt.Errorf("unable to remove workspace: %w", err)
	} else if o.json {
		return json.NewEncoder(output).Encode(map[string]bool{"Discarded": true})
	}
	_, err = fmt.Fprintln(output, "Discarded the workspace; neither copy changed.")
	return err
}

// printConflicts lists conflicts. Paths are local paths, which are relative to
// the working directory if it's inside the root, as checkout expects.
func printConflicts(ctx context.Context, directory string, o *options, output io.Writer) error {
	conflicts, target, err := listConflicts(ctx, directory, o.session)
	if err != nil {
		return err
	}
	l := newLocator(target)
	if o.json {
		listing := struct{ Conflicts []remote.ConflictRoot }{make([]remote.ConflictRoot, len(conflicts))}
		for c, conflict := range conflicts {
			listing.Conflicts[c] = remote.ConflictRoot{Path: l.local(conflict.Path), Resolvable: conflict.Resolvable}
		}
		return json.NewEncoder(output).Encode(&listing)
	} else if len(conflicts) == 0 {
		_, err := fmt.Fprintln(output, "No conflicts.")
		return err
	}
	for _, conflict := range conflicts {
		line := l.display(conflict.Path)
		if !conflict.Resolvable {
			line += " (not a conflict between two files, so remote can't resolve it)"
		}
		if _, err := fmt.Fprintln(output, line); err != nil {
			return err
		}
	}
	return nil
}

// checkout downloads both copies of a conflicting file into its workspace. An
// existing workspace is reused, preserving edits to its merged file.
func checkout(ctx context.Context, directory string, o *options, local string, output io.Writer) error {
	conflicts, target, err := listConflicts(ctx, directory, o.session)
	if err != nil {
		return err
	}
	path, err := newLocator(target).resolve(local)
	if err != nil {
		return err
	}
	var conflict *remote.ConflictRoot
	for c := range conflicts {
		if conflicts[c].Path == path {
			conflict = &conflicts[c]
		}
	}
	if conflict == nil {
		return fmt.Errorf("%s isn't a conflict root (see remote --conflicts)", o.checkout)
	} else if !conflict.Resolvable {
		return fmt.Errorf("%s isn't a conflict between two files, so remote can't resolve it", o.checkout)
	}
	if w, err := loadWorkspace(local); err == nil {
		if !w.matches(target, path) {
			return errors.New("an unrelated workspace exists for this path; discard it first")
		}
		return printWorkspace(output, w, o.json)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	base, final, err := workspaceDirectory(local)
	if err != nil {
		return err
	} else if err := outside(base, target.Root); err != nil {
		return err
	}

	// Prepare the workspace in a temporary directory and move it into place
	// once complete, so that interrupted checkouts leave no workspace.
	staging, err := os.MkdirTemp(base, ".checkout-")
	if err != nil {
		return fmt.Errorf("unable to create workspace: %w", err)
	}
	defer os.RemoveAll(staging)
	w := &workspace{Session: target.Session, Alpha: target.Alpha, Path: path, Token: rand.Text()[:8], directory: staging}
	w.Local, err = writeCopy(w.file(localName), func(file io.Writer) (*remote.Entry, error) {
		return remote.CopyFile(ctx, target.Root, path, file)
	})
	if err != nil {
		return fmt.Errorf("unable to copy the local copy: %w", err)
	}
	w.Remote, err = writeCopy(w.file(remoteName), func(file io.Writer) (*remote.Entry, error) {
		connection, err := dialOpposite(ctx, directory, o.session, target)
		if err != nil {
			return nil, err
		}
		return remote.Copy(ctx, connection, path, file)
	})
	if err != nil {
		return fmt.Errorf("unable to download the remote copy: %w", err)
	} else if w.Local.Kind != remote.EntryFile || w.Remote.Kind != remote.EntryFile {
		return errors.New("the copies are no longer both files; list conflicts again")
	} else if remote.SameFile(w.Local, w.Remote) {
		return errors.New("the copies are now identical; list conflicts again")
	} else if err := prepareMerged(w); err != nil {
		return fmt.Errorf("unable to create merged file: %w", err)
	}
	if err := w.save(); err != nil {
		return fmt.Errorf("unable to save workspace: %w", err)
	} else if err := os.Rename(staging, final); err != nil {
		return fmt.Errorf("unable to save workspace: %w", err)
	}
	w.directory = final
	return printWorkspace(output, w, o.json)
}

// printWorkspace prints the files of a workspace.
func printWorkspace(output io.Writer, w *workspace, jsonOutput bool) error {
	_, err := os.Lstat(w.file(mergedName))
	binary := errors.Is(err, os.ErrNotExist)
	if jsonOutput {
		return json.NewEncoder(output).Encode(&struct {
			Local, Remote, Merged string
			Binary                bool
		}{w.file(localName), w.file(remoteName), w.file(mergedName), binary})
	}
	fmt.Fprintf(output, "Local:  %s\nRemote: %s\nMerged: %s\n", w.file(localName), w.file(remoteName), w.file(mergedName))
	if binary {
		fmt.Fprintln(output, "The copies are binary, so no merged file was created: create it, or use --take.")
	} else if w.Local.Executable != w.Remote.Executable {
		fmt.Fprintln(output, "The copies differ in executability: specify --executable or --take when applying.")
	}
	if !binary && w.Local.Digest != w.Remote.Digest {
		if note := unterminatedNote(w); note != "" {
			fmt.Fprintln(output, note)
		}
	}
	return nil
}

// resolution determines the result entry of a resolution and the workspace
// file providing its contents (for a file result).
func resolution(ctx context.Context, w *workspace, o *options) (*remote.Entry, string, error) {
	if o.delete {
		return &remote.Entry{Kind: remote.EntryMissing}, "", nil
	}
	source, selected := w.file(mergedName), (*remote.Entry)(nil)
	if o.take == "local" {
		source, selected = w.file(localName), w.Local
	} else if o.take == "remote" {
		source, selected = w.file(remoteName), w.Remote
	}
	result, err := remote.ReadEntry(ctx, source)
	if err != nil {
		return nil, "", err
	} else if result.Kind == remote.EntryMissing {
		return nil, "", fmt.Errorf("%s doesn't exist: create it, or use --take or --delete", source)
	} else if result.Kind != remote.EntryFile {
		return nil, "", fmt.Errorf("%s isn't a regular file", source)
	} else if selected != nil && (result.Size != selected.Size || result.Digest != selected.Digest) {
		return nil, "", fmt.Errorf("%s was modified; edit the merged file instead", source)
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

// apply writes a resolution to both copies and then verifies that
// synchronization resolved the conflict. Synchronization doesn't pause between
// the writes, and it may propagate the remaining original copy over a partially
// applied result (such as a deletion, or a result matching the copies' common
// ancestor), removing the conflict without resolving it. Thus, each copy is
// replaced if it matches either checked-out copy or a previously applied result
// (or already matches the result), so that failures can be retried whether or
// not the conflict is still listed, and with a different result.
func apply(ctx context.Context, directory string, o *options, local string, output io.Writer) error {
	w, err := loadWorkspace(local)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s isn't checked out (see remote --conflict-checkout)", o.apply)
	} else if err != nil {
		return err
	}
	result, source, err := resolution(ctx, w, o)
	if err != nil {
		return err
	}
	open := func() (io.ReadCloser, error) {
		if source == "" {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return os.Open(source)
	}

	// Update the remote copy, then the local copy.
	connection, target, err := remote.Dial(ctx, directory, o.session, remote.OperationExecute)
	if err != nil {
		return err
	}
	l := newLocator(target)
	if path, err := l.resolve(local); err != nil || !w.matches(target, path) {
		connection.Close()
		return errors.New("the workspace was checked out through a different session or endpoint")
	}
	expected := append([]*remote.Entry{w.Local, w.Remote}, w.Applied...)
	replacement := &remote.Replacement{Path: w.Path, Expected: expected, Result: result}
	// Check the local copy first, so that one edited since checkout (such as
	// instead of the merged file) doesn't leave only the remote copy updated.
	if err := remote.CheckFile(ctx, target.Root, replacement); err != nil {
		connection.Close()
		return fmt.Errorf("unable to update the local copy: %w", err)
	}
	// Record a file result before writing it, since either write may succeed
	// even if apply fails, so that retries recognize it.
	if result.Kind == remote.EntryFile && !slices.ContainsFunc(w.Applied, func(applied *remote.Entry) bool {
		return remote.SameContents(applied, result)
	}) {
		w.Applied = append(w.Applied, result)
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
		return fmt.Errorf("unable to update the remote copy: %w", err)
	}
	if input, err = open(); err != nil {
		return err
	}
	err = remote.ReplaceFile(ctx, target.Root, replacement, input)
	input.Close()
	if err != nil {
		return fmt.Errorf("updated the remote copy, but unable to update the local copy: %w", err)
	}

	// Verify that synchronization resolved the conflict. No conflict may
	// involve the file, including one at an ancestor or a descendant that a
	// concurrent change created, and both copies must have the result.
	unverified := func(err error) error {
		return fmt.Errorf("updated both copies, but unable to verify synchronization (retry to verify): %w", err)
	}
	conflicts, _, err := listConflicts(ctx, directory, o.session)
	if err != nil {
		return unverified(err)
	}
	for _, conflict := range conflicts {
		if overlaps(conflict.Path, w.Path) {
			return fmt.Errorf("updated both copies, but %s conflicts again, so something changed meanwhile; discard the workspace and list conflicts again", l.display(conflict.Path))
		}
	}
	localEntry, err := remote.ReadEntry(ctx, filepath.Join(target.Root, filepath.FromSlash(w.Path)))
	if err != nil {
		return unverified(err)
	}
	opposite, err := dialOpposite(ctx, directory, o.session, target)
	if err != nil {
		return unverified(err)
	}
	remoteEntry, err := remote.Fetch(ctx, opposite, w.Path)
	if err != nil {
		return unverified(err)
	}
	// Executability isn't compared, since not every filesystem preserves it.
	if !remote.SameContents(localEntry, result) || !remote.SameContents(remoteEntry, result) {
		return errors.New("updated both copies, but after synchronizing they don't both have the result; retry to apply it again")
	}
	if err := os.RemoveAll(w.directory); err != nil {
		return fmt.Errorf("resolved, but unable to remove workspace: %w", err)
	} else if o.json {
		return json.NewEncoder(output).Encode(map[string]bool{"Resolved": true})
	}
	_, err = fmt.Fprintln(output, "Resolved; both copies now match the result.")
	return err
}
