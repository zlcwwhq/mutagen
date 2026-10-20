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
	"testing"

	"github.com/mutagen-io/mutagen/pkg/integration/protocols/netpipe"
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
		CheckedOut bool
	}
	Pending []struct {
		Path    string
		Applied bool
	}
}

// conflictOutcome is a line of JSON output of other conflict operations.
type conflictOutcome struct {
	Path, Error, Local, Remote, Base, Merged  string
	Files, Binary, Pending, Applied, Resolved bool
	Regions                                   int
	Take                                      []string
	Backup                                    string
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

			// Large copies are downloaded completely, repeated checkouts
			// preserve edits, and applying resolves the conflict and keeps
			// the original copies.
			w := c.checkout("large")
			for _, path := range []string{w.Local, w.Remote} {
				if info, err := os.Stat(path); err != nil || info.Size() <= 1024*1024 {
					t.Fatalf("large checkout incomplete: %v", err)
				}
			}
			writeFile(t, w.Merged, "merged\n")
			c.checkout("large")
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
			c.fail("discard the workspace first", "--conflict-checkout", "--base", base, "merge")
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
			// workspace is listed as such, and apply refuses it without
			// changing either copy, since it would replace that resolution.
			// Partially applied resolutions are retried elsewhere (see
			// TestRemoteConflictRetryDeletion).
			c.checkout("resolved")
			resolved := read(local, "resolved")
			write(opposite, "resolved", resolved)
			if _, err := flushRemote(local); err != nil {
				t.Fatal(err)
			}
			if listing := c.list(); len(listing.Pending) != 1 || listing.Pending[0].Path != "resolved" || listing.Pending[0].Applied {
				t.Fatalf("workspace with nothing applied not listed: %+v", listing.Pending)
			} else if w := c.checkout("resolved"); !w.Pending || w.Applied {
				t.Fatalf("workspace with nothing applied not described: %+v", w)
			} else if !strings.Contains(c.invoke("--conflicts"), "with nothing applied") {
				t.Fatal("workspace with nothing applied not listed as text")
			}
			c.fail("resolved some other way", "--conflict-apply", "--take", "remote", "resolved")
			if read(local, "resolved") != resolved || read(opposite, "resolved") != resolved {
				t.Fatal("resolution replaced")
			}
			c.invoke("--conflict-discard", "resolved")

			// Unresolved markers and changed copies are rejected.
			c.checkout("text")
			c.fail("conflict markers", "--conflict-apply", "text")
			write(opposite, "text", "concurrent edit\n")
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
				if conflict.CheckedOut {
					t.Fatalf("unexpected conflict: %+v", conflict)
				}
			}
			if len(listing.Pending) != 0 {
				t.Fatalf("unexpected pending resolutions: %+v", listing.Pending)
			}
		})
	}
}

func TestRemoteConflictTwoResolvers(t *testing.T) {
	// Workspaces are kept on the system that checked them out, so a conflict
	// can be checked out from both endpoints, which conflict operations on
	// one system can't detect. Once it's resolved through one workspace, the
	// other has nothing applied and its conflict is no longer listed, so apply
	// refuses it, even with a result that accepts the copies, which are both
	// one of its checked-out copies.
	roots := createConflictSessionWithCommands(t, synchronization.RemoteCommandMode_RemoteCommandModeBoth,
		core.SynchronizationMode_SynchronizationModeTwoWaySafe)
	writeFile(t, filepath.Join(roots[0], "file"), "base\n")
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(roots[0], "file"), "alpha\n")
	writeFile(t, filepath.Join(roots[1], "file"), "beta\n")
	alpha := &conflictCommands{t: t, directory: roots[0]}
	beta := &conflictCommands{t: t, directory: roots[1]}
	alpha.checkout("file")
	beta.checkout("file")
	if applied := alpha.outcomes("--conflict-apply", "--take", "local", "file")[0]; !applied.Resolved {
		t.Fatalf("conflict not resolved: %+v", applied)
	}
	for _, args := range [][]string{{"--delete"}, {"--take", "local"}, {"--take", "remote"}} {
		beta.fail("resolved some other way", append(append([]string{"--conflict-apply"}, args...), "file")...)
		for _, root := range roots {
			if data := readFile(filepath.Join(root, "file")); data != "alpha\n" {
				t.Fatalf("%v replaced the resolution: %q", args, data)
			}
		}
	}
	if listing := beta.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 1 || listing.Pending[0].Applied {
		t.Fatalf("stale workspace not listed: %+v", listing)
	}
	beta.invoke("--conflict-discard", "file")
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

	// A retry with a different result supersedes the applied one.
	if err := os.Chmod(filepath.Join(local, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
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
	c.checkout(path)

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
	// which removes the conflict. The resolution is listed as unfinished, and
	// applying again completes it.
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	} else if readFile(filepath.Join(opposite, path)) != "local\n" {
		t.Fatal("partial deletion not reverted")
	}
	if listing := c.list(); len(listing.Conflicts) != 0 || len(listing.Pending) != 1 || listing.Pending[0].Path != path || !listing.Pending[0].Applied {
		t.Fatalf("unfinished resolution not listed: %+v", listing)
	} else if w := c.checkout(path); !w.Pending || !w.Applied {
		t.Fatalf("unfinished resolution not described: %+v", w)
	} else if !strings.Contains(c.invoke("--conflicts"), "Applied, but no longer listed") {
		t.Fatal("unfinished resolution not listed as text")
	}
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
