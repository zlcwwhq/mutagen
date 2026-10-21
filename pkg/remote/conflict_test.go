package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCopyAndReplace(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "file")
	original := bytes.Repeat([]byte("large file\n"), 200000)
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	// File operations must remain available even with an invalid override.
	if err := os.Mkdir(filepath.Join(root, workspacePrologueName), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	route := func() net.Conn {
		client, server := net.Pipe()
		go Serve(ctx, server, root, "", false)
		return client
	}

	// Copies are complete, unlike entries read for comparison.
	var body bytes.Buffer
	entry, err := Copy(ctx, route(), "file", &body)
	if err != nil || !bytes.Equal(body.Bytes(), original) || entry.Size <= MaximumComparedFileSize {
		t.Fatalf("incomplete copy: %v", err)
	}
	merged := filepath.Join(t.TempDir(), "merged")
	if err := os.WriteFile(merged, []byte("merged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadEntry(ctx, merged)
	if err != nil {
		t.Fatal(err)
	}
	result.Executable = true
	var sequence uint64
	replace := func(expected []*Entry, result *Entry, input string) error {
		sequence++
		replacement := &Replacement{Path: "file", Expected: expected, Result: result, Sequence: sequence}
		return Replace(ctx, route(), replacement, strings.NewReader(input))
	}
	// other stands in for the opposite endpoint's checked-out copy, which may
	// be listed as expected in either order.
	other := &Entry{Kind: EntryFile, Size: 1, Digest: strings.Repeat("0", 64)}

	// Corrupt contents must leave the original untouched.
	if err := replace([]*Entry{entry}, result, "bad"); err == nil {
		t.Fatal("corrupt contents accepted")
	} else if data, _ := os.ReadFile(path); !bytes.Equal(data, original) {
		t.Fatal("corrupt contents modified the original")
	}
	if err := replace([]*Entry{other, entry}, result, "merged\n"); err != nil {
		t.Fatal(err)
	} else if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0750) {
		t.Fatalf("permissions not preserved apart from executability: %v", err)
	} else if entries, _ := os.ReadDir(root); len(entries) != 2 {
		t.Fatal("replacement left files behind:", entries)
	}

	// Replacement is idempotent, even without contents, but fails if the file
	// matches neither an expected entry nor the result.
	if err := replace([]*Entry{entry}, result, ""); err != nil {
		t.Fatal(err)
	}
	if err := replace([]*Entry{entry, other}, &Entry{Kind: EntryMissing}, ""); !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("stale deletion accepted: %v", err)
	}
	if err := replace(nil, &Entry{Kind: EntryMissing}, ""); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("replacement without expected files accepted: %v", err)
	}
	if err := replace([]*Entry{other, result}, &Entry{Kind: EntryMissing}, ""); err != nil {
		t.Fatal(err)
	} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("file not deleted")
	}
	if err := replace([]*Entry{result}, &Entry{Kind: EntryMissing}, ""); err != nil {
		t.Fatal("repeated deletion failed:", err)
	}
	if entry, err := Copy(ctx, route(), "file", io.Discard); err != nil || entry.Kind != EntryMissing {
		t.Fatalf("missing file not reported: %v", err)
	}
}

func TestReplaceIgnoresUnpreservedExecutability(t *testing.T) {
	// Filesystems that don't preserve executability, such as those on Windows,
	// report files as not executable (or as executable) regardless of how they
	// were written or synchronized, which must not make retries fail as stale.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "file")
	unrelated := &Entry{Kind: EntryFile, Size: 1, Digest: strings.Repeat("0", 64)}
	for s, test := range []struct {
		description, contents string
		expected              func(*Entry) *Entry
		executable            bool
	}{
		{"result written without executability", "merged\n", func(*Entry) *Entry { return unrelated }, true},
		{"expected copy with different executability", "local\n", func(current *Entry) *Entry {
			return &Entry{Kind: EntryFile, Size: current.Size, Digest: current.Digest, Executable: true}
		}, false},
	} {
		if err := os.WriteFile(path, []byte(test.contents), 0600); err != nil {
			t.Fatal(err)
		}
		current, err := ReadEntry(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		result := &Entry{Kind: EntryFile, Size: 7, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("merged\n"))), Executable: test.executable}
		replacement := &Replacement{Path: "file", Expected: []*Entry{unrelated, test.expected(current)}, Result: result, Sequence: uint64(s + 1)}
		if err := ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n")); err != nil {
			t.Fatalf("%s: %v", test.description, err)
		} else if data, _ := os.ReadFile(path); string(data) != "merged\n" {
			t.Fatalf("%s: result not written", test.description)
		} else if info, err := os.Stat(path); runtime.GOOS != "windows" && (err != nil || (info.Mode()&0100 != 0) != test.executable) {
			t.Fatalf("%s: executability not set: %v", test.description, err)
		}
	}
}

// contentsEntry describes a file with the specified contents.
func contentsEntry(contents string) *Entry {
	return &Entry{Kind: EntryFile, Size: int64(len(contents)), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(contents)))}
}

func TestReplaceKinds(t *testing.T) {
	// Expected entries of any kind other than directories can be replaced with
	// a file or a symbolic link, or deleted, without leaving temporary files.
	// Directories are never expected, so they're stale, like unexpected files.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx := context.Background()
	kinds := []struct {
		kind  string
		entry *Entry
		setup func(path string) error
	}{
		{EntryFile, contentsEntry("file\n"), func(path string) error { return os.WriteFile(path, []byte("file\n"), 0600) }},
		{EntrySymbolicLink, &Entry{Kind: EntrySymbolicLink, Target: "target"}, func(path string) error { return os.Symlink("target", path) }},
		{EntryOther, &Entry{Kind: EntryOther}, mkfifo},
		{EntryMissing, &Entry{Kind: EntryMissing}, func(string) error { return nil }},
		{EntryDirectory, contentsEntry("directory\n"), func(path string) error { return os.Mkdir(path, 0700) }},
		{"unexpected file", contentsEntry("expected\n"), func(path string) error { return os.WriteFile(path, []byte("unexpected\n"), 0600) }},
	}
	results := []*Entry{contentsEntry("result\n"), {Kind: EntrySymbolicLink, Target: "result"}, {Kind: EntryMissing}}
	for _, current := range kinds {
		for _, result := range results {
			t.Run(fmt.Sprintf("%s to %s", current.kind, result.Kind), func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "entry")
				if err := current.setup(path); err != nil {
					t.Skipf("%s unavailable: %v", current.kind, err)
				}
				before, err := ReadEntry(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				replacement := &Replacement{Path: "entry", Expected: []*Entry{current.entry}, Result: result, Sequence: 1}
				err = ReplaceFile(ctx, directory, replacement, strings.NewReader("result\n"))
				after, readErr := ReadEntry(ctx, path)
				if readErr != nil {
					t.Fatal(readErr)
				} else if entries, _ := os.ReadDir(directory); len(entries) > 1 {
					t.Fatal("replacement left files behind:", entries)
				}
				if current.kind == EntryDirectory || current.kind == "unexpected file" {
					if !errors.Is(err, ErrStale) {
						t.Fatalf("unexpected entry replaced: %v", err)
					} else if after.Kind != before.Kind || (after.Kind == EntryFile && !SameFile(after, before)) {
						t.Fatalf("unexpected entry changed: %+v", after)
					}
				} else if err != nil {
					t.Fatal(err)
				} else if !SameContents(after, result) {
					t.Fatalf("result not written: %+v", after)
				}
			})
		}
	}

	// Directories can be neither expected nor results, symbolic links need
	// targets, and replacements need sequence numbers.
	file := contentsEntry("file\n")
	for _, replacement := range []*Replacement{
		{Path: "entry", Expected: []*Entry{{Kind: EntryDirectory, Digest: "digest"}}, Result: &Entry{Kind: EntryMissing}, Sequence: 1},
		{Path: "entry", Expected: []*Entry{file}, Result: &Entry{Kind: EntryDirectory, Digest: "digest"}, Sequence: 1},
		{Path: "entry", Expected: []*Entry{file}, Result: &Entry{Kind: EntrySymbolicLink}, Sequence: 1},
		{Path: "entry", Expected: []*Entry{nil}, Result: file, Sequence: 1},
		{Path: "entry", Expected: []*Entry{file}, Result: &Entry{Kind: EntryMissing}},
	} {
		if err := ReplaceFile(ctx, t.TempDir(), replacement, nil); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("invalid replacement accepted: %v", err)
		}
	}
}

func TestReplaceRoot(t *testing.T) {
	// A single-file session's root is replaced in its parent directory, but it
	// can't be deleted or replaced with a symbolic link, which synchronization
	// would refuse.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(root, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original := contentsEntry("original\n")
	for _, result := range []*Entry{{Kind: EntryMissing}, {Kind: EntrySymbolicLink, Target: "target"}} {
		replacement := &Replacement{Path: ".", Expected: []*Entry{original}, Result: result, Sequence: 1}
		if err := ReplaceFile(ctx, root, replacement, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "only be replaced with a file") {
			t.Fatalf("root replaced with %s: %v", result.Kind, err)
		}
	}
	replacement := &Replacement{Path: ".", Expected: []*Entry{original}, Result: contentsEntry("merged\n"), Sequence: 1}
	if err := ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n")); err != nil {
		t.Fatal(err)
	} else if data, _ := os.ReadFile(root); string(data) != "merged\n" {
		t.Fatalf("root not replaced: %q", data)
	} else if entries, _ := os.ReadDir(filepath.Dir(root)); len(entries) != 1 {
		t.Fatal("replacement left files behind:", entries)
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(data []byte) (int, error) {
	return f(data)
}

func TestCopyDetectsConcurrentChanges(t *testing.T) {
	// Editors may save by renaming a new file over the original, which leaves
	// the opened file intact, or may modify the file in place.
	for name, change := range map[string]func(root string) error{
		"rename": func(root string) error {
			saved := filepath.Join(root, "saved")
			if err := os.WriteFile(saved, []byte("saved\n"), 0600); err != nil {
				return err
			}
			return os.Rename(saved, filepath.Join(root, "file"))
		},
		"in place": func(root string) error {
			file, err := os.OpenFile(filepath.Join(root, "file"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			_, err = file.WriteString("appended\n")
			return errors.Join(err, file.Close())
		},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "rename" && runtime.GOOS == "windows" {
				t.Skip("open files can't be replaced on Windows")
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("original\n"), 0600); err != nil {
				t.Fatal(err)
			}
			changed := false
			output := writerFunc(func(data []byte) (int, error) {
				if !changed {
					changed = true
					if err := change(root); err != nil {
						return 0, err
					}
				}
				return len(data), nil
			})
			if _, err := CopyFile(context.Background(), root, "file", output); err == nil || !strings.Contains(err.Error(), "changed while") {
				t.Fatalf("change while reading not detected: %v", err)
			}
		})
	}
}

func TestFileOperationConfinement(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symbolic links unavailable:", err)
	}
	os.Symlink(filepath.Join(outside, "file"), filepath.Join(root, "file"))
	for _, path := range []string{"../file", "link/file", "file"} {
		if _, err := CopyFile(context.Background(), root, path, io.Discard); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
	for _, request := range []*Request{
		{Archive: "."}, {Copy: "../file"}, {Replace: &Replacement{}}, {Copy: "file", Archive: "file"},
		{Copy: "file", Arguments: []string{"echo"}}, {Copy: "file", File: "file"},
	} {
		if request.EnsureValid() == nil {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
}

func TestSyncDirectory(t *testing.T) {
	// Directories are flushed, which does nothing on Windows, but missing
	// directories can't be.
	directory := t.TempDir()
	if err := SyncDirectory(directory); err != nil {
		t.Fatal(err)
	} else if err := SyncDirectory(filepath.Join(directory, "missing")); runtime.GOOS != "windows" && err == nil {
		t.Fatal("missing directory flushed")
	}
}
