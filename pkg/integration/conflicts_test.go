package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// createConflictSession creates a session between two new roots that submits
// remote commands from the specified endpoint, returning both roots.
func createConflictSession(t *testing.T, submitAlpha bool, mode core.SynchronizationMode) [2]string {
	t.Helper()
	commands := synchronization.RemoteCommandMode_RemoteCommandModeBeta
	if !submitAlpha {
		commands = synchronization.RemoteCommandMode_RemoteCommandModeAlpha
	}
	return createConflictSessionWithCommands(t, commands, mode)
}

// createConflictSessionWithCommands creates a session between two new roots
// with the specified remote command mode, returning both roots.
func createConflictSessionWithCommands(t *testing.T, commands synchronization.RemoteCommandMode, mode core.SynchronizationMode) [2]string {
	t.Helper()
	roots := [2]string{t.TempDir(), t.TempDir()}
	ctx := context.Background()
	id, err := synchronizationManager.Create(ctx, &url.URL{Path: roots[0]}, &url.URL{Protocol: netpipe.Protocol_Netpipe, Path: roots[1]},
		&synchronization.Configuration{SynchronizationMode: mode, WatchMode: synchronization.WatchMode_WatchModeNoWatch, RemoteCommandMode: commands},
		&synchronization.Configuration{}, &synchronization.Configuration{}, "", nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { synchronizationManager.Terminate(ctx, &selection.Selection{Specifications: []string{id}}, "") })
	return roots
}

// conflictListing is the JSON output of remote --conflicts.
type conflictListing struct {
	Conflicts []struct {
		Path       string
		Resolvable bool
		Files      bool
		Take       []string
		CheckedOut string
		Changed    bool
		Applied    bool
	}
	Pending []struct {
		Path      string
		Applied   bool
		Workspace string
	}
	RemotePending []string
}

// conflictOutcome is a line of JSON output of other conflict operations.
type conflictOutcome struct {
	Path, Error, Local, Remote, Base, Merged    string
	Files, Binary, Resolved, Discarded, Applied bool
	Regions                                     int
	Take                                        []string
	Backup                                      string
}

// conflictCommands runs conflict operations in a directory.
type conflictCommands struct {
	t         *testing.T
	directory string
	options   []string
}

// invoke runs the remote command line, which must succeed.
func (c *conflictCommands) invoke(args ...string) string {
	c.t.Helper()
	code, output, errors := runRemote(c.t, c.directory, append(append([]string{"--timeout", "20s"}, c.options...), args...)...)
	if code != 0 {
		c.t.Fatalf("%v: %d: %s %s", args, code, output, errors)
	}
	return output
}

// fail runs the remote command line, which must fail with the expected error.
func (c *conflictCommands) fail(expected string, args ...string) {
	c.t.Helper()
	code, _, errors := runRemote(c.t, c.directory, append(append([]string{"--timeout", "20s"}, c.options...), args...)...)
	if code == 0 || !strings.Contains(errors, expected) {
		c.t.Fatalf("%v: expected failure with %q, got %d: %s", args, expected, code, errors)
	}
}

// list lists conflicts.
func (c *conflictCommands) list() (listing conflictListing) {
	c.t.Helper()
	if err := json.Unmarshal([]byte(c.invoke("--conflicts", "--json")), &listing); err != nil {
		c.t.Fatal(err)
	}
	return
}

// outcomes runs a conflict operation with JSON output, which must succeed,
// and returns its outcomes.
func (c *conflictCommands) outcomes(args ...string) []conflictOutcome {
	c.t.Helper()
	decoder := json.NewDecoder(strings.NewReader(c.invoke(append([]string{"--json"}, args...)...)))
	var outcomes []conflictOutcome
	for {
		var outcome conflictOutcome
		if err := decoder.Decode(&outcome); err == io.EOF {
			return outcomes
		} else if err != nil {
			c.t.Fatal(err)
		}
		outcomes = append(outcomes, outcome)
	}
}

// checkout checks out a conflict.
func (c *conflictCommands) checkout(path string) conflictOutcome {
	c.t.Helper()
	return c.outcomes("--conflict-checkout", path)[0]
}

// writeFile writes a file, which must succeed.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

// readFile reads a file, returning an empty string on failure.
func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestRemoteConflictWorkflow(t *testing.T) {
	for _, submitAlpha := range []bool{true, false} {
		t.Run(fmt.Sprintf("submitAlpha=%t", submitAlpha), func(t *testing.T) {
			roots := createConflictSession(t, submitAlpha, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
			local, opposite := roots[0], roots[1]
			if !submitAlpha {
				local, opposite = opposite, local
			}
			write := func(root, name, data string) {
				t.Helper()
				writeFile(t, filepath.Join(root, name), data)
			}
			read := func(root, name string) string {
				return readFile(filepath.Join(root, name))
			}
			for _, name := range []string{"text", "resolved", "edited", "large"} {
				write(roots[0], name, "base\n")
			}
			write(roots[0], "merge", "1\n2\n3\n4\n5\n")
			if report, err := flushRemote(local); err != nil {
				t.Fatal(err)
			} else if !report.Resolution {
				t.Fatal("resolution not reported as supported")
			}
			for side, root := range roots {
				write(root, "text", fmt.Sprintf("shared\nside%d\n", side))
				write(root, "resolved", fmt.Sprintf("side%d\n", side))
				write(root, "edited", fmt.Sprintf("side%d\n", side))
				write(root, "large", strings.Repeat(fmt.Sprintf("side%d data\n", side), 150000))
			}
			write(local, "merge", "1\nlocal\n3\n4\n5\n")
			write(opposite, "merge", "1\n2\n3\nremote\n5\n")
			// A directory created on one side and a file on the other.
			if err := os.Mkdir(filepath.Join(roots[0], "type"), 0700); err != nil {
				t.Fatal(err)
			}
			write(roots[0], filepath.Join("type", "child"), "child\n")
			write(roots[1], "type", "file\n")
			c := &conflictCommands{t: t, directory: local}

			// Conflicts are listed completely, with local paths and how they
			// can be resolved.
			listing := c.list()
			var paths []string
			for _, conflict := range listing.Conflicts {
				paths = append(paths, conflict.Path)
				if conflict.Path == "type" {
					if conflict.Resolvable || conflict.Files || len(conflict.Take) != 0 {
						t.Fatalf("directory conflict reported as resolvable: %+v", conflict)
					}
				} else if !conflict.Resolvable || !conflict.Files || len(conflict.Take) != 2 {
					t.Fatalf("unexpected resolution: %+v", conflict)
				}
			}
			if !slices.Equal(paths, []string{"edited", "large", "merge", "resolved", "text", "type"}) {
				t.Fatal("unexpected conflicts:", paths)
			}

			// Large copies are downloaded completely, checking out again is
			// refused without changing the workspace, which may be another
			// agent's or the user's resolution in progress, and applying
			// resolves the conflict and keeps the original copies.
			w := c.checkout("large")
			for _, path := range []string{w.Local, w.Remote} {
				if info, err := os.Stat(path); err != nil || info.Size() <= 1024*1024 {
					t.Fatalf("large checkout incomplete: %v", err)
				}
			}
			writeFile(t, w.Merged, "merged\n")
			c.fail("is already checked out in "+filepath.Dir(w.Local)+", possibly by another agent or the user", "--conflict-checkout", "large")
			if readFile(w.Merged) != "merged\n" {
				t.Fatal("repeated checkout overwrote edits")
			}
			applied := c.outcomes("--conflict-apply", "large")[0]
			if !applied.Resolved || read(local, "large") != "merged\n" || read(opposite, "large") != "merged\n" {
				t.Fatal("result not applied to both copies")
			} else if info, err := os.Stat(filepath.Join(applied.Backup, "remote")); err != nil || info.Size() <= 1024*1024 {
				t.Fatal("original copies not kept:", err)
			}
			c.fail("isn't checked out", "--conflict-apply", "large")

			// With their common ancestor, both copies' changes are merged. An
			// existing workspace isn't merged again with it, which would
			// replace its edits, so it must be discarded first.
			base := filepath.Join(t.TempDir(), "base")
			writeFile(t, base, "1\n2\n3\n4\n5\n")
			w = c.checkout("merge")
			writeFile(t, w.Merged, "edited\n")
			c.fail("discard the workspace (which changes neither copy) to check out again", "--conflict-checkout", "--base", base, "merge")
			if readFile(w.Merged) != "edited\n" {
				t.Fatal("checkout with --base changed an existing workspace")
			}
			c.invoke("--conflict-discard", "merge")
			c.fail("isn't the copies' common ancestor", "--conflict-checkout", "--base", filepath.Join(local, "merge"), "merge")
			if w := c.outcomes("--conflict-checkout", "--base", base, "merge")[0]; w.Base == "" || w.Regions != 0 || readFile(w.Merged) != "1\nlocal\n3\nremote\n5\n" {
				t.Fatalf("copies not merged with their common ancestor: %+v", w)
			}
			c.invoke("--conflict-apply", "merge")
			if read(opposite, "merge") != "1\nlocal\n3\nremote\n5\n" {
				t.Fatal("merge not applied")
			}

			// A conflict resolved some other way, such as by copying one copy
			// over the other, is no longer listed. With nothing applied, its
			// workspace is listed as such, with its location, and apply
			// refuses it without changing either copy, since it would replace
			// that resolution. Partially applied resolutions are retried
			// elsewhere (see TestRemoteConflictRetryDeletion), as are
			// conflicts ended by a change (see TestRemoteConflictEndedByChange).
			workspace := filepath.Dir(c.checkout("resolved").Local)
			resolved := read(local, "resolved")
			write(opposite, "resolved", resolved)
			if _, err := flushRemote(local); err != nil {
				t.Fatal(err)
			}
			if listing := c.list(); len(listing.Pending) != 1 || listing.Pending[0].Path != "resolved" ||
				listing.Pending[0].Applied || listing.Pending[0].Workspace != workspace {
				t.Fatalf("workspace with nothing applied not listed: %+v", listing.Pending)
			} else if text := c.invoke("--conflicts"); !strings.Contains(text, "with nothing applied") ||
				!strings.Contains(text, "resolved (workspace: "+workspace+")") {
				t.Fatal("workspace with nothing applied not listed as text:", text)
			}
			c.fail("resolved some other way", "--conflict-checkout", "resolved")
			c.fail("resolved some other way", "--conflict-apply", "--take", "remote", "resolved")
			if read(local, "resolved") != resolved || read(opposite, "resolved") != resolved {
				t.Fatal("resolution replaced")
			}
			c.invoke("--conflict-discard", "resolved")

			// Unresolved markers and changed copies are rejected. Listings
			// report copies changed since checkout.
			c.checkout("text")
			c.fail("conflict markers", "--conflict-apply", "text")
			write(opposite, "text", "concurrent edit\n")
			for _, conflict := range c.list().Conflicts {
				if (conflict.Path == "text") != conflict.Changed || (conflict.Path == "text" && conflict.CheckedOut != "local") {
					t.Fatalf("unexpected change reported: %+v", conflict)
				}
			}
			if !strings.Contains(c.invoke("--conflicts"), "text (checked out, but the remote copy changed since") {
				t.Fatal("changed copy not listed as text")
			}
			c.fail("the remote copy changed since checkout", "--conflict-checkout", "text")
			c.fail("changed since", "--conflict-apply", "--take", "local", "text")
			if read(opposite, "text") != "concurrent edit\n" {
				t.Fatal("concurrent edit overwritten")
			}

			// A local copy edited since checkout is detected before the remote
			// copy is updated. Several paths can be discarded at once, which
			// keeps their files.
			c.checkout("edited")
			original := read(opposite, "edited")
			write(local, "edited", "local edit\n")
			c.fail("changed since", "--conflict-apply", "--take", "local", "edited")
			if read(opposite, "edited") != original || read(local, "edited") != "local edit\n" {
				t.Fatal("copy updated despite a local edit")
			}
			discarded := c.outcomes("--conflict-discard", "text", "edited")
			if len(discarded) != 2 || readFile(filepath.Join(discarded[1].Backup, "remote")) != original {
				t.Fatalf("workspaces not discarded and kept: %+v", discarded)
			}
			c.fail("isn't checked out", "--conflict-discard", "text")

			// Directory conflicts remain listed and can't be checked out or
			// applied. Neither copy is changed.
			c.fail("isn't a conflict that remote can resolve", "--conflict-checkout", "type")
			c.fail("isn't checked out", "--conflict-apply", "--take", "local", "type")
			if read(roots[0], filepath.Join("type", "child")) != "child\n" || read(roots[1], "type") != "file\n" {
				t.Fatal("directory conflict modified despite rejection")
			}
			listing = c.list()
			for _, conflict := range listing.Conflicts {
				if conflict.CheckedOut != "" || conflict.Changed {
					t.Fatalf("unexpected conflict: %+v", conflict)
				}
			}
			if len(listing.Pending) != 0 || len(listing.RemotePending) != 0 {
				t.Fatalf("unexpected pending resolutions: %+v", listing)
			}
		})
	}
}

// createBothSidedConflicts creates a session that submits remote commands from
// both endpoints, with conflicts between two files at the specified paths, and
// returns conflict commands for alpha and beta.
func createBothSidedConflicts(t *testing.T, paths ...string) (*conflictCommands, *conflictCommands, [2]string) {
	t.Helper()
	roots := createConflictSessionWithCommands(t, synchronization.RemoteCommandMode_RemoteCommandModeBoth,
		core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	for _, path := range paths {
		writeFile(t, filepath.Join(roots[0], path), "base\n")
	}
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		writeFile(t, filepath.Join(roots[0], path), "alpha\n")
		writeFile(t, filepath.Join(roots[1], path), "beta\n")
	}
	return &conflictCommands{t: t, directory: roots[0]}, &conflictCommands{t: t, directory: roots[1]}, roots
}

func TestRemoteConflictSingleCheckout(t *testing.T) {
	// Workspaces are kept on the system that checked them out, but the daemon
	// records which endpoint checked out each conflict, so a conflict is
	// checked out through a single endpoint at a time, which alone can apply
	// or discard it, and only once.
	alpha, beta, roots := createBothSidedConflicts(t, "file")
	alpha.checkout("file")
	alpha.fail("is already checked out", "--conflict-checkout", "file")
	beta.fail("checked out through the opposite endpoint", "--conflict-checkout", "file")
	beta.fail("isn't checked out", "--conflict-discard", "file")
	if listing := alpha.list(); len(listing.Conflicts) != 1 || listing.Conflicts[0].CheckedOut != "local" {
		t.Fatalf("local checkout not listed: %+v", listing)
	} else if listing := beta.list(); len(listing.Conflicts) != 1 || listing.Conflicts[0].CheckedOut != "remote" {
		t.Fatalf("opposite checkout not listed: %+v", listing)
	} else if !strings.Contains(beta.invoke("--conflicts"), "file (checked out through the opposite endpoint") {
		t.Fatal("opposite checkout not listed as text")
	}

	// Discarding releases the checkout, so that the opposite endpoint can
	// check the conflict out, and applying releases it once resolved.
	alpha.invoke("--conflict-discard", "file")
	beta.checkout("file")
	alpha.fail("checked out through the opposite endpoint", "--conflict-checkout", "file")
	if applied := beta.outcomes("--conflict-apply", "--take", "local", "file")[0]; !applied.Resolved {
		t.Fatalf("conflict not resolved: %+v", applied)
	}
	for _, root := range roots {
		if data := readFile(filepath.Join(root, "file")); data != "beta\n" {
			t.Fatalf("resolution not applied: %q", data)
		}
	}
	for _, c := range []*conflictCommands{alpha, beta} {
		if listing := c.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 0 || len(listing.RemotePending) != 0 {
			t.Fatalf("resolved conflict still listed: %+v", listing)
		}
	}
}

func TestRemoteConflictCheckoutOwnership(t *testing.T) {
	// Apply claims the checkout before writing either copy, so a workspace
	// whose checkout the opposite endpoint holds, such as one checked out
	// after the daemon lost the session's checkouts, can't resolve the
	// conflict, and the opposite endpoint's resolution isn't replaced.
	alpha, beta, roots := createBothSidedConflicts(t, "file")
	alpha.checkout("file")
	directory, err := filesystem.Mutagen(false, filesystem.MutagenSynchronizationCheckoutsDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	checkouts, err := filepath.Glob(filepath.Join(directory, "*"))
	if err != nil || len(checkouts) != 1 {
		t.Fatalf("unexpected checkouts: %v, %v", checkouts, err)
	} else if err := os.Remove(checkouts[0]); err != nil {
		t.Fatal(err)
	}
	beta.checkout("file")
	alpha.fail("checked out through the opposite endpoint", "--conflict-apply", "--take", "local", "file")
	if readFile(filepath.Join(roots[0], "file")) != "alpha\n" || readFile(filepath.Join(roots[1], "file")) != "beta\n" {
		t.Fatal("copies changed by an apply without the checkout")
	} else if listing := alpha.list(); len(listing.Conflicts) != 1 || listing.Conflicts[0].CheckedOut != "remote" {
		t.Fatalf("opposite checkout doesn't prevail over a local workspace: %+v", listing)
	}
	beta.invoke("--conflict-apply", "--take", "remote", "file")
	alpha.invoke("--conflict-discard", "file")
	for _, root := range roots {
		if data := readFile(filepath.Join(root, "file")); data != "alpha\n" {
			t.Fatalf("resolution not applied: %q", data)
		}
	}
}

func TestRemoteConflictLateRequests(t *testing.T) {
	// A claim and a release can arrive after their submitter stopped waiting
	// for them, such as on timeout, and after the conflict was checked out
	// again through the same endpoint. They neither take over nor remove that
	// checkout, so the opposite endpoint can't check the conflict out.
	alpha, beta, _ := createBothSidedConflicts(t, "file")
	submit := func(operation remote.Operation, token string) (uint64, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		connection, _, err := remote.Dial(ctx, alpha.directory, "", operation)
		if err != nil {
			t.Fatal(err)
		}
		if operation == remote.OperationClaim {
			sequence, held, err := remote.ClaimCheckout(ctx, connection, "file", token)
			if err != nil {
				t.Fatal(err)
			}
			return sequence, held
		}
		_, held, err := remote.ReleaseCheckout(ctx, connection, "file", token)
		if err != nil {
			t.Fatal(err)
		}
		return 0, held
	}
	alpha.checkout("file")
	if sequence, held := submit(remote.OperationClaim, "late"); sequence != 0 || held == "" || held == "late" {
		t.Fatalf("late claim recorded (%d, %q)", sequence, held)
	} else if _, released := submit(remote.OperationRelease, "late"); released != held {
		t.Fatalf("late release not ignored (%q, %q)", held, released)
	}
	beta.fail("checked out through the opposite endpoint", "--conflict-checkout", "file")

	// A claim that arrives after the checkout was released records one without
	// a workspace, which checking the conflict out again through the same
	// endpoint releases.
	alpha.invoke("--conflict-discard", "file")
	if sequence, _ := submit(remote.OperationClaim, "late"); sequence == 0 {
		t.Fatal("late claim of a released checkout not recorded")
	}
	beta.fail("checked out through the opposite endpoint", "--conflict-checkout", "file")
	alpha.checkout("file")
	if _, held := submit(remote.OperationRelease, "late"); held == "" || held == "late" {
		t.Fatalf("abandoned checkout not replaced (%q)", held)
	}
	beta.fail("checked out through the opposite endpoint", "--conflict-checkout", "file")
	alpha.invoke("--conflict-discard", "file")
	beta.checkout("file")
}

func TestRemoteConflictLegacyCheckouts(t *testing.T) {
	// Builds that didn't issue tokens recorded checkouts without them. Applying
	// a workspace that such a build checked out claims its checkout, which
	// records the workspace's token, so that resolving the conflict releases
	// the checkout. Discarding releases such a checkout too, with or without a
	// workspace, and reports it. The opposite endpoint can then check the
	// conflicts out.
	alpha, beta, _ := createBothSidedConflicts(t, "applied", "discarded", "abandoned")
	for _, path := range []string{"applied", "discarded", "abandoned"} {
		w := alpha.checkout(path)
		if path == "abandoned" {
			if err := os.RemoveAll(filepath.Dir(w.Local)); err != nil {
				t.Fatal(err)
			}
		}
	}
	directory, err := filesystem.Mutagen(false, filesystem.MutagenSynchronizationCheckoutsDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	checkouts, err := filepath.Glob(filepath.Join(directory, "*"))
	if err != nil || len(checkouts) != 1 {
		t.Fatalf("unexpected checkouts: %v, %v", checkouts, err)
	}
	writeFile(t, checkouts[0], `[{"Path":"abandoned","Alpha":true},{"Path":"applied","Alpha":true},{"Path":"discarded","Alpha":true}]`)
	if applied := alpha.outcomes("--conflict-apply", "--take", "local", "applied")[0]; !applied.Resolved {
		t.Fatalf("conflict not resolved: %+v", applied)
	} else if listing := beta.list(); len(listing.RemotePending) != 0 {
		t.Fatalf("checkout of a resolved conflict not released: %+v", listing)
	}
	if discarded := alpha.outcomes("--conflict-discard", "discarded")[0]; !discarded.Discarded || discarded.Backup == "" {
		t.Fatalf("workspace not discarded: %+v", discarded)
	} else if discarded := alpha.outcomes("--conflict-discard", "abandoned")[0]; !discarded.Discarded || discarded.Backup != "" {
		t.Fatalf("checkout without a workspace not released: %+v", discarded)
	}
	alpha.fail("isn't checked out", "--conflict-discard", "abandoned")
	beta.checkout("discarded")
	beta.checkout("abandoned")
}

func TestRemoteConflictCorruptCheckouts(t *testing.T) {
	// Checkouts that can't be read, such as an empty file left by an operating
	// system crash, aren't taken for no checkouts, which would let both
	// endpoints check out the same conflict. Conflict operations report the
	// file, which can be removed to resolve conflicts again, and a workspace
	// whose conflict is still listed then claims its checkout again.
	alpha, _, roots := createBothSidedConflicts(t, "file")
	alpha.checkout("file")
	directory, err := filesystem.Mutagen(false, filesystem.MutagenSynchronizationCheckoutsDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	checkouts, err := filepath.Glob(filepath.Join(directory, "*"))
	if err != nil || len(checkouts) != 1 {
		t.Fatalf("unexpected checkouts: %v, %v", checkouts, err)
	}
	writeFile(t, checkouts[0], "")
	alpha.fail(checkouts[0], "--conflicts")
	alpha.fail("remove that file", "--conflict-apply", "--take", "local", "file")
	if err := os.Remove(checkouts[0]); err != nil {
		t.Fatal(err)
	} else if applied := alpha.outcomes("--conflict-apply", "--take", "local", "file")[0]; !applied.Resolved {
		t.Fatalf("conflict not resolved: %+v", applied)
	}
	for _, root := range roots {
		if data := readFile(filepath.Join(root, "file")); data != "alpha\n" {
			t.Fatalf("resolution not applied: %q", data)
		}
	}
}

func TestRemoteConflictAbandonedCheckout(t *testing.T) {
	// A checkout whose workspace is missing, such as one left by an
	// interrupted checkout, keeps the opposite endpoint from checking the
	// conflict out until listing conflicts or discarding releases it, or until
	// it's checked out again through its endpoint.
	alpha, beta, _ := createBothSidedConflicts(t, "listed", "discarded", "reused")
	for _, path := range []string{"listed", "discarded", "reused"} {
		w := alpha.checkout(path)
		if err := os.RemoveAll(filepath.Dir(w.Local)); err != nil {
			t.Fatal(err)
		}
		beta.fail("checked out through the opposite endpoint", "--conflict-checkout", path)
	}
	alpha.checkout("reused")
	if discarded := alpha.outcomes("--conflict-discard", "discarded")[0]; !discarded.Discarded || discarded.Backup != "" {
		t.Fatalf("checkout without a workspace not released: %+v", discarded)
	}
	alpha.fail("isn't checked out", "--conflict-discard", "discarded")
	for _, conflict := range alpha.list().Conflicts {
		if (conflict.Path == "reused") != (conflict.CheckedOut == "local") {
			t.Fatalf("abandoned checkout listed: %+v", conflict)
		}
	}
	beta.checkout("listed")
	beta.checkout("discarded")
	beta.fail("checked out through the opposite endpoint", "--conflict-checkout", "reused")
}

func TestRemoteConflictUnreadableWorkspace(t *testing.T) {
	// A workspace that can't be read, such as one whose metadata was modified
	// or removed, can be neither applied nor checked out again, and listing
	// conflicts doesn't release its checkout, which keeps the opposite endpoint
	// from checking the conflict out, so discarding sets it aside regardless.
	alpha, beta, _ := createBothSidedConflicts(t, "invalid", "missing")
	for _, path := range []string{"invalid", "missing"} {
		metadata := filepath.Join(filepath.Dir(alpha.checkout(path).Local), "workspace.json")
		if path == "invalid" {
			writeFile(t, metadata, "{")
		} else if err := os.Remove(metadata); err != nil {
			t.Fatal(err)
		}
		alpha.fail("discard it", "--conflict-apply", "--take", "local", path)
		alpha.fail("discard it", "--conflict-checkout", path)
		for _, conflict := range alpha.list().Conflicts {
			if conflict.Path == path && conflict.CheckedOut != "local" {
				t.Fatalf("checkout of an unreadable workspace released: %+v", conflict)
			}
		}
		beta.fail("checked out through the opposite endpoint", "--conflict-checkout", path)
		if discarded := alpha.outcomes("--conflict-discard", path)[0]; !discarded.Discarded || discarded.Backup == "" {
			t.Fatalf("unreadable workspace not discarded: %+v", discarded)
		} else if readFile(filepath.Join(discarded.Backup, "local")) != "alpha\n" {
			t.Fatal("unreadable workspace not kept")
		}
		beta.checkout(path)
	}
}

func TestRemoteConflictDiscardFromOutside(t *testing.T) {
	// Discarding locates the endpoint by the conflict root rather than the
	// working directory, so that it releases the checkout from outside both
	// roots, even though both endpoints are on this system, and from inside
	// the opposite root. That includes checkouts whose workspaces are already
	// gone, so that a discard whose release failed can be retried.
	alpha, beta, roots := createBothSidedConflicts(t, "workspace", "missing", "opposite")
	outside := &conflictCommands{t: t, directory: t.TempDir()}
	for _, path := range []string{"workspace", "missing", "opposite"} {
		w := alpha.checkout(path)
		if path == "missing" {
			if err := os.RemoveAll(filepath.Dir(w.Local)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if discarded := outside.outcomes("--conflict-discard", filepath.Join(roots[0], "workspace"))[0]; !discarded.Discarded || discarded.Backup == "" {
		t.Fatalf("workspace not discarded: %+v", discarded)
	} else if discarded := outside.outcomes("--conflict-discard", filepath.Join(roots[0], "missing"))[0]; !discarded.Discarded {
		t.Fatalf("checkout without a workspace not released: %+v", discarded)
	} else if discarded := beta.outcomes("--conflict-discard", filepath.Join(roots[0], "opposite"))[0]; !discarded.Discarded || discarded.Backup == "" {
		t.Fatalf("workspace not discarded from the opposite root: %+v", discarded)
	}
	for _, path := range []string{"workspace", "missing", "opposite"} {
		beta.checkout(path)
	}
}

func TestRemoteConflictStaleApplyForgotten(t *testing.T) {
	// An apply that fails because the remote copy changed since checkout
	// writes neither copy, so its workspace isn't taken for an unfinished
	// resolution once the conflict is resolved some other way, and apply then
	// refuses it.
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	local, opposite := filepath.Join(roots[0], "file"), filepath.Join(roots[1], "file")
	writeFile(t, local, "base\n")
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	}
	writeFile(t, local, "local\n")
	writeFile(t, opposite, "remote\n")
	c := &conflictCommands{t: t, directory: roots[0]}
	c.checkout("file")
	writeFile(t, opposite, "edited\n")
	c.fail("changed since", "--conflict-apply", "--take", "local", "file")
	writeFile(t, local, "edited\n")
	if listing := c.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 1 || listing.Pending[0].Applied {
		t.Fatalf("stale apply taken for an unfinished resolution: %+v", listing)
	}
	c.fail("resolved some other way", "--conflict-apply", "--take", "local", "file")
	if readFile(local) != "edited\n" || readFile(opposite) != "edited\n" {
		t.Fatal("resolution replaced")
	}
	c.invoke("--conflict-discard", "file")
}

func TestRemoteConflictTimedOutApply(t *testing.T) {
	// An apply that times out while the opposite endpoint replaces the remote
	// copy may leave that replacement running, since the endpoint only notices
	// later, even once it has checked the copy for the last time. Replacements
	// of a path are serialized, so a retry that takes the local copy, which
	// that replacement accepts as a checked-out copy, waits for it rather than
	// have its result overwritten by it, and supersedes it once it finishes.
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	local, opposite := filepath.Join(roots[0], "file"), filepath.Join(roots[1], "file")
	writeFile(t, local, "base\n")
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	}
	writeFile(t, local, "local\n")
	writeFile(t, opposite, "remote\n")
	c := &conflictCommands{t: t, directory: roots[0]}
	writeFile(t, c.checkout("file").Merged, "merged\n")

	// Pause the first replacement of the remote copy immediately before it
	// writes the merged file.
	var first atomic.Bool
	paused, resumed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(resumed) }) }
	remote.ReplaceHook = func(stage, directory, _ string) {
		if stage == "writing" && directory == roots[1] && first.CompareAndSwap(false, true) {
			close(paused)
			<-resumed
		}
	}
	t.Cleanup(func() {
		resume()
		remote.ReplaceHook = nil
	})
	if code, _, errors := runRemote(t, roots[0], "--timeout", "2s", "--conflict-apply", "file"); code != 124 {
		t.Fatalf("apply didn't time out: %d: %s", code, errors)
	}
	select {
	case <-paused:
	default:
		t.Fatal("replacement of the remote copy not pending")
	}

	// A retry waits for the pending replacement, here until it times out too,
	// and once that replacement finishes, a retry supersedes it.
	if code, _, errors := runRemote(t, roots[0], "--timeout", "2s", "--conflict-apply", "--take", "local", "file"); code != 124 {
		t.Fatalf("retry not serialized with the pending replacement: %d: %s", code, errors)
	} else if data := readFile(opposite); data != "remote\n" {
		t.Fatalf("remote copy replaced while a replacement was pending: %q", data)
	}
	resume()
	c.invoke("--conflict-apply", "--take", "local", "file")
	for _, root := range roots {
		if data := readFile(filepath.Join(root, "file")); data != "local\n" {
			t.Fatalf("retry's result not kept: %q", data)
		}
	}
}

func TestRemoteConflictRetryWithChangedResult(t *testing.T) {
	// The local copy is made unwritable by its parent's permissions, which
	// neither Windows nor the root user observe.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip()
	}
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	local, opposite := roots[0], roots[1]
	path := filepath.Join("sub", "file")
	for _, root := range roots {
		if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(local, path), "base\n")
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(local, path), "local\n")
	writeFile(t, filepath.Join(opposite, path), "remote\n")
	c := &conflictCommands{t: t, directory: local}
	w := c.checkout(path)

	// Apply fails after updating only the remote copy.
	writeFile(t, w.Merged, "first\n")
	if err := os.Chmod(filepath.Join(local, "sub"), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(local, "sub"), 0700)
	c.fail("updated the remote copy", "--conflict-apply", path)
	if err := os.Chmod(filepath.Join(local, "sub"), 0700); err != nil {
		t.Fatal(err)
	}

	// The conflict is still listed, between the result and the local copy, and
	// its resolution is marked as partially applied, which is finished by
	// applying again rather than by discarding and checking out again, which
	// would take the result for the remote copy. The copy that apply updated
	// isn't taken for one changed since checkout.
	if listing := c.list(); len(listing.Conflicts) != 1 || !listing.Conflicts[0].Applied ||
		listing.Conflicts[0].CheckedOut != "local" || listing.Conflicts[0].Changed || len(listing.Pending) != 0 {
		t.Fatalf("partially applied resolution not listed: %+v", listing)
	} else if text := c.invoke("--conflicts"); !strings.Contains(text, path+" (checked out and partially applied: apply again to finish)") {
		t.Fatal("partially applied resolution not listed as text:", text)
	}
	c.fail("partially applied through it: apply again to finish resolving it", "--conflict-checkout", path)

	// A retry with a different result supersedes the applied one.
	writeFile(t, w.Merged, "second\n")
	c.invoke("--conflict-apply", path)
	for _, root := range roots {
		if data := readFile(filepath.Join(root, path)); data != "second\n" {
			t.Fatalf("result not applied: %q", data)
		}
	}
}

func TestRemoteConflictRetryDeletion(t *testing.T) {
	// The local copy is made undeletable by its parent's permissions, which
	// neither Windows nor the root user observe.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip()
	}
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	local, opposite := roots[0], roots[1]
	path := filepath.Join("sub", "file")
	for _, root := range roots {
		if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(local, path), "base\n")
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(local, path), "local\n")
	writeFile(t, filepath.Join(opposite, path), "remote\n")
	c := &conflictCommands{t: t, directory: local}
	workspace := filepath.Dir(c.checkout(path).Local)

	// Apply fails after deleting only the remote copy.
	if err := os.Chmod(filepath.Join(local, "sub"), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(local, "sub"), 0700)
	c.fail("updated the remote copy", "--conflict-apply", "--delete", path)
	if err := os.Chmod(filepath.Join(local, "sub"), 0700); err != nil {
		t.Fatal(err)
	}

	// Synchronization propagates the local copy over the partial deletion,
	// which removes the conflict. The resolution is listed as unfinished, with
	// its workspace, and applying again completes it.
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	} else if readFile(filepath.Join(opposite, path)) != "local\n" {
		t.Fatal("partial deletion not reverted")
	}
	if listing := c.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 1 || listing.Pending[0].Path != path ||
		!listing.Pending[0].Applied || listing.Pending[0].Workspace != workspace {
		t.Fatalf("unfinished resolution not listed: %+v", listing)
	} else if text := c.invoke("--conflicts"); !strings.Contains(text, "Partially applied, but no longer listed") ||
		!strings.Contains(text, "apply again to finish resolving") || !strings.Contains(text, path+" (workspace: "+workspace+")") {
		t.Fatal("unfinished resolution not listed as text:", text)
	}
	c.fail("apply again to finish resolving it", "--conflict-checkout", path)
	c.invoke("--conflict-apply", "--delete", path)
	for _, root := range roots {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatal("copies not deleted:", err)
		}
	}
}

func TestRemoteConflictRetryAncestorResult(t *testing.T) {
	// The local copy is made unwritable by its parent's permissions, which
	// neither Windows nor the root user observe.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip()
	}
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	local, opposite := roots[0], roots[1]
	path := filepath.Join("sub", "file")
	for _, root := range roots {
		if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(local, path), "base\n")
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(local, path), "local\n")
	writeFile(t, filepath.Join(opposite, path), "remote\n")
	c := &conflictCommands{t: t, directory: local}
	w := c.checkout(path)

	// Apply fails after writing a result that matches what both copies last
	// agreed on to the remote copy only.
	writeFile(t, w.Merged, "base\n")
	if err := os.Chmod(filepath.Join(local, "sub"), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(local, "sub"), 0700)
	c.fail("updated the remote copy", "--conflict-apply", path)
	if err := os.Chmod(filepath.Join(local, "sub"), 0700); err != nil {
		t.Fatal(err)
	}

	// The remote copy then looks unchanged, so synchronization copies the
	// local copy over it, which removes the conflict. Applying again writes
	// the result to both copies, and synchronization keeps it.
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	} else if readFile(filepath.Join(opposite, path)) != "local\n" {
		t.Fatal("result not replaced by synchronization")
	} else if listing := c.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 1 || !listing.Pending[0].Applied {
		t.Fatalf("unfinished resolution not listed: %+v", listing)
	}
	c.invoke("--conflict-apply", path)
	for _, root := range roots {
		if data := readFile(filepath.Join(root, path)); data != "base\n" {
			t.Fatalf("result not applied: %q", data)
		}
	}
}

func TestRemoteConflictDiscardUnfinished(t *testing.T) {
	// The local copies are made undeletable by their parent's permissions,
	// which neither Windows nor the root user observe.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip()
	}
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	local, opposite := roots[0], roots[1]
	paths := []string{filepath.Join("sub", "json"), filepath.Join("sub", "text")}
	for _, root := range roots {
		if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		writeFile(t, filepath.Join(local, path), "base\n")
	}
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		writeFile(t, filepath.Join(local, path), "local\n")
		writeFile(t, filepath.Join(opposite, path), "remote\n")
	}
	c := &conflictCommands{t: t, directory: local}
	workspaces := make(map[string]string)
	for _, path := range paths {
		workspaces[path] = filepath.Dir(c.checkout(path).Local)
	}

	// Apply fails after deleting only the remote copies, and synchronization
	// then propagates the local copies over the partial deletions, which
	// removes the conflicts, so the remote originals exist only in the
	// workspaces. The listing recommends applying again.
	if err := os.Chmod(filepath.Join(local, "sub"), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(local, "sub"), 0700)
	c.fail("updated the remote copy", append([]string{"--conflict-apply", "--delete"}, paths...)...)
	if err := os.Chmod(filepath.Join(local, "sub"), 0700); err != nil {
		t.Fatal(err)
	} else if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	}
	heading, _, _ := strings.Cut(c.invoke("--conflicts"), "\n")
	if !strings.HasPrefix(heading, "Partially applied") || !strings.Contains(heading, "apply again to finish resolving") || strings.Contains(heading, "discard") {
		t.Fatal("applying again not recommended:", heading)
	}

	// Discarding the workspaces instead changes neither copy, but says that
	// results were applied through them, and where the remote originals are
	// kept.
	discarded := c.outcomes("--conflict-discard", paths[0])[0]
	if !discarded.Discarded || !discarded.Applied || readFile(filepath.Join(discarded.Backup, "remote")) != "remote\n" {
		t.Fatalf("applied results not reported: %+v", discarded)
	}
	text := c.invoke("--conflict-discard", paths[1])
	if kept, _ := filepath.Glob(filepath.Join(filepath.Dir(discarded.Backup), "*-"+filepath.Base(workspaces[paths[1]]))); len(kept) != 1 {
		t.Fatal("workspace not kept:", kept)
	} else if original := filepath.Join(kept[0], "remote"); readFile(original) != "remote\n" ||
		!strings.Contains(text, "Results were applied through it") || !strings.Contains(text, original) {
		t.Fatal("remote original not located:", text)
	}
	for _, path := range paths {
		for _, root := range roots {
			if data := readFile(filepath.Join(root, path)); data != "local\n" {
				t.Fatalf("copy changed by discard: %q", data)
			}
		}
	}
}

func TestRemoteConflictEndedByChange(t *testing.T) {
	// A change after checkout can end a conflict, such as one copy restored to
	// what both copies last agreed on (as git checkout would), so that
	// synchronization copies the other copy over it. The restored side's
	// changes then exist only in the workspace, which can't be told apart
	// from one whose conflict was resolved some other way, so the listing and
	// the errors of checkout and apply say both, and where the workspace is,
	// so that the current file can be compared with its copies before
	// discarding it.
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	local, opposite := filepath.Join(roots[0], "file"), filepath.Join(roots[1], "file")
	writeFile(t, local, "base\n")
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	}
	writeFile(t, local, "local\n")
	writeFile(t, opposite, "remote\n")
	c := &conflictCommands{t: t, directory: roots[0]}
	w := c.checkout("file")
	workspace := filepath.Dir(w.Local)
	writeFile(t, local, "base\n")
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	} else if readFile(local) != "remote\n" {
		t.Fatal("conflict not ended by the change")
	}
	if listing := c.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 1 ||
		listing.Pending[0].Path != "file" || listing.Pending[0].Applied || listing.Pending[0].Workspace != workspace {
		t.Fatalf("workspace of the ended conflict not listed: %+v", listing)
	}
	text := c.invoke("--conflicts")
	for _, expected := range []string{
		"resolved some other way, or ended by a change after checkout", "one side's changes only in the workspace",
		"compare the current file with the workspace's local and remote files before discarding",
		"discarded workspaces are deleted after 7 days", "file (workspace: " + workspace + ")",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("%q not listed: %s", expected, text)
		}
	}
	c.fail("ended by a change after checkout", "--conflict-checkout", "file")
	c.fail("ended by a change after checkout", "--conflict-apply", "--take", "local", "file")

	// The local changes remain in the workspace, which discarding keeps,
	// without changing either copy.
	if readFile(w.Local) != "local\n" {
		t.Fatal("local changes not kept in the workspace")
	}
	discarded := c.outcomes("--conflict-discard", "file")[0]
	if !discarded.Discarded || discarded.Applied || readFile(filepath.Join(discarded.Backup, "local")) != "local\n" {
		t.Fatalf("workspace not discarded and kept: %+v", discarded)
	}
	for _, path := range []string{local, opposite} {
		if data := readFile(path); data != "remote\n" {
			t.Fatalf("copy changed by discard: %q", data)
		}
	}
}

func TestRemoteConflictsOneWaySafe(t *testing.T) {
	for _, submitAlpha := range []bool{true, false} {
		t.Run(fmt.Sprintf("submitAlpha=%t", submitAlpha), func(t *testing.T) {
			roots := createConflictSession(t, submitAlpha, core.SynchronizationMode_SynchronizationModeOneWaySafe)
			local := roots[0]
			if !submitAlpha {
				local = roots[1]
			}
			writeFile(t, filepath.Join(roots[0], "file"), "base\n")
			if err := os.Mkdir(filepath.Join(roots[0], "type"), 0700); err != nil {
				t.Fatal(err)
			}
			if report, err := flushRemote(local); err != nil {
				t.Fatal(err)
			} else if !report.Resolution {
				t.Fatal("resolution not reported as supported")
			}
			writeFile(t, filepath.Join(roots[0], "file"), "alpha\n")
			writeFile(t, filepath.Join(roots[1], "file"), "beta\n")
			if err := os.Remove(filepath.Join(roots[1], "type")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(roots[0], "type", "child"), "child\n")
			writeFile(t, filepath.Join(roots[1], "type"), "file\n")
			c := &conflictCommands{t: t, directory: local}

			// Beta's changes to files can be merged into alpha, while
			// directory conflicts require manual resolution.
			listing := c.list()
			if len(listing.Conflicts) != 2 || !listing.Conflicts[0].Files || len(listing.Conflicts[0].Take) != 2 ||
				listing.Conflicts[1].Resolvable || listing.Conflicts[1].Files || len(listing.Conflicts[1].Take) != 0 {
				t.Fatalf("unexpected conflicts: %+v", listing.Conflicts)
			}
			w := c.checkout("file")
			writeFile(t, w.Merged, "merged\n")
			c.invoke("--conflict-apply", "file")
			c.fail("isn't a conflict that remote can resolve", "--conflict-checkout", "type")
			if readFile(filepath.Join(roots[1], "file")) != "merged\n" ||
				readFile(filepath.Join(roots[0], "type", "child")) != "child\n" || readFile(filepath.Join(roots[1], "type")) != "file\n" {
				t.Fatal("file resolution failed or directory conflict modified")
			}
		})
	}
}

func TestRemoteConflictsRequireSafeMode(t *testing.T) {
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWayResolved)
	if report, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	} else if report.Resolution {
		t.Fatal("resolution reported as supported")
	}
	c := &conflictCommands{t: t, directory: roots[0]}
	c.fail("two-way-safe or one-way-safe", "--conflicts")
}

func TestRemoteConflictFileRoot(t *testing.T) {
	// A session synchronizing a single file has its conflict at the root,
	// which is resolved from outside it.
	directory := t.TempDir()
	alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
	writeFile(t, alphaRoot, "base\n")
	sessionID := createRemoteDiffSession(t, alphaRoot, betaRoot)
	writeFile(t, alphaRoot, "local\n")
	writeFile(t, betaRoot, "remote\n")
	c := &conflictCommands{t: t, directory: directory, options: []string{"--session", sessionID}}
	if listing := c.list(); len(listing.Conflicts) != 1 || listing.Conflicts[0].Path != alphaRoot || !listing.Conflicts[0].Files {
		t.Fatalf("unexpected conflicts: %+v", listing.Conflicts)
	}
	// Discarding releases the root's checkout, though the root's parent is
	// outside it, so that it can be checked out again.
	c.checkout(alphaRoot)
	c.invoke("--conflict-discard", alphaRoot)
	w := c.checkout(alphaRoot)
	writeFile(t, w.Merged, "merged\n")
	c.fail("root can't be deleted", "--conflict-apply", "--delete", alphaRoot)
	c.invoke("--conflict-apply", alphaRoot)
	if readFile(alphaRoot) != "merged\n" || readFile(betaRoot) != "merged\n" {
		t.Fatal("root not resolved")
	}
}

func TestRemoteConflictDirectoryRejected(t *testing.T) {
	// A modified directory can be represented by changes only to its
	// children. It must still be recognized as an unsupported conflict.
	directory := t.TempDir()
	alphaRoot, betaRoot := filepath.Join(directory, "alpha"), filepath.Join(directory, "beta")
	if err := os.MkdirAll(filepath.Join(alphaRoot, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(alphaRoot, "d", "f"), "base\n")
	createRemoteDiffSession(t, alphaRoot, betaRoot)
	if err := os.RemoveAll(filepath.Join(betaRoot, "d")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(betaRoot, "d"), "replacement\n")
	writeFile(t, filepath.Join(alphaRoot, "d", "f"), "edited\n")
	c := &conflictCommands{t: t, directory: alphaRoot}
	if listing := c.list(); len(listing.Conflicts) != 1 || listing.Conflicts[0].Resolvable || len(listing.Conflicts[0].Take) != 0 {
		t.Fatalf("unexpected conflicts: %+v", listing.Conflicts)
	}
	c.fail("isn't a conflict that remote can resolve", "--conflict-checkout", "d")
	for _, side := range []string{"local", "remote"} {
		c.fail("isn't checked out", "--conflict-apply", "--take", side, "d")
	}
	if readFile(filepath.Join(alphaRoot, "d", "f")) != "edited\n" || readFile(filepath.Join(betaRoot, "d")) != "replacement\n" {
		t.Fatal("directory conflict modified despite rejection")
	}
}

func TestRemoteConflictCopyBecomesDirectory(t *testing.T) {
	// A copy to take that became a directory since checkout has changed, so
	// apply fails without changing either copy.
	for _, take := range []string{"local", "remote"} {
		t.Run(take, func(t *testing.T) {
			roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
			writeFile(t, filepath.Join(roots[0], "file"), "base\n")
			if _, err := flushRemote(roots[0]); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(roots[0], "file"), "local\n")
			if err := os.Remove(filepath.Join(roots[1], "file")); err != nil {
				t.Fatal(err)
			} else if err := os.Symlink("target", filepath.Join(roots[1], "file")); err != nil {
				t.Skip("symbolic links unavailable:", err)
			}
			c := &conflictCommands{t: t, directory: roots[0]}
			c.checkout("file")
			selected := roots[0]
			if take == "remote" {
				selected = roots[1]
			}
			if err := os.Remove(filepath.Join(selected, "file")); err != nil {
				t.Fatal(err)
			} else if err := os.Mkdir(filepath.Join(selected, "file"), 0700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(selected, "file", "child"), "new\n")
			c.fail("changed since", "--conflict-apply", "--take", take, "file")
			if readFile(filepath.Join(selected, "file", "child")) != "new\n" {
				t.Fatal("new directory modified")
			}
			if take == "remote" {
				if readFile(filepath.Join(roots[0], "file")) != "local\n" {
					t.Fatal("local copy deleted despite rejection")
				}
			} else if target, err := os.Readlink(filepath.Join(roots[1], "file")); err != nil || target != "target" {
				t.Fatalf("remote copy deleted despite rejection: %q: %v", target, err)
			}

			// Restoring the checked-out kind allows ordinary file/link
			// resolutions to proceed through the same workspace.
			if err := os.RemoveAll(filepath.Join(selected, "file")); err != nil {
				t.Fatal(err)
			}
			if take == "local" {
				writeFile(t, filepath.Join(selected, "file"), "local\n")
			} else if err := os.Symlink("target", filepath.Join(selected, "file")); err != nil {
				t.Fatal(err)
			}
			c.invoke("--conflict-apply", "--take", take, "file")
			for _, root := range roots {
				if take == "local" {
					if readFile(filepath.Join(root, "file")) != "local\n" {
						t.Fatal("file copy not taken")
					}
				} else if target, err := os.Readlink(filepath.Join(root, "file")); err != nil || target != "target" {
					t.Fatalf("symbolic link not taken: %q: %v", target, err)
				}
			}
		})
	}
}

func TestRemoteConflictTake(t *testing.T) {
	// Copies that aren't both files, such as a symbolic link and a file, are
	// resolved by writing the copy taken, as checked out, to the other
	// endpoint, in both safe modes and from either endpoint.
	for _, mode := range []core.SynchronizationMode{
		core.SynchronizationMode_SynchronizationModeTwoWaySafe,
		core.SynchronizationMode_SynchronizationModeOneWaySafe,
	} {
		for _, submitAlpha := range []bool{true, false} {
			for _, take := range []string{"local", "remote"} {
				t.Run(fmt.Sprintf("mode=%d/submitAlpha=%t/take=%s", mode, submitAlpha, take), func(t *testing.T) {
					roots := createConflictSession(t, submitAlpha, mode)
					local, alphaName, betaName := roots[0], "local", "remote"
					if !submitAlpha {
						local, alphaName, betaName = roots[1], "remote", "local"
					}
					writeFile(t, filepath.Join(roots[0], "file"), "base\n")
					if _, err := flushRemote(local); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(roots[0], "file")); err != nil {
						t.Fatal(err)
					} else if err := os.Symlink("target", filepath.Join(roots[0], "file")); err != nil {
						t.Skip("symbolic links unavailable:", err)
					}
					writeFile(t, filepath.Join(roots[1], "file"), "beta\n")
					c := &conflictCommands{t: t, directory: local}
					if listing := c.list(); len(listing.Conflicts) != 1 || listing.Conflicts[0].Files || len(listing.Conflicts[0].Take) != 2 {
						t.Fatalf("unexpected conflicts: %+v", listing.Conflicts)
					} else if w := c.checkout("file"); w.Files || len(w.Take) != 2 {
						t.Fatalf("unexpected checkout: %+v", w)
					}
					c.fail("--take local or --take remote", "--conflict-apply", "file")
					applied := c.outcomes("--conflict-apply", "--take", take, "file")[0]
					for _, root := range roots {
						if take == alphaName {
							if target, err := os.Readlink(filepath.Join(root, "file")); err != nil || target != "target" {
								t.Fatalf("symbolic link not taken: %q: %v", target, err)
							}
						} else if data := readFile(filepath.Join(root, "file")); data != "beta\n" {
							t.Fatalf("file not taken: %q", data)
						}
					}
					if listing := c.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 0 {
						t.Fatalf("conflict not resolved: %+v", listing)
					} else if target, err := os.Readlink(filepath.Join(applied.Backup, alphaName)); err != nil || target != "target" {
						t.Fatalf("original symbolic link not kept: %q: %v", target, err)
					} else if data := readFile(filepath.Join(applied.Backup, betaName)); data != "beta\n" {
						t.Fatalf("original file not kept: %q", data)
					}
				})
			}
		}
	}
}

func TestRemoteConflictTakeChangedCopy(t *testing.T) {
	// Checked-out paths mustn't change until they're resolved. Apply detects
	// either copy changed beforehand, whichever copy is taken, and fails
	// without changing either copy.
	for _, changed := range []string{"local", "remote"} {
		for _, take := range []string{"local", "remote"} {
			t.Run(fmt.Sprintf("changed=%s/take=%s", changed, take), func(t *testing.T) {
				roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
				writeFile(t, filepath.Join(roots[0], "file"), "base\n")
				if _, err := flushRemote(roots[0]); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(roots[0], "file")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				} else if err := os.Symlink("target", link); err != nil {
					t.Skip("symbolic links unavailable:", err)
				}
				writeFile(t, filepath.Join(roots[1], "file"), "remote\n")
				c := &conflictCommands{t: t, directory: roots[0]}
				c.checkout("file")
				target, data := "target", "remote\n"
				if changed == "local" {
					target = "edited"
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					} else if err := os.Symlink(target, link); err != nil {
						t.Fatal(err)
					}
				} else {
					data = "edited\n"
					writeFile(t, filepath.Join(roots[1], "file"), data)
				}
				c.fail("changed since", "--conflict-apply", "--take", take, "file")
				if current, err := os.Readlink(link); err != nil || current != target {
					t.Fatalf("local copy changed: %q: %v", current, err)
				} else if current := readFile(filepath.Join(roots[1], "file")); current != data {
					t.Fatalf("remote copy changed: %q", current)
				}
			})
		}
	}
}
