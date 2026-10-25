package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// setPullModificationTime sets a file's time and returns its actual value,
// accounting for the precision supported by the filesystem.
func setPullModificationTime(t *testing.T, path string, mtime time.Time) time.Time {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func TestPullModificationTimes(t *testing.T) {
	alpha, pullPaths := setupPullSessions(t,
		map[string]string{"results/replaced": "old", "results/metadata": "same"},
		map[string]string{"results/replaced": "new", "results/metadata": "same", "results/sub/added": "added"},
	)
	beta := filepath.Join(filepath.Dir(alpha), "beta")
	mtime := time.Unix(1700000000, 876543210)
	expected := make(map[string]time.Time)
	for _, path := range []string{"results/replaced", "results/metadata", "results/sub/added"} {
		expected[path] = setPullModificationTime(t, filepath.Join(beta, filepath.FromSlash(path)), mtime).Truncate(time.Second)
	}
	setPullModificationTime(t, filepath.Join(alpha, "results", "metadata"), mtime.Add(-time.Hour))

	// New and replaced files, including nested ones, get the remote time.
	// Equal contents with a different time are counted separately.
	if output, err := pullPaths("results"); err != nil || output != "remote: pulled results (1 added, 1 replaced, 1 metadata updated, 0 unchanged)\n" {
		t.Fatalf("incorrect initial pull: %q, %v", output, err)
	}
	for path, expected := range expected {
		if info, err := os.Stat(filepath.Join(alpha, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		} else if !info.ModTime().Equal(expected) {
			t.Errorf("%s: modification time %v, expected %v", path, info.ModTime(), expected)
		}
	}
	// Both recursive pulls and a single-file pull are stable on repetition.
	if output, err := pullPaths("results", "results/metadata"); err != nil || output !=
		"remote: pulled results (0 added, 0 replaced, 3 unchanged)\nremote: pulled results/metadata (0 added, 0 replaced, 1 unchanged)\n" {
		t.Fatalf("repeated pull changed files: %q, %v", output, err)
	}
	// A remote timestamp change also updates an otherwise identical file.
	setPullModificationTime(t, filepath.Join(beta, "results", "metadata"), mtime.Add(time.Hour))
	if output, err := pullPaths("results/metadata"); err != nil || output !=
		"remote: pulled results/metadata (0 added, 0 replaced, 1 metadata updated, 0 unchanged)\n" {
		t.Fatalf("remote timestamp change not applied: %q, %v", output, err)
	}
}

func TestPullModificationTimeLinks(t *testing.T) {
	for _, symbolic := range []bool{false, true} {
		name := "hard"
		if symbolic {
			name = "symbolic"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			writeFiles(t, directory, map[string]string{"staged": "same", "original": "same"})
			source, original, destination := filepath.Join(directory, "staged"), filepath.Join(directory, "original"), filepath.Join(directory, "destination")
			expected := setPullModificationTime(t, source, time.Unix(1700000000, 123456789))
			originalTime := setPullModificationTime(t, original, time.Unix(1600000000, 876543210))
			var err error
			if symbolic {
				err = os.Symlink(original, destination)
			} else {
				err = os.Link(original, destination)
			}
			if err != nil {
				t.Skipf("%s links unavailable: %v", name, err)
			}
			before, err := os.Stat(original)
			if err != nil {
				t.Fatal(err)
			}
			summary := &pullSummary{}
			summary.merge(source, destination, "destination")
			if len(summary.problems) != 0 || summary.added != 0 || summary.unchanged != 0 ||
				(symbolic && (summary.replaced != 1 || summary.metadataUpdated != 0)) ||
				(!symbolic && (summary.replaced != 0 || summary.metadataUpdated != 1)) {
				t.Fatalf("incorrect merge: %s, %v", summary, summary.problems)
			}
			if info, err := os.Lstat(destination); err != nil {
				t.Fatal(err)
			} else if !info.Mode().IsRegular() || !info.ModTime().Equal(expected) || os.SameFile(info, before) {
				t.Fatal("destination not replaced by the staged regular file")
			}
			if info, err := os.Stat(original); err != nil {
				t.Fatal(err)
			} else if !info.ModTime().Equal(originalTime) || !os.SameFile(info, before) {
				t.Fatal("original inode or timestamp changed")
			} else if data, err := os.ReadFile(original); err != nil || string(data) != "same" {
				t.Fatalf("original contents changed: %q, %v", data, err)
			}
		})
	}
}

func TestPullMetadataUpdatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows only records whether files are read-only")
	}
	// A metadata-only update keeps the local file's permissions, even when
	// they're stricter than the staged copy's default ones, while a content
	// change takes the staged copy's permissions as before.
	for _, test := range []struct {
		contents string
		expected os.FileMode
	}{{"same", 0600}, {"different", 0644}} {
		directory := t.TempDir()
		source, destination := filepath.Join(directory, "staged"), filepath.Join(directory, "destination")
		writeFiles(t, directory, map[string]string{"staged": test.contents, "destination": "same"})
		for path, mode := range map[string]os.FileMode{source: 0644, destination: 0600} {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
		setPullModificationTime(t, source, time.Unix(1700000000, 0))
		summary := &pullSummary{}
		summary.merge(source, destination, "destination")
		if len(summary.problems) != 0 || summary.metadataUpdated+summary.replaced != 1 {
			t.Fatalf("incorrect merge of %q: %s, %v", test.contents, summary, summary.problems)
		} else if info, err := os.Stat(destination); err != nil {
			t.Fatal(err)
		} else if info.Mode().Perm() != test.expected {
			t.Errorf("%q: permissions %v, expected %v", test.contents, info.Mode().Perm(), test.expected)
		}
	}
}
