package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
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
		"duplicate file": archive(top, &tar.Header{Typeflag: tar.TypeReg, Name: "file"},
			&tar.Header{Typeflag: tar.TypeReg, Name: "file"}),
		"duplicate directory": archive(top, &tar.Header{Typeflag: tar.TypeDir, Name: "dir"},
			&tar.Header{Typeflag: tar.TypeDir, Name: "dir"}),
		"missing parent": archive(top, &tar.Header{Typeflag: tar.TypeReg, Name: "missing/file"}),
		"file parent": archive(top, &tar.Header{Typeflag: tar.TypeReg, Name: "file"},
			&tar.Header{Typeflag: tar.TypeReg, Name: "file/child"}),
	} {
		if _, err := ExtractArchive(ctx, input, filepath.Join(t.TempDir(), "copy")); err == nil {
			t.Fatalf("%s accepted", description)
		} else if ClassifyError(err) != ErrorKindConnection {
			t.Fatalf("%s not classified as protocol failure: %v", description, err)
		} else if _, err := os.Lstat(filepath.Join(outside, "escaped")); err == nil {
			t.Fatalf("%s escaped", description)
		}
	}
}

func TestExtractArchivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions aren't represented on Windows")
	}
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"dir/plain": "plain\n", "dir/sub/tool": "tool\n"})
	if err := os.Chmod(filepath.Join(root, "dir", "sub", "tool"), 0700); err != nil {
		t.Fatal(err)
	}

	// The permissions that the umask leaves of those requested by programs.
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Mkdir(probe, 0777); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	umasked := info.Mode().Perm()

	// Private extraction ignores the umask, while shared extraction applies it.
	for _, test := range []struct {
		extract             func(context.Context, io.Reader, string) (*Entry, error)
		file, tool, subtree os.FileMode
	}{
		{ExtractArchive, 0600, 0700, 0700},
		{ExtractSharedArchive, umasked & 0666, umasked, umasked},
	} {
		var archive bytes.Buffer
		if _, err := ArchiveEntry(ctx, root, "dir", &archive); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "copy")
		if _, err := test.extract(ctx, &archive, destination); err != nil {
			t.Fatal(err)
		}
		for path, expected := range map[string]os.FileMode{"plain": test.file, "sub/tool": test.tool, "sub": test.subtree, ".": test.subtree} {
			if info, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(path))); err != nil {
				t.Fatal(err)
			} else if info.Mode().Perm() != expected {
				t.Errorf("incorrect permissions of %s: %v, expected %v", path, info.Mode().Perm(), expected)
			}
		}
	}
}
