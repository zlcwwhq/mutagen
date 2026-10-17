package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCopyAndReplace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	original := bytes.Repeat([]byte("large file\n"), 200000)
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	route := func() net.Conn {
		client, server := net.Pipe()
		go Serve(ctx, server, root, "")
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
	replace := func(expected []*Entry, result *Entry, input string) error {
		return Replace(ctx, route(), &Replacement{Path: "file", Expected: expected, Result: result}, strings.NewReader(input))
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
	} else if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatal("replacement left files behind:", entries)
	}

	// Replacement is idempotent, even without contents, but fails if the file
	// matches neither an expected entry nor the result.
	if err := replace([]*Entry{entry}, result, ""); err != nil {
		t.Fatal(err)
	}
	if err := replace([]*Entry{entry, other}, &Entry{Kind: EntryMissing}, ""); err == nil || !strings.Contains(err.Error(), "changed since") {
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
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "file")
	unrelated := &Entry{Kind: EntryFile, Size: 1, Digest: strings.Repeat("0", 64)}
	for _, test := range []struct {
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
		replacement := &Replacement{Path: "file", Expected: []*Entry{unrelated, test.expected(current)}, Result: result}
		if err := ReplaceFile(ctx, root, replacement, strings.NewReader("merged\n")); err != nil {
			t.Fatalf("%s: %v", test.description, err)
		} else if data, _ := os.ReadFile(path); string(data) != "merged\n" {
			t.Fatalf("%s: result not written", test.description)
		} else if info, err := os.Stat(path); runtime.GOOS != "windows" && (err != nil || (info.Mode()&0100 != 0) != test.executable) {
			t.Fatalf("%s: executability not set: %v", test.description, err)
		}
	}
}

func TestSupersedeKeepsConcurrentSaves(t *testing.T) {
	// A file saved after the last check but before it's replaced or deleted
	// must survive, rather than be overwritten silently.
	for name, save := range map[string]func(path string) error{
		"rename": func(path string) error {
			if err := os.WriteFile(path+".new", []byte("saved\n"), 0600); err != nil {
				return err
			}
			return os.Rename(path+".new", path)
		},
		"in place": func(path string) error {
			return os.WriteFile(path, []byte("saved\n"), 0600)
		},
	} {
		for _, staged := range []string{".mutagen-temporary-staged", ""} {
			directory := t.TempDir()
			path := filepath.Join(directory, "file")
			if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
				t.Fatal(err)
			} else if staged != "" {
				if err := os.WriteFile(filepath.Join(directory, staged), []byte("result\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			checked, err := root.Lstat("file")
			if err != nil {
				t.Fatal(err)
			}
			if err := save(path); err != nil {
				t.Fatal(err)
			}
			if err := supersede(root, "file", checked, staged); !errors.Is(err, errStale) {
				t.Fatalf("%s save (staged %q) not detected: %v", name, staged, err)
			} else if data, _ := os.ReadFile(path); string(data) != "saved\n" {
				t.Fatalf("%s save (staged %q) lost", name, staged)
			} else if entries, _ := os.ReadDir(directory); len(entries) != 1+min(len(staged), 1) {
				t.Fatalf("%s save (staged %q) left files behind: %v", name, staged, entries)
			}
		}
	}

	// Files are never placed over one created concurrently.
	directory := t.TempDir()
	for name, data := range map[string]string{"from": "from\n", "to": "to\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := place(root, "from", "to"); !errors.Is(err, fs.ErrExist) {
		t.Fatal("existing file not detected:", err)
	} else if data, _ := os.ReadFile(filepath.Join(directory, "to")); string(data) != "to\n" {
		t.Fatal("existing file replaced")
	}

	// Neither are they renamed over one where hard links aren't supported.
	if err := renameNoReplace(root, "from", "to"); errors.Is(err, errors.ErrUnsupported) {
		t.Log("renaming without replacement unsupported")
	} else if !errors.Is(err, fs.ErrExist) {
		t.Fatal("existing file not detected when renaming:", err)
	} else if data, _ := os.ReadFile(filepath.Join(directory, "to")); string(data) != "to\n" {
		t.Fatal("existing file replaced when renaming")
	} else if err := renameNoReplace(root, "from", "placed"); err != nil {
		t.Fatal("unable to rename without replacement:", err)
	} else if data, _ := os.ReadFile(filepath.Join(directory, "placed")); string(data) != "from\n" {
		t.Fatal("file not renamed")
	}
}

func TestReplaceCompletedConcurrently(t *testing.T) {
	// Synchronization may propagate the result after the last check but before
	// the file is replaced or deleted, which completes the replacement rather
	// than making it stale.
	ctx := context.Background()
	original := &Entry{Kind: EntryFile, Size: 9, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("original\n")))}
	merged := &Entry{Kind: EntryFile, Size: 7, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("merged\n")))}
	for _, result := range []*Entry{merged, {Kind: EntryMissing}} {
		directory := t.TempDir()
		path := filepath.Join(directory, "file")
		staged := ""
		if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
			t.Fatal(err)
		} else if result.Kind == EntryFile {
			staged = ".mutagen-temporary-staged"
			if err := os.WriteFile(filepath.Join(directory, staged), []byte("merged\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		checked, err := root.Lstat("file")
		if err != nil {
			t.Fatal(err)
		}
		if result.Kind == EntryMissing {
			err = os.Remove(path)
		} else if err = os.WriteFile(path+".new", []byte("merged\n"), 0600); err == nil {
			err = os.Rename(path+".new", path)
		}
		if err != nil {
			t.Fatal(err)
		}
		replacement := &Replacement{Path: "file", Expected: []*Entry{original}, Result: result}
		if err := replacement.replace(ctx, root, "file", checked, staged); err != nil {
			t.Fatalf("propagated %s result treated as stale: %v", result.Kind, err)
		} else if current, err := ReadEntry(ctx, path); err != nil || !SameFile(current, result) {
			t.Fatalf("propagated %s result not kept: %v", result.Kind, err)
		} else if entries, _ := os.ReadDir(directory); len(entries) != 2*min(len(staged), 1) {
			t.Fatalf("propagated %s result left files behind: %v", result.Kind, entries)
		}
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
		{Copy: "."}, {Copy: "../file"}, {Replace: &Replacement{}},
		{Copy: "file", Arguments: []string{"echo"}}, {Copy: "file", File: "file"},
	} {
		if request.EnsureValid() == nil {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
}
