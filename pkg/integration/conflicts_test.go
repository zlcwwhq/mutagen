package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	roots := [2]string{t.TempDir(), t.TempDir()}
	commands := synchronization.RemoteCommandMode_RemoteCommandModeBeta
	if !submitAlpha {
		commands = synchronization.RemoteCommandMode_RemoteCommandModeAlpha
	}
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
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			read := func(root, name string) string {
				data, _ := os.ReadFile(filepath.Join(root, name))
				return string(data)
			}
			for _, name := range []string{"text", "partial", "deleted", "edited", "large"} {
				write(roots[0], name, "base\n")
			}
			if report, err := flushRemote(local); err != nil {
				t.Fatal(err)
			} else if !report.Resolution {
				t.Fatal("resolution not reported as supported")
			}
			for side, root := range roots {
				write(root, "text", fmt.Sprintf("shared\nside%d\n", side))
				write(root, "partial", fmt.Sprintf("side%d\n", side))
				write(root, "deleted", fmt.Sprintf("side%d\n", side))
				write(root, "edited", fmt.Sprintf("side%d\n", side))
				write(root, "large", strings.Repeat(fmt.Sprintf("side%d data\n", side), 150000))
			}
			// A file created on one side and a directory on the other.
			if err := os.Mkdir(filepath.Join(roots[0], "type"), 0700); err != nil {
				t.Fatal(err)
			}
			write(roots[1], "type", "file\n")

			invoke := func(args ...string) string {
				t.Helper()
				code, output, errors := runRemote(t, local, append([]string{"--timeout", "20s"}, args...)...)
				if code != 0 {
					t.Fatalf("%v: %d: %s %s", args, code, output, errors)
				}
				return output
			}
			fail := func(expected string, args ...string) {
				t.Helper()
				if code, _, errors := runRemote(t, local, append([]string{"--timeout", "20s"}, args...)...); code == 0 || !strings.Contains(errors, expected) {
					t.Fatalf("%v: expected failure with %q, got %d: %s", args, expected, code, errors)
				}
			}
			checkout := func(path string) (w struct{ Local, Remote, Merged string }) {
				t.Helper()
				if err := json.Unmarshal([]byte(invoke("--conflict-checkout", path, "--json")), &w); err != nil {
					t.Fatal(err)
				}
				return
			}

			// Conflicts are listed completely, with local paths.
			var listing struct{ Conflicts []remote.ConflictRoot }
			if err := json.Unmarshal([]byte(invoke("--conflicts", "--json")), &listing); err != nil {
				t.Fatal(err)
			}
			expected := []remote.ConflictRoot{
				{Path: "deleted", Resolvable: true}, {Path: "edited", Resolvable: true}, {Path: "large", Resolvable: true}, {Path: "partial", Resolvable: true},
				{Path: "text", Resolvable: true}, {Path: "type"},
			}
			if fmt.Sprint(listing.Conflicts) != fmt.Sprint(expected) {
				t.Fatal("unexpected conflicts:", listing.Conflicts)
			}
			fail("can't resolve", "--conflict-checkout", "type")

			// Large copies are downloaded completely, repeated checkouts
			// preserve edits, and applying resolves the conflict.
			w := checkout("large")
			for _, path := range []string{w.Local, w.Remote} {
				if info, err := os.Stat(path); err != nil || info.Size() <= remote.MaximumComparedFileSize {
					t.Fatalf("large checkout incomplete: %v", err)
				}
			}
			if err := os.WriteFile(w.Merged, []byte("merged\n"), 0600); err != nil {
				t.Fatal(err)
			}
			checkout("large")
			if data, _ := os.ReadFile(w.Merged); string(data) != "merged\n" {
				t.Fatal("repeated checkout overwrote edits")
			}
			invoke("--conflict-apply", "large")
			if read(local, "large") != "merged\n" || read(opposite, "large") != "merged\n" {
				t.Fatal("result not applied to both copies")
			}
			fail("isn't checked out", "--conflict-apply", "large")

			// A partially applied resolution remains a conflict, and applying
			// again completes it.
			checkout("partial")
			write(opposite, "partial", read(local, "partial"))
			if _, err := flushRemote(local); err != nil {
				t.Fatal(err)
			}
			invoke("--conflict-apply", "partial", "--take", "local")
			if read(opposite, "partial") != read(local, "partial") {
				t.Fatal("partial resolution not completed")
			}

			// Synchronization may revert a partially applied resolution, such
			// as a deletion, which it overrides with the local copy, removing
			// the conflict. Applying again still completes the resolution.
			checkout("deleted")
			if err := os.Remove(filepath.Join(opposite, "deleted")); err != nil {
				t.Fatal(err)
			} else if _, err := flushRemote(local); err != nil {
				t.Fatal(err)
			} else if read(opposite, "deleted") != read(local, "deleted") {
				t.Fatal("partial deletion not reverted")
			}
			invoke("--conflict-apply", "deleted", "--delete")
			for _, root := range roots {
				if _, err := os.Lstat(filepath.Join(root, "deleted")); !os.IsNotExist(err) {
					t.Fatal("copies not deleted:", err)
				}
			}

			// Unresolved markers and changed copies are rejected.
			checkout("text")
			fail("conflict markers", "--conflict-apply", "text")
			write(opposite, "text", "concurrent edit\n")
			fail("changed since", "--conflict-apply", "text", "--take", "local")
			if read(opposite, "text") != "concurrent edit\n" {
				t.Fatal("concurrent edit overwritten")
			}
			invoke("--conflict-discard", "text")
			fail("isn't checked out", "--conflict-discard", "text")

			// A local copy edited since checkout is detected before the remote
			// copy is updated.
			checkout("edited")
			original := read(opposite, "edited")
			write(local, "edited", "local edit\n")
			fail("changed since", "--conflict-apply", "edited", "--take", "local")
			if read(opposite, "edited") != original || read(local, "edited") != "local edit\n" {
				t.Fatal("copy updated despite a local edit")
			}
		})
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
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(local, path), "base\n")
	if _, err := flushRemote(local); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(local, path), "local\n")
	write(filepath.Join(opposite, path), "remote\n")
	run := func(args ...string) (int, string, string) {
		return runRemote(t, local, append([]string{"--timeout", "20s"}, args...)...)
	}
	code, output, errors := run("--conflict-checkout", path, "--json")
	var w struct{ Merged string }
	if code != 0 || json.Unmarshal([]byte(output), &w) != nil {
		t.Fatalf("checkout failed: %d: %s", code, errors)
	}

	// Apply fails after updating only the remote copy.
	write(w.Merged, "first\n")
	if err := os.Chmod(filepath.Join(local, "sub"), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(local, "sub"), 0700)
	if code, _, errors := run("--conflict-apply", path); code == 0 || !strings.Contains(errors, "updated the remote copy") {
		t.Fatalf("apply didn't fail after updating the remote copy: %d: %s", code, errors)
	}

	// A retry with a different result supersedes the applied one.
	if err := os.Chmod(filepath.Join(local, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	write(w.Merged, "second\n")
	if code, _, errors := run("--conflict-apply", path); code != 0 {
		t.Fatalf("retry with a different result failed: %d: %s", code, errors)
	}
	for _, root := range roots {
		if data, _ := os.ReadFile(filepath.Join(root, path)); string(data) != "second\n" {
			t.Fatalf("result not applied: %q", data)
		}
	}
}

func TestRemoteConflictsRequireTwoWaySafe(t *testing.T) {
	roots := createConflictSession(t, true, core.SynchronizationMode_SynchronizationModeOneWaySafe)
	write := func(root, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "file"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(roots[0], "base\n")
	if _, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	}
	write(roots[0], "alpha\n")
	write(roots[1], "beta\n")

	// Conflicts are reported without referring to resolution.
	if report, err := flushRemote(roots[0]); err != nil {
		t.Fatal(err)
	} else if len(report.Conflicts) != 1 || report.Resolution {
		t.Fatalf("unexpected report: %+v", report)
	}
	if code, _, errors := runRemote(t, roots[0], "--timeout", "20s", "--flush"); code != 0 || strings.Contains(errors, "--conflicts") {
		t.Fatalf("resolution referenced in one-way-safe mode: %d: %s", code, errors)
	}
	if code, _, errors := runRemote(t, roots[0], "--timeout", "20s", "--conflicts"); code == 0 || !strings.Contains(errors, "two-way-safe") {
		t.Fatalf("conflict listing allowed in one-way-safe mode: %d: %s", code, errors)
	}
}
