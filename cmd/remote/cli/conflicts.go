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
// applied whose conflict is no longer listed (see workspace.applied). The
// conflict may also have been ended by a change after checkout, which may have
// left one side's changes only in the workspace.
var errResolvedOtherwise = errors.New("is no longer listed as a conflict, and nothing was applied, so it was resolved some other way, or ended by a change after checkout, which may have left one side's changes only in the workspace; apply changed neither copy: compare the current file with the workspace's local and remote files before discarding it")

// errEmptyMerged indicates that apply refused an empty merged file although
// neither copy is empty. Such a file has no markers left to refuse, but it was
// most likely truncated rather than resolved, such as by a command that reads
// it and has its output redirected to it, and applying it would replace both
// copies with nothing.
var errEmptyMerged = errors.New("the merged file is empty, but neither copy is, so it was probably truncated, such as by redirecting a command's output to the file that it reads")

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
	flags.BoolVar(&o.conflicts, "conflicts", false, "Sync and list conflicts and checkout state")
	flags.BoolVar(&o.checkout, "conflict-checkout", false, "Download copies into a merge workspace")
	flags.BoolVar(&o.apply, "conflict-apply", false, "Apply chosen or merged copies and verify")
	flags.BoolVar(&o.discard, "conflict-discard", false, "Set aside workspace; leave copies alone")
	flags.StringVar(&o.take, "take", "", "Take `SIDE` (local/remote); auto-checkout")
	flags.BoolVar(&o.delete, "delete", false, "Delete both checked-out copies")
	flags.StringVar(&o.executable, "executable", "", "Set result executability (`BOOL`)")
	flags.StringVar(&o.base, "base", "", "Use `FILE` as a verified merge ancestor")
	flags.BoolVar(&o.json, "json", false, "Print conflict results as JSON")
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
		len(o.environment) != 0 || len(o.environmentFiles) != 0 || o.noPrologue ||
		o.script != "" || o.detach || o.log != "") {
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
	// Token identifies the workspace's claim of its checkout (see
	// remote.ClaimCheckout) and labels conflict markers in the merged file.
	Token string
	// Local and Remote describe the checked-out copies (see remote.Snapshot).
	Local, Remote *remote.Entry
	// Take lists the copies that can be taken, for conflicts other than
	// between two files.
	Take []string `json:",omitempty"`
	// BaseSource describes where the copies' common ancestor in the
	// workspace was found, if it was.
	BaseSource string `json:",omitempty"`
	// ListedLocal and ListedRemote are the digests of the local and remote
	// copies of a conflict between two files, as listed when it was checked
	// out, which checkout verified the downloaded copies to have, if it could.
	// Listings compare them with the copies' current digests (see
	// changedSince).
	ListedLocal  string `json:",omitempty"`
	ListedRemote string `json:",omitempty"`
	// Applied describes the results (files, symbolic links, or deletions) that
	// apply has written (or may have written) to either copy, which retries
	// may supersede, since they're the workspace's own, even if the merged
	// file has changed since.
	Applied []*remote.Entry `json:",omitempty"`
	// directory is the workspace directory.
	directory string
	// merge describes how checkout prepared the merged file, which it
	// describes (see prepareMerged). It isn't saved.
	merge mergeOutcome
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

// changedSince describes the copies of a checked-out conflict between two files
// that changed since checkout, according to their digests in a later listing
// of the conflict, or returns an empty string if neither did. Changes are only
// detected while nothing was applied, since apply changes the copies itself,
// and if checkout verified the digests of the copies that it downloaded.
// Copies that are no longer both files have changed, but their listing has no
// digests that would identify which one did.
func (w *workspace) changedSince(conflict *remote.ConflictRoot) string {
	if w.applied() || w.ListedLocal == "" || w.ListedRemote == "" {
		return ""
	} else if !conflict.Files {
		return "copies"
	}
	local, opposite := conflict.AlphaDigest, conflict.BetaDigest
	if !w.Alpha {
		local, opposite = opposite, local
	}
	switch localChanged, remoteChanged := local != w.ListedLocal, opposite != w.ListedRemote; {
	case localChanged && remoteChanged:
		return "local and remote copies"
	case localChanged:
		return "local copy"
	case remoteChanged:
		return "remote copy"
	}
	return ""
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

// unreadableWorkspace indicates that the workspace for a local path exists but
// can't be read, such as because its metadata was modified or removed. Neither
// checkout nor apply can use it, so it can only be discarded (see discard).
// Whether anything was applied through it is unknown, so the copies are to be
// checked against its files if anything was.
type unreadableWorkspace struct {
	// directory is the workspace directory.
	directory string
	// err describes why it can't be read.
	err error
}

func (e *unreadableWorkspace) Error() string {
	return fmt.Sprintf("unable to read the workspace in %s (%v); discard it, which changes neither copy, and if anything was applied through it, check the copies against its files, which discard keeps", e.directory, e.err)
}

// loadWorkspace loads the workspace for a local path, failing with an error
// wrapping os.ErrNotExist if there's none, or with an unreadableWorkspace if
// it can't be read. A workspace directory without metadata is unreadable
// rather than missing, since checkouts create workspaces complete, and a new
// checkout couldn't replace it.
func loadWorkspace(local string) (*workspace, error) {
	_, directory, err := workspaceDirectory(local)
	if err != nil {
		return nil, err
	}
	w, err := readWorkspace(directory)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(directory); statErr != nil {
			return nil, err
		}
		err = errors.New("missing metadata")
	}
	if err != nil {
		return nil, &unreadableWorkspace{directory: directory, err: err}
	}
	return w, nil
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

// listConflicts synchronizes the session, showing the synchronization's
// progress on errorOutput if enabled (see newProgressOutput), and lists all of
// its conflicts.
func listConflicts(ctx context.Context, directory string, o *options, errorOutput io.Writer) (*remote.Listing, *remote.Target, error) {
	output := newProgressOutput(errorOutput, o.progress)
	header := &remote.Submission{Operation: remote.OperationConflicts, Progress: output.writer != nil}
	connection, target, err := remote.Submit(ctx, directory, o.session, header)
	if err != nil {
		return nil, nil, err
	}
	display := newProgressDisplay(output, target)
	listing, err := remote.ListConflictsWithProgress(ctx, connection, display.begin())
	display.finish(ctx, err)
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

// dialSession submits an operation through the endpoint that the specified
// target identifies, which must be the one located for the submission.
func dialSession(ctx context.Context, directory, session string, operation remote.Operation, expected *remote.Target) (io.ReadWriteCloser, error) {
	connection, target, err := remote.Dial(ctx, directory, session, operation)
	if err != nil {
		return nil, err
	} else if target.Session != expected.Session || target.Alpha != expected.Alpha {
		connection.Close()
		return nil, errors.New("the session was located differently while resolving the conflict")
	}
	return connection, nil
}

// dialOpposite opens a route to the opposite endpoint, which must be the one
// that the specified target identifies.
func dialOpposite(ctx context.Context, directory, session string, expected *remote.Target) (io.ReadWriteCloser, error) {
	return dialSession(ctx, directory, session, remote.OperationExecute, expected)
}

// claimSequence records with the daemon that a conflict root is checked out
// through the endpoint that the specified target identifies, under a token
// identifying the claim, failing if the opposite endpoint holds its checkout (see
// remote.ClaimCheckout). Each conflict is checked out through a single
// endpoint, which alone can apply or discard its resolution, since workspaces
// are kept on the system that checked them out, where the opposite endpoint
// can't see them.
//
// Claims are only made while the local endpoint's workspace for the conflict
// root is locked, by the checkout creating the workspace or through the
// workspace itself, so a checkout that the endpoint holds under another token
// has no workspace. It's abandoned, such as one left by an interrupted
// checkout or recorded by a claim that arrived late, and claims leave it as it
// is, so it's released by its token before claiming the checkout again, which
// fails if the opposite endpoint claims it meanwhile. A checkout recorded
// without a token, by a build that didn't issue tokens, takes the claim's
// token instead, since it may belong to the workspace.
//
// It returns the claim's sequence number, which orders the replacements an apply
// makes after claiming its checkout (see remote.Replacement.Sequence) after
// those of all earlier applies of the conflict, including those whose clients
// stopped waiting for them while the replacements kept running.
func claimSequence(ctx context.Context, directory, session string, target *remote.Target, path, token string) (uint64, error) {
	claimOnce := func() (uint64, string, error) {
		connection, err := dialSession(ctx, directory, session, remote.OperationClaim, target)
		if err != nil {
			return 0, "", err
		}
		return remote.ClaimCheckout(ctx, connection, path, token)
	}
	sequence, held, err := claimOnce()
	if err != nil || sequence != 0 {
		return sequence, err
	}
	connection, err := dialSession(ctx, directory, session, remote.OperationRelease, target)
	if err == nil {
		_, _, err = remote.ReleaseCheckout(ctx, connection, path, held)
	}
	if err != nil {
		return 0, fmt.Errorf("unable to release an abandoned checkout: %w", err)
	} else if sequence, _, err = claimOnce(); err != nil {
		return 0, err
	} else if sequence == 0 {
		return 0, errors.New("the abandoned checkout was claimed again while it was being released")
	}
	return sequence, nil
}

// release removes the checkout of a conflict root held by the endpoint that
// the specified target identifies, if any, and returns whether it held one
// (see releaseOver).
func release(ctx context.Context, directory, session string, target *remote.Target, path, token string) (bool, error) {
	connection, err := dialSession(ctx, directory, session, remote.OperationRelease, target)
	if err != nil {
		return false, err
	}
	return releaseOver(ctx, connection, directory, session, target, path, token)
}

// releaseOver removes the checkout of a conflict root held by the endpoint
// that the specified target identifies, if any, over a release submission
// through it, and returns whether it held one. Checkouts are only released
// once the local endpoint has no workspace for the conflict root, which is
// locked meanwhile, so any checkout that the endpoint holds is abandoned.
//
// Releases still name the token of the claim that they end (see
// remote.ReleaseCheckout), starting with the specified one, which is empty if
// it's unknown: an operation that stops waiting for a release, such as on
// timeout, unlocks its workspace while the release may still arrive, and the
// token keeps it from removing the checkout of a later claim, which has
// another token. A claim can arrive late as well and, if the endpoint holds no
// checkout, record one without a workspace. A checkout with another token is
// thus abandoned too, since the local endpoint has no workspace for the
// conflict root, so it's released by that token, and one recorded after the
// release is released by listing conflicts, discarding, or checking the
// conflict out again (see claimSequence). That includes a checkout without a token,
// recorded by a build that didn't issue tokens, which a release without one
// removes.
func releaseOver(ctx context.Context, connection io.ReadWriteCloser, directory, session string, target *remote.Target, path, token string) (bool, error) {
	held, current, err := remote.ReleaseCheckout(ctx, connection, path, token)
	if err != nil || !held || current == token {
		return held, err
	}
	connection, err = dialSession(ctx, directory, session, remote.OperationRelease, target)
	if err != nil {
		return true, err
	} else if stillHeld, remaining, err := remote.ReleaseCheckout(ctx, connection, path, current); err != nil {
		return true, err
	} else if stillHeld && remaining != current {
		return true, errors.New("the checkout was claimed again while it was being released")
	}
	return true, nil
}

// releaseAbandoned releases the checkouts held by the local endpoint that have
// no workspace, such as those left by interrupted checkouts or by releases
// that failed, so that the opposite endpoint can check them out, and returns
// the remaining checkouts. Checkouts of paths with an operation in progress,
// such as a checkout that hasn't created its workspace yet, are kept, as are
// those whose workspaces can't be read, which discarding releases, and those
// that can't be released, since the listing is advisory.
func releaseAbandoned(ctx context.Context, directory string, target *remote.Target, checkouts []remote.Checkout, workspaces map[string]*workspace) []remote.Checkout {
	var remaining []remote.Checkout
	for _, checkout := range checkouts {
		if checkout.Alpha != target.Alpha || workspaces[checkout.Path] != nil || !releaseIfAbandoned(ctx, directory, target, checkout) {
			remaining = append(remaining, checkout)
		}
	}
	return remaining
}

// releaseIfAbandoned releases a listed checkout held by the local endpoint if
// it has no workspace and no operation on its path is in progress, and returns
// whether the local endpoint no longer holds it.
func releaseIfAbandoned(ctx context.Context, directory string, target *remote.Target, checkout remote.Checkout) bool {
	local := filepath.Join(target.Root, filepath.FromSlash(checkout.Path))
	unlock, err := lockWorkspace(local)
	if err != nil {
		return false
	}
	defer unlock()
	if _, err := loadWorkspace(local); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	_, err = release(ctx, directory, target.Session, target, checkout.Path, checkout.Token)
	return err == nil
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
// data corrupted in transit. Extracted files are private.
func extractCopy(ctx context.Context, path string, archive func(io.Writer) (*remote.Entry, error)) (*remote.Entry, error) {
	return extractVerified(ctx, path, remote.ExtractArchive, archive)
}

// extractVerified implements extractCopy, extracting the archive with the
// specified function (see remote.ExtractArchive).
func extractVerified(ctx context.Context, path string,
	extract func(context.Context, io.Reader, string) (*remote.Entry, error),
	archive func(io.Writer) (*remote.Entry, error),
) (*remote.Entry, error) {
	reader, writer := io.Pipe()
	type extraction struct {
		entry *remote.Entry
		err   error
	}
	extracted := make(chan extraction, 1)
	go func() {
		entry, err := extract(ctx, reader, path)
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
	// Resolved indicates that apply resolved the conflict.
	Resolved bool `json:",omitempty"`
	// Discarded indicates that discard set the workspace aside.
	Discarded bool `json:",omitempty"`
	// Applied indicates that results were applied through a discarded
	// workspace, so that a copy may have one, or one original copy may exist
	// only in the workspace (see appliedNote).
	Applied bool `json:",omitempty"`
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

// appliedNote warns that a workspace through which results were applied, which
// discard set aside in a backup, belonged to an unfinished resolution: a copy
// may still have a result, or synchronization may have copied one original
// copy over a partially applied result (a deletion, or a result matching the
// copies' common ancestor), so that the other original copy exists only in the
// backup.
func appliedNote(backup string) string {
	return fmt.Sprintf("Results were applied through it, so a copy may still have one, or synchronization may have copied one original copy over the other, which then exists only in the workspace's local or remote file: compare the current file with %s and %s.",
		filepath.Join(backup, localName), filepath.Join(backup, remoteName))
}

// runConflicts performs a conflict operation. Failures for individual paths
// are printed as they occur and reported with errReported, while other errors
// are returned.
func runConflicts(ctx context.Context, directory string, o *options, output, errorOutput io.Writer) error {
	pruneWorkspaces()
	if o.conflicts {
		return printConflicts(ctx, directory, o, output, errorOutput)
	}
	p := &printer{output: output, errors: errorOutput, json: o.json, headings: len(o.arguments) > 1}
	var err error
	if o.checkout {
		err = checkoutAll(ctx, directory, o, p)
	} else if o.apply {
		err = applyAll(ctx, directory, o, p)
	} else {
		for _, argument := range o.arguments {
			result, err := discard(ctx, o, argument)
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
	// LocalChanges and RemoteChanges describe changes from the submitting
	// endpoint's perspective, with paths mapped in the same way as Path.
	LocalChanges  []remote.Change
	RemoteChanges []remote.Change
	// ExcludedChanges counts changes omitted from this conflict's summary.
	ExcludedChanges uint64 `json:",omitempty"`
	// Resolvable indicates whether remote can resolve the conflict.
	Resolvable bool
	// Files indicates whether both copies are files, which can be merged.
	Files bool `json:",omitempty"`
	// Take lists the copies that can be taken.
	Take []string `json:",omitempty"`
	// CheckedOut identifies the endpoint through which the conflict is checked
	// out, if it is: local, or remote for the opposite endpoint, which alone
	// can apply or discard it.
	CheckedOut string `json:",omitempty"`
	// Changed indicates that a copy of a conflict checked out locally changed
	// since checkout, so that its workspace must be discarded and checked out
	// again.
	Changed bool `json:",omitempty"`
	// Applied indicates that apply has changed (or may have changed) either
	// copy through the local workspace of a conflict checked out locally (see
	// workspace.applied), so that its resolution is unfinished and is finished
	// by applying again, since a copy may already have the result.
	Applied bool `json:",omitempty"`
	// changed describes the copies that changed (see workspace.changedSince).
	changed string
}

// pendingResolution is a workspace as listed by --conflicts --json whose
// conflict is no longer listed, along with its location. If something was
// applied, its resolution is unfinished, and it's finished by applying again.
// Otherwise, the conflict was resolved some other way, or ended by a change
// after checkout, such as one copy reverted or deleted, so that
// synchronization copied the other copy over it, which can't be told apart.
// Apply refuses it, and it's set aside by discarding it, but it may hold the
// only copy of one side's changes, so its copies are compared with the current
// file first.
type pendingResolution struct {
	// Path is the local path of the conflict root.
	Path string
	// Applied indicates whether apply has changed (or may have changed) either
	// copy through the workspace (see workspace.applied).
	Applied bool `json:",omitempty"`
	// Workspace is the workspace directory, which contains the checked-out
	// copies (see localName and remoteName).
	Workspace string
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

// printConflicts lists conflicts, the endpoint through which each one is
// checked out, if any, whether a local checkout's copies changed since checkout
// or results were applied through its workspace, and the workspaces whose
// conflicts are no longer listed, with their locations: unfinished
// resolutions, whose conflicts may no longer be listed if synchronization
// propagated one copy over a partially applied resolution, and workspaces with
// nothing applied, whose conflicts were resolved some other way or ended by a
// change after checkout, which may have left one side's changes only in the
// workspace. Changes since checkout can't be identified once a conflict is no
// longer listed, so the listing says to compare the current file with the
// workspace's copies before discarding it. It also lists the paths whose
// conflicts are no longer listed but which the opposite endpoint has checked
// out, possibly for unfinished resolutions, and releases abandoned local
// checkouts (see releaseAbandoned). Paths are local paths, which are relative
// to the working directory if it's inside the root, as other operations
// expect.
func printConflicts(ctx context.Context, directory string, o *options, output, errorOutput io.Writer) error {
	listing, target, err := listConflicts(ctx, directory, o, errorOutput)
	if err != nil {
		return err
	}
	warnOverlapping(errorOutput, target)
	l := newLocator(target)
	workspaces := sessionWorkspaces(target)
	holders := make(map[string]bool)
	for _, checkout := range releaseAbandoned(ctx, directory, target, listing.Checkouts, workspaces) {
		holders[checkout.Path] = checkout.Alpha
	}
	// A checkout held by the opposite endpoint prevails over a local workspace,
	// which apply then refuses.
	checkedOut := func(path string) string {
		if alpha, held := holders[path]; held && alpha != target.Alpha {
			return sideRemote
		} else if held || workspaces[path] != nil {
			return sideLocal
		}
		return ""
	}
	conflicts := make([]listedConflict, len(listing.Conflicts))
	reports := newReporter(errorOutput, target)
	for c := range listing.Conflicts {
		conflict := &listing.Conflicts[c]
		localChanges, remoteChanges := reports.sides(conflict.Changes())
		localize := func(changes []remote.Change) []remote.Change {
			result := slices.Clone(changes)
			for c := range result {
				result[c].Path = l.local(result[c].Path)
			}
			return result
		}
		listed := listedConflict{
			Path: l.local(conflict.Path), Resolvable: conflict.Resolvable(), Files: conflict.Files,
			Take: localSides(conflict, target), CheckedOut: checkedOut(conflict.Path),
			LocalChanges: localize(localChanges), RemoteChanges: localize(remoteChanges),
			ExcludedChanges: conflict.ExcludedChanges,
		}
		// Copies changed since checkout are only identified while nothing was
		// applied (see workspace.changedSince), so a workspace through which
		// results were applied is marked as such instead, since its resolution
		// is unfinished. Neither is marked for a conflict that the opposite
		// endpoint has checked out, whose workspace apply refuses.
		if w := workspaces[conflict.Path]; w != nil && listed.CheckedOut == sideLocal {
			listed.changed = w.changedSince(conflict)
			listed.Changed = listed.changed != ""
			listed.Applied = w.applied()
		}
		conflicts[c] = listed
		delete(workspaces, conflict.Path)
		delete(holders, conflict.Path)
	}
	var pending, elsewhere []string
	for path := range workspaces {
		pending = append(pending, path)
	}
	slices.Sort(pending)
	for path, alpha := range holders {
		if alpha != target.Alpha {
			elsewhere = append(elsewhere, path)
		}
	}
	slices.Sort(elsewhere)
	if o.json {
		listed := struct {
			Conflicts     []listedConflict
			Pending       []pendingResolution
			RemotePending []string
		}{Conflicts: conflicts, Pending: make([]pendingResolution, len(pending)), RemotePending: make([]string, len(elsewhere))}
		for p, path := range pending {
			w := workspaces[path]
			listed.Pending[p] = pendingResolution{Path: l.local(path), Applied: w.applied(), Workspace: w.directory}
		}
		for p, path := range elsewhere {
			listed.RemotePending[p] = l.local(path)
		}
		return json.NewEncoder(output).Encode(&listed)
	} else if len(conflicts) == 0 && len(pending) == 0 && len(elsewhere) == 0 {
		_, err := fmt.Fprintln(output, "No conflicts.")
		return err
	}
	for c, conflict := range conflicts {
		line := reports.describeConflict(listing.Conflicts[c].Changes())
		if !conflict.Resolvable {
			line += " (remote can't resolve it, since it involves a directory, the synchronization root, or content that can't be synchronized or read)"
		} else if !conflict.Files {
			line += fmt.Sprintf(" (not two files, so it can only be resolved with --take %s)", strings.Join(conflict.Take, " or --take "))
		}
		switch {
		case conflict.Changed:
			line += fmt.Sprintf(" (checked out, but the %s changed since: discard and check out again)", conflict.changed)
		case conflict.Applied:
			line += " (checked out and partially applied: apply again to finish)"
		case conflict.CheckedOut == sideLocal:
			line += " (checked out)"
		case conflict.CheckedOut == sideRemote:
			line += " (checked out through the opposite endpoint, which alone can apply or discard it)"
		}
		if _, err := fmt.Fprintln(output, line); err != nil {
			return err
		}
	}
	// Workspaces whose conflicts are no longer listed are listed with their
	// locations, since they may hold the only copy of one side's changes.
	var unfinished, unapplied, remotely []string
	for _, path := range pending {
		w := workspaces[path]
		line := fmt.Sprintf("%s (workspace: %s)", l.display(path), w.directory)
		if w.applied() {
			unfinished = append(unfinished, line)
		} else {
			unapplied = append(unapplied, line)
		}
	}
	for _, path := range elsewhere {
		remotely = append(remotely, l.display(path))
	}
	for _, section := range []struct {
		heading string
		lines   []string
	}{
		{"Partially applied, but no longer listed as conflicts (synchronization may have copied an original copy over a partial result; apply again to finish resolving):", unfinished},
		{fmt.Sprintf("Checked out, but no longer listed as conflicts, with nothing applied (resolved some other way, or ended by a change after checkout, which may have left one side's changes only in the workspace; compare the current file with the workspace's local and remote files before discarding, since discarded workspaces are deleted after %d days):",
			int(backupRetention.Hours()/24)), unapplied},
		{"Checked out through the opposite endpoint, but no longer listed as conflicts (possibly unfinished; leave them unchanged until applied or discarded there):", remotely},
	} {
		if len(section.lines) == 0 {
			continue
		}
		fmt.Fprintln(output, section.heading)
		for _, line := range section.lines {
			if _, err := fmt.Fprintln(output, "  "+line); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkoutAll checks out the specified conflicts after listing them.
func checkoutAll(ctx context.Context, directory string, o *options, p *printer) error {
	listing, target, err := listConflicts(ctx, directory, o, p.errors)
	if err != nil {
		return err
	}
	warnOverlapping(p.errors, target)
	for _, argument := range o.arguments {
		result, err := checkout(ctx, directory, o, listing, target, argument)
		p.print(argument, result, err)
	}
	return nil
}

// checkedOut explains why a conflict with a workspace can't be checked out
// again: each conflict has a single workspace, on either endpoint, until it's
// applied or discarded, so that the merged file's edits are never replaced and
// resolutions never race.
//
// A workspace through which results were applied is finished by applying
// again, whether or not its conflict is still listed, rather than discarded,
// since a copy may already have a result, or synchronization may have copied
// one original copy over a partially applied result. A workspace with nothing
// applied whose conflict is no longer listed may hold the only copy of one
// side's changes, if a change ended the conflict, so the current file is
// compared with its copies before discarding it. Otherwise, the workspace may
// be another agent's or the user's resolution in progress, since checkouts
// aren't tied to whoever made them, and a checkout after discarding it would
// reuse its files, so it's only applied or discarded by whoever checked it
// out.
func checkedOut(w *workspace, conflict *remote.ConflictRoot, target *remote.Target, path string) error {
	if !w.matches(target, path) {
		return errors.New("an unrelated workspace exists for this path; discard it first")
	}
	message := "is already checked out in " + w.directory
	if w.hasDirectory() {
		return fmt.Errorf("%s, but %w (discard the workspace, which changes neither copy)", message, errDirectoryConflict)
	} else if conflict == nil && w.applied() {
		return fmt.Errorf("%s, and results were applied through it, but the conflict is no longer listed, since synchronization may have copied an original copy over a partial result: apply again to finish resolving it", message)
	} else if conflict == nil {
		return fmt.Errorf("%s, but the conflict is no longer listed, and nothing was applied, so it was resolved some other way, or ended by a change after checkout, which may have left one side's changes only in the workspace: compare the current file with the workspace's local and remote files before discarding it (which changes neither copy, and discarded workspaces are deleted after %d days)",
			message, int(backupRetention.Hours()/24))
	} else if w.applied() {
		return fmt.Errorf("%s, and a result was partially applied through it: apply again to finish resolving it", message)
	} else if changed := w.changedSince(conflict); changed != "" {
		return fmt.Errorf("%s, but the %s changed since checkout: discard the workspace (which changes neither copy) and check out again", message, changed)
	}
	return fmt.Errorf("%s, possibly by another agent or the user, whose resolution may be in progress: if you checked it out, apply it, or discard the workspace (which changes neither copy) to check out again; otherwise, leave it, or ask the user", message)
}

// checkout downloads both copies of a conflict into a new workspace. A
// conflict that's already checked out, through either endpoint, can't be
// checked out again until its workspace is applied or discarded.
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
	w, _, err := checkoutLocked(ctx, directory, o, listing, target, local)
	if err != nil {
		return nil, err
	}
	return describe(w, argument)
}

// checkoutLocked prepares a checkout while the caller holds the local path's
// workspace lock. It returns the claim's sequence number so that an automatic
// checkout can apply its result under that same claim, without another round
// trip. A non-nil workspace on failure has been published and is kept for retry.
func checkoutLocked(ctx context.Context, directory string, o *options, listing *remote.Listing, target *remote.Target, local string) (*workspace, uint64, error) {
	path, err := newLocator(target).resolve(local)
	if err != nil {
		return nil, 0, err
	}
	conflict := findConflict(listing, path)
	if w, err := loadWorkspace(local); err == nil {
		return nil, 0, checkedOut(w, conflict, target, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	} else if conflict == nil {
		return nil, 0, errors.New("isn't a conflict root (see remote --conflicts)")
	} else if !conflict.Resolvable() {
		return nil, 0, errors.New("isn't a conflict that remote can resolve (see remote --conflicts)")
	} else if o.base != "" && !conflict.Files {
		return nil, 0, errors.New("--base requires a conflict between two files")
	}

	// Claim the checkout before downloading the copies, so that the opposite
	// endpoint can't check the conflict out meanwhile, and release it if
	// checkout fails. The claim has a new token, which the workspace keeps, so
	// that late claims and releases of earlier checkouts can't take over or
	// remove its checkout. A checkout that the local endpoint already holds
	// without a workspace, such as one left by an interrupted checkout, is
	// released first (see claimSequence).
	token := rand.Text()[:8]
	sequence, err := claimSequence(ctx, directory, o.session, target, path, token)
	if err != nil {
		return nil, 0, err
	}
	created := false
	defer func() {
		if !created {
			release(ctx, directory, o.session, target, path, token)
		}
	}()
	base, final, err := workspaceDirectory(local)
	if err != nil {
		return nil, 0, err
	} else if err := outside(base, target.Root); err != nil {
		return nil, 0, err
	}

	// Prepare the workspace in a temporary directory and move it into place
	// once complete, so that interrupted checkouts leave no workspace.
	staging, err := os.MkdirTemp(base, stagingPrefix)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to create workspace: %w", err)
	}
	defer os.RemoveAll(staging)
	w := &workspace{Session: target.Session, Alpha: target.Alpha, Path: path, Token: token, directory: staging}
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
		return nil, 0, err
	} else if err := w.save(); err != nil {
		return nil, 0, fmt.Errorf("unable to save workspace: %w", err)
	} else if err := os.Rename(staging, final); err != nil {
		return nil, 0, fmt.Errorf("unable to save workspace: %w", err)
	}
	created = true
	w.directory = final
	// The workspace's files and directory were flushed (see workspace.save),
	// so flush the workspaces directory that it was moved into, and that
	// directory's parent, in case the workspaces directory was just created,
	// so that the copies survive a power loss or an operating system crash
	// once checkout reports them.
	if err := errors.Join(remote.SyncDirectory(base), remote.SyncDirectory(filepath.Dir(base))); err != nil {
		return w, sequence, fmt.Errorf("unable to save workspace: %w", err)
	}
	return w, sequence, nil
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
	if unverifiable == nil {
		w.ListedLocal, w.ListedRemote = listed[0], listed[1]
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

// describe describes a workspace for its checkout.
func describe(w *workspace, argument string) (*outcome, error) {
	result := &outcome{Path: argument, Local: w.file(localName), Remote: w.file(remoteName), Files: w.files(), Take: w.Take}
	note := func(format string, arguments ...any) {
		result.lines = append(result.lines, fmt.Sprintf(format, arguments...))
	}
	if !w.files() {
		note("Local:  %s (%s)", result.Local, describeKind(w.Local))
		note("Remote: %s (%s)", result.Remote, describeKind(w.Remote))
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
	if _, err := os.Lstat(result.Merged); errors.Is(err, os.ErrNotExist) {
		result.Binary = true
		note("The copies are binary, so no merged file was created: create it, or use --take.")
	} else if w.Local.Digest != w.Remote.Digest {
		regions, err := countMarkers(result.Merged, markersFor(w.Token).local)
		if err != nil {
			return nil, err
		}
		result.Regions = regions
		note("%s", mergeNote(w, regions))
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
// files can only be resolved by taking one of them as checked out. A merged
// file is refused if it still contains conflict markers, or if it's empty
// while neither copy is (see errEmptyMerged).
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
		} else if result.Size == 0 && w.Local.Size != 0 && w.Remote.Size != 0 {
			// Checking out again recreates the merged file, but discarding a
			// workspace through which a result was applied would leave that
			// resolution unfinished (see workspace.applied).
			recreate := "recreate it from the workspace's other files"
			if !w.applied() {
				recreate += " (or discard the workspace and check it out again, which recreates it)"
			}
			return nil, "", fmt.Errorf("%w; apply changed neither copy: %s, or, if the result should be empty, discard the workspace and empty both copies", errEmptyMerged, recreate)
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
	// Pin the session once selected, including before automatically checking
	// out multiple paths. Each workspace also checks the submitting endpoint.
	options := *o
	o = &options
	var preliminary *remote.Listing
	var preliminaryTarget, selected *remote.Target
	var preliminaryErr error
	listed := func() (*remote.Listing, *remote.Target, error) {
		if preliminary == nil && preliminaryErr == nil {
			preliminary, preliminaryTarget, preliminaryErr = listConflicts(ctx, directory, o, p.errors)
			if preliminaryErr == nil {
				if selected != nil && (selected.Session != preliminaryTarget.Session || selected.Alpha != preliminaryTarget.Alpha) {
					preliminaryErr = errors.New("the session was located differently while resolving the conflict")
					return nil, nil, preliminaryErr
				}
				selected = preliminaryTarget
				o.session = preliminaryTarget.Session
				warnOverlapping(p.errors, preliminaryTarget)
			}
		}
		return preliminary, preliminaryTarget, preliminaryErr
	}
	var applications []*application
	seen := make(map[string]bool)
	for _, argument := range o.arguments {
		// POSIX record locks don't exclude locks in the same process, and
		// closing another descriptor would release the first lock. Aliases of
		// an earlier argument must therefore be rejected before locking them.
		local, err := filepath.Abs(argument)
		var key string
		if err == nil {
			_, key, err = workspaceDirectory(local)
		}
		if err != nil {
			p.print(argument, nil, err)
			continue
		} else if seen[key] {
			p.print(argument, nil, errors.New("this path was already specified"))
			continue
		}
		seen[key] = true
		a, unlock, err := apply(ctx, directory, o, argument, selected, listed)
		if unlock != nil {
			unlocks = append(unlocks, unlock)
		}
		if a != nil {
			selected = a.target
			o.session = selected.Session
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
	listing, target, listErr := listConflicts(ctx, directory, o, p.errors)
	for _, a := range applications {
		err := listErr
		if err == nil && (target.Session != a.target.Session || target.Alpha != a.target.Alpha) {
			err = errors.New("the session was located differently while resolving the conflict")
		}
		if err != nil {
			p.print(a.argument, nil, fmt.Errorf("updated the copies, but unable to verify synchronization (retry to verify): %w", err))
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
func apply(ctx context.Context, directory string, o *options, argument string, expected *remote.Target, listed func() (*remote.Listing, *remote.Target, error)) (_ *application, _ func(), resultErr error) {
	local, err := filepath.Abs(argument)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to locate it: %w", err)
	}
	unlock, err := lockWorkspace(local)
	if err != nil {
		return nil, nil, err
	}
	w, err := loadWorkspace(local)
	var sequence uint64
	if errors.Is(err, os.ErrNotExist) && (o.take == sideLocal || o.take == sideRemote) {
		// Hold this lock from checking for a workspace through claim,
		// checkout, both replacements, and final verification. In particular,
		// don't call checkout, which would try to take this lock again.
		listing, target, listErr := listed()
		if listErr != nil {
			return nil, unlock, fmt.Errorf("unable to synchronize before applying: %w", listErr)
		}
		w, sequence, err = checkoutLocked(ctx, directory, o, listing, target, local)
		if w != nil {
			defer func() {
				if resultErr != nil {
					resultErr = fmt.Errorf("%w; the automatic checkout is kept in %s: address this error, then retry apply (see remote --help conflicts)", resultErr, w.directory)
				}
			}()
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return nil, unlock, errors.New("isn't checked out (see remote --conflict-checkout)")
	}
	if err != nil {
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
	if expected != nil && (target.Session != expected.Session || target.Alpha != expected.Alpha) {
		connection.Close()
		return nil, unlock, errors.New("the session was located differently while resolving the conflict")
	} else if path, err := a.locator.resolve(local); err != nil || !w.matches(target, path) {
		connection.Close()
		return nil, unlock, errors.New("the workspace was checked out through a different session or endpoint")
	} else if !w.applied() {
		if listing, listedTarget, err := listed(); err != nil {
			connection.Close()
			return nil, unlock, fmt.Errorf("unable to synchronize before applying: %w", err)
		} else if listedTarget.Session != target.Session || listedTarget.Alpha != target.Alpha {
			connection.Close()
			return nil, unlock, errors.New("the session was located differently while resolving the conflict")
		} else if findConflict(listing, w.Path) == nil {
			connection.Close()
			return nil, unlock, errResolvedOtherwise
		}
	}
	// Only the endpoint holding a conflict's checkout can resolve it. A
	// workspace whose checkout is missing claims it again, if the conflict is
	// still listed and the opposite endpoint hasn't checked it out. Each apply
	// claims the checkout again, since the claim's sequence number orders its
	// replacements after those of earlier applies (see write).
	// Automatic checkout already claimed this token and obtained the sequence
	// while holding the same lock. Existing workspaces always claim anew.
	if sequence == 0 {
		sequence, err = claimSequence(ctx, directory, o.session, target, w.Path, w.Token)
	}
	if err != nil {
		connection.Close()
		return nil, unlock, err
	}
	return a, unlock, a.write(ctx, connection, source, sequence)
}

// supersededHint adds advice to an error caused by a later apply of the
// conflict, which replaced a copy (or tried to) first.
func supersededHint(err error) error {
	if errors.Is(err, remote.ErrSuperseded) {
		return fmt.Errorf("%w; list conflicts (remote --conflicts) to see whether it resolved the conflict", err)
	}
	return staleHint(err)
}

// write writes the result to both copies using a connection to the opposite
// endpoint, reading a file result's contents from a workspace file, with the
// sequence number of the apply's claim of the checkout.
//
// An apply whose client stops waiting, such as on timeout, may leave its
// replacement of the remote copy running, and a retry, or another resolution
// of the conflict, may then replace the copy first. The replacements of each
// copy are thus serialized and ordered by the sequence numbers of their
// applies' claims, on the remote copy's system and on this one, which applies
// through either endpoint share, and a replacement whose sequence number is
// earlier than that of one admitted before fails with remote.ErrSuperseded,
// without changing the copy (see remote.ReplaceFile).
func (a *application) write(ctx context.Context, connection io.ReadWriteCloser, source string, sequence uint64) error {
	w := a.w
	open := func() (io.ReadCloser, error) {
		if source == "" {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return os.Open(source)
	}
	expected := append([]*remote.Entry{w.Local, w.Remote}, w.Applied...)
	replacement := &remote.Replacement{Path: w.Path, Expected: expected, Result: a.result, Sequence: sequence}
	// Check the local copy first, so that one changed since checkout doesn't
	// leave only the remote copy updated.
	if err := remote.CheckFile(ctx, a.target.Root, replacement); err != nil {
		connection.Close()
		return fmt.Errorf("unable to update the local copy: %w", staleHint(err))
	}
	// Record the result before writing it, since either write may succeed even
	// if apply fails, so that retries recognize it. A result recorded for this
	// attempt is forgotten if neither copy was written, such as when the remote
	// copy changed since checkout or a later apply reached it first, so that
	// the workspace isn't taken for an unfinished resolution. Failing to forget
	// it is merely conservative.
	recorded := false
	if !slices.ContainsFunc(w.Applied, func(applied *remote.Entry) bool {
		return remote.SameContents(applied, a.result)
	}) {
		w.Applied = append(w.Applied, a.result)
		if err := w.save(); err != nil {
			connection.Close()
			return fmt.Errorf("unable to save workspace: %w", err)
		}
		recorded = true
	}
	forget := func() {
		if recorded {
			w.Applied = w.Applied[:len(w.Applied)-1]
			w.save()
		}
	}
	input, err := open()
	if err != nil {
		connection.Close()
		forget()
		return err
	}
	err = remote.Replace(ctx, connection, replacement, input)
	input.Close()
	if err != nil {
		// Stale and superseded replacements fail before writing (see
		// remote.ReplaceFile).
		if errors.Is(err, remote.ErrStale) || errors.Is(err, remote.ErrSuperseded) {
			forget()
		}
		return fmt.Errorf("unable to update the remote copy: %w", supersededHint(err))
	}
	if input, err = open(); err != nil {
		return err
	}
	err = remote.ReplaceFile(ctx, a.target.Root, replacement, input)
	input.Close()
	if err != nil {
		return fmt.Errorf("updated the remote copy, but unable to update the local copy: %w", supersededHint(err))
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
	lines := []string{resolved, retentionNote(backup)}
	// The checkout is released once the workspace is set aside. One left
	// behind is released by the next listing (see releaseAbandoned).
	if _, err := release(ctx, directory, o.session, a.target, w.Path, w.Token); err != nil {
		lines = append(lines, fmt.Sprintf("Unable to release the checkout (%v); remote --conflicts releases it.", err))
	}
	return &outcome{Path: a.argument, Resolved: true, Backup: backup, lines: lines}, nil
}

// releaseAt releases the checkout of the conflict root at a local path, held
// by the endpoint whose root contains it, and returns whether that endpoint
// held one (see remote.ReleaseCheckout). The endpoint is located by the
// conflict root's parent rather than the working directory, so that it's found
// from outside the root, or from inside the opposite root, even if both
// endpoints are on this system. The parent is used since the conflict root may
// be a symbolic link, which would be resolved, or missing; a conflict at the
// root of a single-file session is outside its parent, so it's located by the
// session alone. If a workspace is specified, which must have been set aside,
// the checkout must be the one that it was checked out through, and it's
// released starting with the workspace's token. Otherwise, the token is
// unknown (see releaseOver).
func releaseAt(ctx context.Context, local, session string, w *workspace) (bool, error) {
	connection, target, err := remote.Dial(ctx, filepath.Dir(local), session, remote.OperationRelease)
	if err != nil {
		return false, err
	}
	path, err := newLocator(target).resolve(local)
	if err == nil && w != nil && !w.matches(target, path) {
		err = errors.New("the workspace was checked out through a different session or endpoint")
	}
	if err != nil {
		connection.Close()
		return false, err
	}
	var token string
	if w != nil {
		token = w.Token
	}
	return releaseOver(ctx, connection, filepath.Dir(local), session, target, path, token)
}

// discard sets a workspace aside without changing either copy and releases its
// checkout, so that the conflict can be checked out again, through either
// endpoint. A checkout without a workspace, such as one left by an interrupted
// checkout or by a discard whose release failed, is released as well, as is
// one whose workspace can't be read, which is set aside regardless.
//
// Leaving the copies unchanged doesn't make discarding a workspace through
// which results were applied inconsequential, since its resolution was
// unfinished (see appliedNote), so discard says so, and where the workspace's
// copies are kept. Whether anything was applied through an unreadable
// workspace is unknown, so the copies are to be checked against its files if
// anything was.
func discard(ctx context.Context, o *options, argument string) (*outcome, error) {
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
	var unreadable *unreadableWorkspace
	if errors.Is(err, os.ErrNotExist) {
		if held, err := releaseAt(ctx, local, o.session, nil); err != nil {
			return nil, err
		} else if !held {
			return nil, errors.New("isn't checked out")
		}
		return &outcome{Path: argument, Discarded: true, lines: []string{
			"Released the checkout, which had no workspace, without changing either copy.",
		}}, nil
	} else if errors.As(err, &unreadable) {
		// An unreadable workspace would otherwise keep its conflict checked out
		// indefinitely. Its session and endpoint are unknown, so its checkout
		// is released like one without a workspace, as a retry would release
		// it once the workspace is set aside.
		backup, err := (&workspace{directory: unreadable.directory}).retire()
		if err != nil {
			return nil, fmt.Errorf("unable to set the workspace aside: %w", err)
		} else if _, err := releaseAt(ctx, local, o.session, nil); err != nil {
			return nil, fmt.Errorf("set the unreadable workspace aside without changing either copy (it's kept in %s; if anything was applied through it, check the copies against its files), but unable to release its checkout, so the opposite endpoint can't check the conflict out yet: %w; discard again to release it", backup, err)
		}
		return &outcome{Path: argument, Discarded: true, Backup: backup, lines: []string{
			"Set aside the workspace, which couldn't be read, without changing either copy.", retentionNote(backup),
			"If anything was applied through it, check the copies against its files.",
		}}, nil
	} else if err != nil {
		return nil, err
	}
	backup, err := w.retire()
	if err != nil {
		return nil, fmt.Errorf("unable to set the workspace aside: %w", err)
	}
	// The workspace's session is selected by its identifier, so that it's
	// found even if --session wasn't specified for a single-file session. A
	// retry would find no workspace, so a failure to release the checkout
	// mentions anything applied through it as well.
	if _, err := releaseAt(ctx, local, w.Session, w); err != nil {
		kept := "it's kept in " + backup
		if w.applied() {
			kept += ", and since results were applied through it, a copy may still have one, or one original copy may exist only in its local or remote file"
		}
		return nil, fmt.Errorf("discarded the workspace without changing either copy (%s), but unable to release its checkout, so the opposite endpoint can't check the conflict out yet: %w; discard again to release it", kept, err)
	}
	lines := []string{"Discarded the workspace without changing either copy.", retentionNote(backup)}
	if w.applied() {
		lines = append(lines, appliedNote(backup))
	}
	return &outcome{Path: argument, Discarded: true, Applied: w.applied(), Backup: backup, lines: lines}, nil
}
