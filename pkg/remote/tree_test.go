package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeTree creates files (or directories, for names ending with a slash) with
// the specified contents beneath a directory.
func writeTree(t *testing.T, directory string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		path := filepath.Join(directory, filepath.FromSlash(strings.TrimSuffix(name, "/")))
		err := os.MkdirAll(filepath.Dir(path), 0700)
		if err == nil && strings.HasSuffix(name, "/") {
			err = os.Mkdir(path, 0700)
		} else if err == nil {
			err = os.WriteFile(path, []byte(data), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotArchiveRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"dir/a": "a\n", "dir/sub/b": "b\n", "dir/empty/": "", "dir/.mutagen-temporary-x": "ignored\n", "file": "file\n",
	})
	if err := os.Chmod(filepath.Join(root, "dir", "sub", "b"), 0700); err != nil {
		t.Fatal(err)
	}
	links := os.Symlink("a", filepath.Join(root, "dir", "link")) == nil
	if links {
		os.Symlink("file", filepath.Join(root, "link"))
	}
	fifo := mkfifo(filepath.Join(root, "dir", "fifo")) == nil

	// Archives of each kind of entry are extracted as snapshotted.
	for _, path := range []string{"dir", "file", "link", "missing"} {
		if path == "link" && !links {
			continue
		}
		var archive bytes.Buffer
		entry, err := ArchiveEntry(ctx, root, path, &archive)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		} else if snapshotted, err := Snapshot(ctx, root, path); err != nil || !SameContents(entry, snapshotted) {
			t.Fatalf("%s: archived snapshot differs: %v", path, err)
		}
		destination := filepath.Join(t.TempDir(), "copy")
		if extracted, err := ExtractArchive(ctx, &archive, destination); err != nil {
			t.Fatalf("%s: %v", path, err)
		} else if !SameContents(entry, extracted) || entry.Executable != extracted.Executable {
			t.Fatalf("%s: extracted %+v, archived %+v", path, extracted, entry)
		}
	}

	// Directory snapshots cover all contents and executability, but not
	// Mutagen's temporary files.
	original, err := Snapshot(ctx, root, "dir")
	if err != nil {
		t.Fatal(err)
	}
	// Where FIFOs aren't supported, the empty directory is removed instead, so
	// the new file is created elsewhere.
	for _, test := range []struct {
		description string
		change      func() error
	}{
		{"modified file", func() error { return os.WriteFile(filepath.Join(root, "dir", "a"), []byte("A\n"), 0600) }},
		{"executability", func() error { return os.Chmod(filepath.Join(root, "dir", "sub", "b"), 0600) }},
		{"removed fifo", func() error {
			if !fifo {
				return os.Remove(filepath.Join(root, "dir", "empty"))
			}
			return os.Remove(filepath.Join(root, "dir", "fifo"))
		}},
		{"new file", func() error { return os.WriteFile(filepath.Join(root, "dir", "sub", "c"), nil, 0600) }},
	} {
		description, change := test.description, test.change
		if runtime.GOOS == "windows" && description == "executability" {
			continue
		}
		if err := change(); err != nil {
			t.Fatal(err)
		} else if changed, err := Snapshot(ctx, root, "dir"); err != nil || SameContents(changed, original) {
			t.Fatalf("%s not reflected in snapshot: %v", description, err)
		}
		original, _ = Snapshot(ctx, root, "dir")
	}
	if err := os.WriteFile(filepath.Join(root, "dir", ".mutagen-temporary-y"), nil, 0600); err != nil {
		t.Fatal(err)
	} else if unchanged, err := Snapshot(ctx, root, "dir"); err != nil || !SameContents(unchanged, original) {
		t.Fatalf("temporary file reflected in snapshot: %v", err)
	}
}

func TestExtractionConfinement(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir()
	archive := func(headers ...*tar.Header) *bytes.Buffer {
		var buffer bytes.Buffer
		writer := tar.NewWriter(&buffer)
		for _, header := range headers {
			writer.WriteHeader(header)
		}
		writer.Close()
		return &buffer
	}
	top := &tar.Header{Typeflag: tar.TypeDir, Name: "."}
	for description, input := range map[string]*bytes.Buffer{
		"parent reference": archive(top, &tar.Header{Typeflag: tar.TypeReg, Name: "../escaped"}),
		"absolute path":    archive(top, &tar.Header{Typeflag: tar.TypeReg, Name: filepath.ToSlash(filepath.Join(outside, "escaped"))}),
		"link traversal": archive(top, &tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: outside},
			&tar.Header{Typeflag: tar.TypeReg, Name: "link/escaped"}),
		"misnamed top": archive(&tar.Header{Typeflag: tar.TypeReg, Name: "file"}),
		"extra entry":  archive(&tar.Header{Typeflag: tar.TypeReg, Name: "."}, &tar.Header{Typeflag: tar.TypeReg, Name: "file"}),
	} {
		if _, err := ExtractArchive(ctx, input, filepath.Join(t.TempDir(), "copy")); err == nil {
			t.Fatalf("%s accepted", description)
		} else if _, err := os.Lstat(filepath.Join(outside, "escaped")); err == nil {
			t.Fatalf("%s escaped", description)
		}
	}
}

func TestDeleteVerifiedEntries(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"dir/a": "a\n", "dir/sub/b": "b\n", "file": "file\n"})
	deletion := func(path string, expected *Entry) error {
		return ReplaceFile(ctx, root, &Replacement{Path: path, Expected: []*Entry{expected}, Result: &Entry{Kind: EntryMissing}}, nil)
	}
	snapshot, err := Snapshot(ctx, root, "dir")
	if err != nil {
		t.Fatal(err)
	}

	// A directory changed since its snapshot is kept.
	if err := os.WriteFile(filepath.Join(root, "dir", "sub", "c"), []byte("c\n"), 0600); err != nil {
		t.Fatal(err)
	} else if err := deletion("dir", snapshot); !errors.Is(err, ErrStale) {
		t.Fatal("changed directory not detected:", err)
	} else if _, err := os.Stat(filepath.Join(root, "dir", "sub", "c")); err != nil {
		t.Fatal("changed directory modified:", err)
	}

	// An unchanged directory is deleted completely, as is a symbolic link.
	if snapshot, err = Snapshot(ctx, root, "dir"); err != nil {
		t.Fatal(err)
	} else if err := deletion("dir", snapshot); errors.Is(err, errors.ErrUnsupported) {
		// Directories can't be hard linked, so deleting them requires renaming
		// without replacement, which isn't supported everywhere.
		t.Log("deleting directories unsupported:", err)
		if _, err := os.Stat(filepath.Join(root, "dir", "sub", "c")); err != nil {
			t.Fatal("directory modified despite failure:", err)
		}
	} else if err != nil {
		t.Fatal(err)
	} else if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatal("directory not deleted completely:", entries)
	} else if err := deletion("dir", snapshot); err != nil {
		t.Fatal("repeated deletion failed:", err)
	}
	if os.Symlink("file", filepath.Join(root, "link")) == nil {
		if err := deletion("link", &Entry{Kind: EntrySymbolicLink, Target: "other"}); !errors.Is(err, ErrStale) {
			t.Fatal("changed symbolic link not detected:", err)
		} else if err := deletion("link", &Entry{Kind: EntrySymbolicLink, Target: "file"}); err != nil {
			t.Fatal(err)
		} else if _, err := os.Lstat(filepath.Join(root, "link")); !os.IsNotExist(err) {
			t.Fatal("symbolic link not deleted:", err)
		}
	}

	// Directories and symbolic links can only be deleted.
	file, _ := Snapshot(ctx, root, "file")
	for _, replacement := range []*Replacement{
		{Path: "file", Expected: []*Entry{snapshot}, Result: file},
		{Path: "file", Expected: []*Entry{{Kind: EntryDirectory}}, Result: &Entry{Kind: EntryMissing}},
	} {
		if err := ReplaceFile(ctx, root, replacement, nil); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("invalid replacement accepted: %v", err)
		}
	}
}

func TestReplaceRootFile(t *testing.T) {
	// A session's root may be a file, which is replaced within its parent.
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(root, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var copied bytes.Buffer
	original, err := CopyFile(ctx, root, ".", &copied)
	if err != nil || copied.String() != "original\n" {
		t.Fatalf("root not copied: %v", err)
	}
	merged := &Entry{Kind: EntryFile, Size: 7, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("merged\n")))}
	replacement := &Replacement{Path: ".", Expected: []*Entry{original}, Result: merged}

	// Moving the root aside would make synchronization see it deleted, so
	// replacing it requires exchanging files.
	exchangeFiles = func(*os.Root, string, string) error { return errors.ErrUnsupported }
	err = ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n"))
	exchangeFiles = exchange
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal("root replaced without exchanging files:", err)
	} else if data, _ := os.ReadFile(root); string(data) != "original\n" {
		t.Fatal("root changed despite failure")
	}

	err = ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n"))
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("exchanging files unsupported:", err)
	} else if err != nil {
		t.Fatal(err)
	} else if data, _ := os.ReadFile(root); string(data) != "merged\n" {
		t.Fatal("root not replaced")
	} else if entries, _ := os.ReadDir(filepath.Dir(root)); len(entries) != 1 {
		t.Fatal("replacement left files behind:", entries)
	}
	if err := ReplaceFile(ctx, root, &Replacement{Path: ".", Expected: []*Entry{merged}, Result: &Entry{Kind: EntryMissing}}, nil); err == nil {
		t.Fatal("root deleted")
	}
}

func TestCleanUpLeftovers(t *testing.T) {
	// Files left by interrupted replacements are removed if they're copies
	// that the replacement would replace, and are otherwise kept.
	ctx := context.Background()
	root := t.TempDir()
	original := &Entry{Kind: EntryFile, Size: 9, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("original\n")))}
	merged := &Entry{Kind: EntryFile, Size: 7, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("merged\n")))}
	replacement := &Replacement{Path: "file", Expected: []*Entry{original}, Result: merged}
	staged, aside := temporaryName("file", roleStaged), temporaryName("file", roleAside)
	writeTree(t, root, map[string]string{"file": "original\n", staged: "merged\n", aside: "original\n", temporaryName("file", roleProbe): ""})
	if err := ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n")); err != nil {
		t.Fatal(err)
	} else if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatal("leftovers not removed:", entries)
	}
	writeTree(t, root, map[string]string{aside: "saved meanwhile\n"})
	replacement.Expected = []*Entry{merged}
	replacement.Result = &Entry{Kind: EntryMissing}
	if err := ReplaceFile(ctx, root, replacement, nil); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatal("unknown leftover not reported:", err)
	} else if data, _ := os.ReadFile(filepath.Join(root, aside)); string(data) != "saved meanwhile\n" {
		t.Fatal("unknown leftover removed")
	} else if _, err := os.Stat(filepath.Join(root, "file")); err != nil {
		t.Fatal("file changed despite an unknown leftover:", err)
	}
}

func TestRecoverInterruptedReplacement(t *testing.T) {
	ctx := context.Background()
	original := &Entry{Kind: EntryFile, Size: 9, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("original\n")))}
	merged := &Entry{Kind: EntryFile, Size: 7, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("merged\n")))}
	for _, checkFirst := range []bool{false, true} {
		for _, staged := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkFirst=%t/staged=%t", checkFirst, staged), func(t *testing.T) {
				// A replacement can be interrupted after moving the original
				// aside. Normal unwinding may also remove the staged result.
				root := t.TempDir()
				writeTree(t, root, map[string]string{temporaryName("file", roleAside): "original\n"})
				if staged {
					writeTree(t, root, map[string]string{temporaryName("file", roleStaged): "merged\n"})
				}
				replacement := &Replacement{Path: "file", Expected: []*Entry{original}, Result: merged}
				if checkFirst {
					if err := CheckFile(ctx, root, replacement); err != nil {
						t.Fatal("unable to check interrupted replacement:", err)
					} else if data, err := os.ReadFile(filepath.Join(root, "file")); err != nil || string(data) != "original\n" {
						t.Fatalf("original not restored before checking: %q: %v", data, err)
					}
				}
				if err := ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n")); err != nil {
					t.Fatal("unable to retry interrupted replacement:", err)
				} else if data, err := os.ReadFile(filepath.Join(root, "file")); err != nil || string(data) != "merged\n" {
					t.Fatalf("replacement not completed: %q: %v", data, err)
				} else if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
					t.Fatalf("replacement left temporary files: %v: %v", entries, err)
				}
			})
		}
	}

	// Recovery must keep a copy saved at the path since the interruption.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"file": "saved meanwhile\n", temporaryName("file", roleAside): "original\n", temporaryName("file", roleStaged): "merged\n",
	})
	replacement := &Replacement{Path: "file", Expected: []*Entry{original}, Result: merged}
	if err := ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n")); !errors.Is(err, ErrStale) {
		t.Fatal("concurrent save not detected:", err)
	} else if data, err := os.ReadFile(filepath.Join(root, "file")); err != nil || string(data) != "saved meanwhile\n" {
		t.Fatalf("concurrent save overwritten: %q: %v", data, err)
	}

	// A deletion already moved the original out of the way intentionally.
	root = t.TempDir()
	writeTree(t, root, map[string]string{temporaryName("file", roleAside): "original\n"})
	replacement.Result = &Entry{Kind: EntryMissing}
	if err := ReplaceFile(ctx, root, replacement, nil); err != nil {
		t.Fatal("unable to finish interrupted deletion:", err)
	} else if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("deletion resurrected a copy or left temporary files: %v: %v", entries, err)
	}
}
