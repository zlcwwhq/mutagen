package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// readTree reads the files beneath a directory, with symbolic links read as
// their targets prefixed by "-> ".
func readTree(t *testing.T, directory string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		relative, _ := filepath.Rel(directory, path)
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			result[filepath.ToSlash(relative)] = "-> " + target
			return err
		}
		data, err := os.ReadFile(path)
		result[filepath.ToSlash(relative)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// writeFiles creates files with the specified contents beneath a directory.
func writeFiles(t *testing.T, directory string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPullMerge(t *testing.T) {
	staged, destination := t.TempDir(), t.TempDir()
	writeFiles(t, staged, map[string]string{
		"same": "same\n", "changed": "new\n", "added/a": "a\n", "sub/b": "b\n", "file-to-directory/c": "c\n", "directory-to-file": "d\n",
	})
	writeFiles(t, destination, map[string]string{
		"same": "same\n", "changed": "old\n", "local-only": "kept\n", "sub/local": "kept\n",
		"file-to-directory": "file\n", "directory-to-file/e": "e\n",
	})
	links := runtime.GOOS != "windows"
	if links {
		os.Symlink("same", filepath.Join(staged, "link"))
		os.Symlink("other", filepath.Join(destination, "link"))
	}

	// Differing files are replaced and missing ones added, while local
	// content is never deleted, and kind mismatches are reported.
	summary := &pullSummary{}
	summary.merge(staged, destination, "results")
	expected := map[string]string{
		"same": "same\n", "changed": "new\n", "added/a": "a\n", "sub/b": "b\n", "sub/local": "kept\n", "local-only": "kept\n",
		"file-to-directory": "file\n", "directory-to-file/e": "e\n",
	}
	added, replaced := 2, 1
	if links {
		expected["link"] = "-> same"
		replaced++
	}
	if tree := readTree(t, destination); len(tree) != len(expected) {
		t.Fatalf("incorrect merge: %v", tree)
	} else {
		for name, data := range expected {
			if tree[name] != data {
				t.Errorf("incorrect merge of %s: %q", name, tree[name])
			}
		}
	}
	if summary.added != added || summary.replaced != replaced || summary.unchanged != 1 {
		t.Errorf("incorrect summary: %s", summary)
	} else if problems := strings.Join(summary.problems, "\n"); !strings.Contains(problems, "results/file-to-directory exists locally but isn't a directory") ||
		!strings.Contains(problems, "results/directory-to-file exists locally as a directory") || len(summary.problems) != 2 {
		t.Errorf("incorrect problems: %q", summary.problems)
	}

	// Executability differences replace files.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(staged, "same"), 0700); err != nil {
			t.Fatal(err)
		}
		summary = &pullSummary{}
		summary.merge(filepath.Join(staged, "same"), filepath.Join(destination, "same"), "same")
		if summary.replaced != 1 {
			t.Errorf("file differing in executability not replaced: %s", summary)
		}
	}
}

func TestPullParents(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"src/a.py": "", "file": ""})
	target := &remote.Target{
		Root: root, Contained: true,
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results/", "*.log", "linked"}},
	}
	locator := newLocator(target)

	// Ignored parents are created, while synchronized ones aren't.
	if err := prepareParents(target, locator, "results/run1/metrics.json"); err != nil {
		t.Fatal("ignored parents not created:", err)
	} else if info, err := os.Stat(filepath.Join(root, "results", "run1")); err != nil || !info.IsDir() {
		t.Fatal("ignored parents missing:", err)
	} else if err := prepareParents(target, locator, "src/train.log"); err != nil {
		t.Fatal("existing parent rejected:", err)
	} else if err := prepareParents(target, locator, "missing/train.log"); err == nil || !strings.Contains(err.Error(), "synchronizes it") {
		t.Fatal("synchronized parent created:", err)
	} else if err := prepareParents(target, locator, "file/train.log"); err == nil || !strings.Contains(err.Error(), "isn't a directory") {
		t.Fatal("file accepted as a parent:", err)
	}

	// Symbolic links are only followed if they're ignored.
	if runtime.GOOS == "windows" {
		return
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	} else if err := os.Symlink(outside, filepath.Join(root, "synchronized")); err != nil {
		t.Fatal(err)
	}
	if err := prepareParents(target, locator, "linked/train.log"); err != nil {
		t.Fatal("ignored symbolic link not followed:", err)
	} else if err := prepareParents(target, locator, "synchronized/train.log"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatal("synchronized symbolic link followed:", err)
	}
}
