package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestDirectoryWorkspaceRejected(t *testing.T) {
	// Existing workspaces must not bypass the rejection of directory
	// conflicts, even when their conflict is no longer listed.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx := context.Background()
	for _, localDirectory := range []bool{false, true} {
		t.Run(fmt.Sprintf("localDirectory=%t", localDirectory), func(t *testing.T) {
			root := t.TempDir()
			local := filepath.Join(root, "conflict")
			_, directory, err := workspaceDirectory(local)
			if err != nil {
				t.Fatal(err)
			} else if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			w := &workspace{
				Session: "session", Path: "conflict", directory: directory,
				Local: &remote.Entry{Kind: remote.EntryFile}, Remote: &remote.Entry{Kind: remote.EntryDirectory},
				Take: []string{sideLocal, sideRemote},
			}
			if localDirectory {
				w.Local, w.Remote = w.Remote, w.Local
			}
			if err := w.save(); err != nil {
				t.Fatal(err)
			}
			target := &remote.Target{Session: "session", Root: root}
			if _, err := checkout(ctx, root, &options{}, &remote.Listing{}, target, local); !errors.Is(err, errDirectoryConflict) {
				t.Fatal("directory workspace reused:", err)
			}
			for _, side := range []string{sideLocal, sideRemote} {
				o := &options{conflictOptions: conflictOptions{take: side}}
				_, unlock, err := apply(ctx, root, o, local, func() (*remote.Listing, error) {
					t.Fatal("conflicts listed for a directory workspace")
					return nil, nil
				})
				if unlock != nil {
					unlock()
				}
				if !errors.Is(err, errDirectoryConflict) {
					t.Fatal("directory workspace not rejected before dialing:", err)
				}
			}
			// Without a daemon, the workspace is set aside, but its checkout
			// can't be released, which is reported.
			if _, err := discard(ctx, &options{}, local); err == nil || !strings.Contains(err.Error(), "unable to release its checkout") {
				t.Fatal("unreleased checkout not reported:", err)
			} else if _, err := loadWorkspace(local); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("directory workspace can't be discarded:", err)
			}
		})
	}
}

func TestConflictOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--conflicts", "--json"},
		{"--conflict-checkout", "--json", "a b"},
		{"--conflict-checkout", "--base", "base", "file"},
		{"--conflict-apply", "--take", "remote", "file", "other"},
		{"--conflict-apply", "--executable", "false", "file"},
		{"--conflict-apply", "--delete", "file"},
		{"--conflict-discard", "file", "./-file"},
	} {
		if _, err := parse(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"--conflicts", "--diff"}, {"--conflicts", "echo"}, {"--conflict-checkout", "--no-flush", "file"},
		{"--take", "remote", "echo"}, {"--conflict-apply", "--take", "alpha", "file"},
		{"--conflict-apply", "--delete", "--take", "local", "file"},
		{"--conflict-apply", "--conflict-discard", "file"}, {"--json", "echo"},
		{"--conflict-checkout"}, {"--conflict-apply", "file", "--take", "local"},
		{"--conflict-checkout", "--base", "base", "file", "other"}, {"--conflict-apply", "--base", "base", "file"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestMarkedMerge(t *testing.T) {
	markers := markersFor("test")
	for _, pair := range [][2]string{{"same\na\nend\n", "same\nb\nend\n"}, {"", "add"}, {"a", ""}, {"a", "b"}, {"same\n", "same\n"}} {
		merged := markedMerge([]byte(pair[0]), []byte(pair[1]), markers)
		if pair[0] == pair[1] {
			if merged != pair[0] {
				t.Fatal("equal contents changed")
			}
			continue
		}
		for _, marker := range []string{markers.local, markers.separator, markers.remote} {
			if !strings.Contains(merged, marker) {
				t.Fatal("missing marker")
			}
		}
		if strings.Contains(merged, markers.base) {
			t.Fatal("base marked without a base")
		}
		if strings.HasPrefix(pair[0], "same\n") && !strings.HasPrefix(merged, "same\n") {
			t.Fatal("shared prefix lost")
		}
	}
	path := filepath.Join(t.TempDir(), "merged")
	for _, marker := range markers.all() {
		os.WriteFile(path, []byte(strings.Repeat("a", 65530)+marker+"tail"), 0600)
		if found, err := containsMarkers(path, "test"); err != nil || !found {
			t.Fatalf("marker crossing read boundary missed: %v", err)
		}
	}

	// Markers are counted once, including when they cross read boundaries.
	data := strings.Repeat(markers.local+"\n", 3) + strings.Repeat("a", 65536-3*len(markers.local+"\n")-5) + markers.local + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	} else if count, err := countMarkers(path, markers.local); err != nil || count != 4 {
		t.Fatalf("counted %d markers: %v", count, err)
	}
}

func TestMergeThreeWay(t *testing.T) {
	markers := markersFor("test")
	lines := func(values ...string) []byte {
		return []byte(strings.Join(values, "\n") + "\n")
	}
	base := lines("1", "2", "3", "4", "5")
	for _, test := range []struct {
		description           string
		local, remote, merged []byte
		regions               int
	}{
		{"separate changes", lines("1", "two", "3", "4", "5"), lines("1", "2", "3", "4", "five"), lines("1", "two", "3", "4", "five"), 0},
		{"addition and deletion", lines("0", "1", "2", "3", "4", "5"), lines("1", "2", "4", "5"), lines("0", "1", "2", "4", "5"), 0},
		{"identical changes", lines("1", "2", "three", "4", "5"), lines("1", "2", "three", "4", "5"), lines("1", "2", "three", "4", "5"), 0},
		{"one side unchanged", base, lines("1", "2", "3", "4"), lines("1", "2", "3", "4"), 0},
		{"overlapping changes", lines("1", "2", "local", "4", "5"), lines("1", "2", "remote", "4", "5"), []byte(
			"1\n2\n" + markers.local + "\nlocal\n" + markers.base + "\n3\n" + markers.separator + "\nremote\n" + markers.remote + "\n4\n5\n"), 1},
		{"insertions at the same place", lines("1", "2", "3", "4", "5", "local"), lines("1", "2", "3", "4", "5", "remote"), []byte(
			"1\n2\n3\n4\n5\n" + markers.local + "\nlocal\n" + markers.base + "\n" + markers.separator + "\nremote\n" + markers.remote + "\n"), 1},
	} {
		if merged, regions := mergeThreeWay(base, test.local, test.remote, markers); merged != string(test.merged) || regions != test.regions {
			t.Fatalf("%s: merged into %q with %d regions", test.description, merged, regions)
		}
	}
}

func TestConflictOverlap(t *testing.T) {
	// A conflict at an ancestor, such as a directory replaced by a file on one
	// endpoint, or at a descendant involves the file just as one at its path.
	for _, path := range []string{"sub/file", "sub", "", "sub/file/child"} {
		if !overlaps(path, "sub/file") {
			t.Fatalf("%q doesn't overlap", path)
		}
	}
	for _, path := range []string{"sub/other", "sub/file2", "su", "other/sub/file"} {
		if overlaps(path, "sub/file") {
			t.Fatalf("%q overlaps", path)
		}
	}
}

func TestChangedSinceCheckout(t *testing.T) {
	// Listings identify copies changed since checkout by their digests, which
	// are listed for alpha and beta and checked out as local and remote.
	file := &remote.Entry{Kind: remote.EntryFile}
	listing := func(alpha, beta string) *remote.ConflictRoot {
		return &remote.ConflictRoot{Files: true, AlphaDigest: alpha, BetaDigest: beta}
	}
	for _, alpha := range []bool{true, false} {
		w := &workspace{Alpha: alpha, Local: file, Remote: file, ListedLocal: "local", ListedRemote: "remote"}
		swap := func(local, opposite string) *remote.ConflictRoot {
			if !alpha {
				return listing(opposite, local)
			}
			return listing(local, opposite)
		}
		for _, test := range []struct {
			conflict *remote.ConflictRoot
			expected string
		}{
			{swap("local", "remote"), ""},
			{swap("edited", "remote"), "local copy"},
			{swap("local", "edited"), "remote copy"},
			{swap("edited", "edited"), "local and remote copies"},
			{&remote.ConflictRoot{AlphaTakable: true}, "copies"},
		} {
			if changed := w.changedSince(test.conflict); changed != test.expected {
				t.Fatalf("alpha=%t: %+v: expected %q, got %q", alpha, test.conflict, test.expected, changed)
			}
		}
	}

	// Nothing is reported once something was applied, which changes the copies
	// itself, or without the digests of verified copies.
	w := &workspace{Alpha: true, Local: file, Remote: file, ListedLocal: "local", ListedRemote: "remote", Applied: []*remote.Entry{file}}
	if changed := w.changedSince(listing("edited", "edited")); changed != "" {
		t.Fatal("change reported after apply:", changed)
	}
	w = &workspace{Alpha: true, Local: file, Remote: file}
	if changed := w.changedSince(listing("edited", "edited")); changed != "" {
		t.Fatal("change reported without verified digests:", changed)
	}
}

func TestCheckedOutAgain(t *testing.T) {
	// A conflict with a workspace can't be checked out again, and the error
	// says what to do with the workspace.
	file := &remote.Entry{Kind: remote.EntryFile}
	target := &remote.Target{Session: "session", Alpha: true}
	listed := &remote.ConflictRoot{Path: "file", Files: true, AlphaDigest: "local", BetaDigest: "remote"}
	changed := &remote.ConflictRoot{Path: "file", Files: true, AlphaDigest: "local", BetaDigest: "edited"}
	w := func(applied bool) *workspace {
		w := &workspace{Session: "session", Alpha: true, Path: "file", Local: file, Remote: file, ListedLocal: "local", ListedRemote: "remote", directory: "workspace"}
		if applied {
			w.Applied = []*remote.Entry{file}
		}
		return w
	}
	for _, test := range []struct {
		w        *workspace
		conflict *remote.ConflictRoot
		expected string
	}{
		{w(false), listed, "is already checked out in workspace: apply it"},
		{w(false), changed, "the remote copy changed since checkout: discard the workspace"},
		{w(false), nil, "resolved some other way"},
		{w(true), nil, "apply again to finish resolving it"},
		{&workspace{Session: "other", Alpha: true, Path: "file", Local: file, Remote: file}, listed, "an unrelated workspace exists"},
		{&workspace{Session: "session", Alpha: true, Path: "file", Local: file, Remote: &remote.Entry{Kind: remote.EntryDirectory}}, listed, "aren't supported"},
	} {
		if err := checkedOut(test.w, test.conflict, target, "file"); err == nil || !strings.Contains(err.Error(), test.expected) {
			t.Fatalf("expected %q, got %v", test.expected, err)
		}
	}
}

func TestUnsettledResolution(t *testing.T) {
	// Verification fails if a conflict or a problem involves the conflict
	// root, at its path, an ancestor, or a descendant. Problems, such as a
	// symbolic link that synchronization can't propagate, may leave the copies
	// unsynchronized without a conflict.
	target := &remote.Target{Alpha: true, Root: t.TempDir()}
	a := &application{w: &workspace{Path: "sub/link"}, target: target, locator: newLocator(target)}
	if err := a.unsettled(&remote.Listing{
		Conflicts: []remote.ConflictRoot{{Path: "sub/other"}},
		Problems:  []remote.Problem{{Path: "sub/link2", Error: "unrelated"}, {Path: "other/sub/link", Error: "unrelated"}},
	}); err != nil {
		t.Fatal("unrelated conflict or problem blocked verification:", err)
	}
	for _, problem := range []remote.Problem{
		{Path: "sub/link", Error: "invalid symbolic link: backslash in target"},
		{Alpha: true, Path: "sub", Error: "permission denied"},
		{Path: "", Error: "unreadable root"},
		{Alpha: true, Path: "sub/link/child", Error: "permission denied"},
	} {
		side := "remote"
		if problem.Alpha {
			side = "local"
		}
		err := a.unsettled(&remote.Listing{Problems: []remote.Problem{problem}})
		if err == nil || !strings.Contains(err.Error(), "isn't verified") || !strings.Contains(err.Error(), problem.Error) ||
			!strings.Contains(err.Error(), a.locator.display(problem.Path)+" on the "+side+" endpoint") {
			t.Fatalf("problem at %q didn't block verification: %v", problem.Path, err)
		}
	}
	if err := a.unsettled(&remote.Listing{Conflicts: []remote.ConflictRoot{{Path: "sub"}}}); err == nil || !strings.Contains(err.Error(), "conflicts again") {
		t.Fatal("conflict at an ancestor didn't block verification:", err)
	}
}

// newTestWorkspace creates a workspace with the specified copies.
func newTestWorkspace(t *testing.T, local, opposite string) *workspace {
	t.Helper()
	w := &workspace{Path: "file", Token: "token", directory: t.TempDir()}
	return newTestWorkspaceWithBase(t, w, local, opposite, "")
}

// newTestWorkspaceWithBase prepares a workspace with the specified copies and,
// if specified, base.
func newTestWorkspaceWithBase(t *testing.T, w *workspace, local, opposite, base string) *workspace {
	t.Helper()
	if base != "" {
		w.BaseSource = "test"
		if err := os.WriteFile(w.file(baseName), []byte(base), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, copy := range []struct {
		name, data string
		entry      **remote.Entry
	}{{localName, local, &w.Local}, {remoteName, opposite, &w.Remote}} {
		path := w.file(copy.name)
		if err := os.WriteFile(path, []byte(copy.data), 0600); err != nil {
			t.Fatal(err)
		}
		entry, err := remote.ReadEntry(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		entry.Data = nil
		*copy.entry = entry
	}
	if err := prepareMerged(w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestLargeConflictWorkspace(t *testing.T) {
	w := newTestWorkspace(t, strings.Repeat("a", maximumMergeInput+10), strings.Repeat("b", maximumMergeInput+10))
	data, err := os.ReadFile(w.file(mergedName))
	if err != nil {
		t.Fatal(err)
	} else if len(data) < 2*(maximumMergeInput+10) || !strings.Contains(string(data), "======= token") {
		t.Fatal("large merge truncated")
	}
	if _, _, err := resolution(context.Background(), w, &options{}); err == nil {
		t.Fatal("unresolved markers accepted")
	}
	if result, source, err := resolution(context.Background(), w, &options{conflictOptions: conflictOptions{take: "remote"}}); err != nil ||
		result.Digest != w.Remote.Digest || source != w.file(remoteName) {
		t.Fatalf("take failed: %v", err)
	}
}

func TestBinaryAndExecutableResolution(t *testing.T) {
	ctx := context.Background()
	w := newTestWorkspace(t, "\x00local", "\x00remote")
	if _, err := os.Stat(w.file(mergedName)); !os.IsNotExist(err) {
		t.Fatal("binary copies given conflict markers")
	}
	if _, _, err := resolution(ctx, w, &options{}); err == nil {
		t.Fatal("missing merged file accepted")
	}
	take := &options{conflictOptions: conflictOptions{take: "remote"}}
	if result, _, err := resolution(ctx, w, take); err != nil || result.Digest != w.Remote.Digest {
		t.Fatalf("binary selection failed: %v", err)
	}
	os.WriteFile(w.file(remoteName), []byte("changed copy"), 0600)
	if _, _, err := resolution(ctx, w, take); err == nil {
		t.Fatal("modified original accepted as --take")
	}
	if result, source, err := resolution(ctx, w, &options{conflictOptions: conflictOptions{delete: true}}); err != nil ||
		result.Kind != remote.EntryMissing || source != "" {
		t.Fatalf("deletion failed: %v", err)
	}

	// Copies differing only in executability get an unmarked merged file, but
	// require an explicit choice of executability.
	w = newTestWorkspace(t, "same\n", "same\n")
	w.Remote.Executable = true
	if data, err := os.ReadFile(w.file(mergedName)); err != nil || string(data) != "same\n" {
		t.Fatalf("identical contents not copied: %v", err)
	}
	if _, _, err := resolution(ctx, w, &options{}); err == nil {
		t.Fatal("ambiguous executability accepted")
	}
	if result, _, err := resolution(ctx, w, &options{conflictOptions: conflictOptions{executable: "true"}}); err != nil || !result.Executable {
		t.Fatalf("explicit executability failed: %v", err)
	}
}

func TestWorkspaceMetadata(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	local := filepath.Join(t.TempDir(), "file")
	_, directory, err := workspaceDirectory(local)
	if err != nil {
		t.Fatal(err)
	} else if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	file := &remote.Entry{Kind: remote.EntryFile, Size: 1, Digest: strings.Repeat("0", 64)}
	w := &workspace{Path: "file", Local: file, Remote: file, Applied: []*remote.Entry{file}, directory: directory}
	if err := w.save(); err != nil {
		t.Fatal(err)
	} else if loaded, err := loadWorkspace(local); err != nil || len(loaded.Applied) != 1 || !remote.SameFile(loaded.Applied[0], file) {
		t.Fatalf("applied results not saved: %v", err)
	} else if entries, _ := os.ReadDir(directory); len(entries) != 1 {
		t.Fatal("saving left files behind:", entries)
	}
	// Deletions and symbolic links are results too, but directories aren't.
	w.Applied = append(w.Applied, &remote.Entry{Kind: remote.EntryMissing}, &remote.Entry{Kind: remote.EntrySymbolicLink, Target: "target"})
	if err := w.save(); err != nil {
		t.Fatal(err)
	} else if loaded, err := loadWorkspace(local); err != nil || len(loaded.Applied) != 3 || !loaded.applied() {
		t.Fatalf("applied results not saved: %v", err)
	}
	w.Applied = append(w.Applied, &remote.Entry{Kind: remote.EntryDirectory})
	if err := w.save(); err != nil {
		t.Fatal(err)
	} else if _, err := loadWorkspace(local); err == nil {
		t.Fatal("invalid applied result accepted")
	}
	w = &workspace{Path: "file", Local: file, Remote: file, directory: directory}
	if err := w.save(); err != nil {
		t.Fatal(err)
	} else if loaded, err := loadWorkspace(local); err != nil || loaded.applied() {
		t.Fatalf("unapplied workspace considered applied: %v", err)
	}
}

func TestTakeResolution(t *testing.T) {
	// Copies that aren't both files can only be taken as checked out: a
	// symbolic link by its target, and a file from the workspace, unmodified.
	ctx := context.Background()
	w := &workspace{Path: "file", Token: "token", directory: t.TempDir(), Take: []string{sideLocal, sideRemote}}
	if err := os.WriteFile(w.file(localName), []byte("local\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	if w.Local, err = remote.ReadEntry(ctx, w.file(localName)); err != nil {
		t.Fatal(err)
	}
	w.Local.Data = nil
	w.Remote = &remote.Entry{Kind: remote.EntrySymbolicLink, Target: "target"}
	for _, o := range []conflictOptions{{}, {delete: true}, {take: sideLocal, executable: "false"}} {
		if _, _, err := resolution(ctx, w, &options{conflictOptions: o}); err == nil || !strings.Contains(err.Error(), "can only be resolved with --take") {
			t.Fatalf("%+v accepted: %v", o, err)
		}
	}
	if result, source, err := resolution(ctx, w, &options{conflictOptions: conflictOptions{take: sideRemote}}); err != nil ||
		result.Kind != remote.EntrySymbolicLink || result.Target != "target" || source != "" {
		t.Fatalf("symbolic link not taken: %+v: %v", result, err)
	}
	if result, source, err := resolution(ctx, w, &options{conflictOptions: conflictOptions{take: sideLocal}}); err != nil ||
		!remote.SameFile(result, w.Local) || source != w.file(localName) {
		t.Fatalf("file not taken: %+v: %v", result, err)
	}
	if err := os.WriteFile(w.file(localName), []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	} else if _, _, err := resolution(ctx, w, &options{conflictOptions: conflictOptions{take: sideLocal}}); err == nil || !strings.Contains(err.Error(), "as checked out") {
		t.Fatalf("modified copy taken: %v", err)
	}
	w.Take = []string{sideLocal}
	if _, _, err := resolution(ctx, w, &options{conflictOptions: conflictOptions{take: sideRemote}}); err == nil || !strings.Contains(err.Error(), "can't be taken") {
		t.Fatalf("copy that can't be taken accepted: %v", err)
	}
}

func TestWorkspaceLock(t *testing.T) {
	// POSIX record locks don't exclude other locks held by the same process,
	// so a helper process holds the lock until its input closes.
	if local := os.Getenv("MUTAGEN_TEST_WORKSPACE_LOCK"); local != "" {
		unlock, err := lockWorkspace(local)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("locked")
		io.Copy(io.Discard, os.Stdin)
		unlock()
		os.Exit(0)
	}
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	local := filepath.Join(t.TempDir(), "file")
	helper := exec.Command(os.Args[0], "-test.run=^TestWorkspaceLock$")
	helper.Env = append(os.Environ(), "MUTAGEN_TEST_WORKSPACE_LOCK="+local)
	input, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	} else if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("helper unable to lock: %q: %v", line, err)
	}
	if unlock, err := lockWorkspace(local); err == nil {
		unlock()
		t.Fatal("concurrent operation allowed")
	} else if !strings.Contains(err.Error(), "in progress") {
		t.Fatal("unexpected error:", err)
	}
	input.Close()
	if err := helper.Wait(); err != nil {
		t.Fatal("helper failed:", err)
	}
	unlock, err := lockWorkspace(local)
	if err != nil {
		t.Fatal("unable to lock after release:", err)
	}
	unlock()
}

func TestWriteCopyVerifiesContents(t *testing.T) {
	// Copies are written as received, but must have the contents read at
	// their source.
	directory := t.TempDir()
	entry := &remote.Entry{Kind: remote.EntryFile, Size: 5, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("data\n")))}
	for name, data := range map[string]string{"intact": "data\n", "corrupted": "dat4\n", "truncated": "data"} {
		_, err := writeCopy(filepath.Join(directory, name), func(file io.Writer) (*remote.Entry, error) {
			_, err := io.WriteString(file, data)
			return entry, err
		})
		if (err == nil) != (name == "intact") {
			t.Fatalf("%s copy: %v", name, err)
		}
	}
}

func TestUnterminatedCopies(t *testing.T) {
	// Copies that don't end with a newline are noted, since conflict markers
	// add one to the merged file.
	for _, test := range []struct {
		local, remote, note string
	}{
		{"same\nlocal", "same\nremote\n", "The local copy doesn't end with a newline"},
		{"same\nlocal\n", "same\nremote", "The remote copy doesn't end with a newline"},
		{"local", "remote", "Neither copy ends with a newline"},
		{"local\n", "remote\n", ""},
	} {
		result, err := describe(newTestWorkspace(t, test.local, test.remote), "file")
		if err != nil {
			t.Fatal(err)
		}
		output := strings.Join(result.lines, "\n")
		if test.note == "" && strings.Contains(output, "newline") {
			t.Fatalf("%q and %q noted: %s", test.local, test.remote, output)
		} else if !strings.Contains(output, test.note) {
			t.Fatalf("%q and %q not noted: %s", test.local, test.remote, output)
		}
	}
}
