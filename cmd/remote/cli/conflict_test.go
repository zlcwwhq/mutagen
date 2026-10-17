package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestConflictOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--conflicts", "--json"},
		{"--conflict-checkout", "a b", "--json"},
		{"--conflict-apply", "file", "--take", "remote"},
		{"--conflict-apply", "file", "--executable", "false"},
		{"--conflict-apply", "file", "--delete"},
		{"--conflict-discard", "file"},
	} {
		if _, err := parse(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"--conflicts", "--diff"}, {"--conflicts", "echo"}, {"--conflict-checkout", "file", "--no-flush"},
		{"--take", "remote", "echo"}, {"--conflict-apply", "file", "--take", "alpha"},
		{"--conflict-apply", "file", "--delete", "--take", "local"},
		{"--conflict-apply", "file", "--conflict-discard", "file"}, {"--json", "echo"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestMarkedMerge(t *testing.T) {
	for _, pair := range [][2]string{{"same\na\nend\n", "same\nb\nend\n"}, {"", "add"}, {"a", ""}, {"a", "b"}, {"same\n", "same\n"}} {
		merged := markedMerge([]byte(pair[0]), []byte(pair[1]), "test")
		if pair[0] == pair[1] {
			if merged != pair[0] {
				t.Fatal("equal contents changed")
			}
			continue
		}
		for _, marker := range markerLines("test") {
			if !strings.Contains(merged, marker) {
				t.Fatal("missing marker")
			}
		}
		if strings.HasPrefix(pair[0], "same\n") && !strings.HasPrefix(merged, "same\n") {
			t.Fatal("shared prefix lost")
		}
	}
	path := filepath.Join(t.TempDir(), "merged")
	for _, marker := range markerLines("token") {
		os.WriteFile(path, []byte(strings.Repeat("a", 65530)+marker+"tail"), 0600)
		if found, err := containsMarkers(path, "token"); err != nil || !found {
			t.Fatalf("marker crossing read boundary missed: %v", err)
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

// newTestWorkspace creates a workspace with the specified copies.
func newTestWorkspace(t *testing.T, local, opposite string) *workspace {
	t.Helper()
	w := &workspace{Token: "token", directory: t.TempDir()}
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
	w.Applied = append(w.Applied, &remote.Entry{Kind: remote.EntryMissing})
	if err := w.save(); err != nil {
		t.Fatal(err)
	} else if _, err := loadWorkspace(local); err == nil {
		t.Fatal("invalid applied result accepted")
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
		var output strings.Builder
		if err := printWorkspace(&output, newTestWorkspace(t, test.local, test.remote), false); err != nil {
			t.Fatal(err)
		} else if test.note == "" && strings.Contains(output.String(), "newline") {
			t.Fatalf("%q and %q noted: %s", test.local, test.remote, output.String())
		} else if !strings.Contains(output.String(), test.note) {
			t.Fatalf("%q and %q not noted: %s", test.local, test.remote, output.String())
		}
	}
}
