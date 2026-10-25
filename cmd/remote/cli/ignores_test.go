package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// pinnedRouter creates a router pinned to a target, which locates it without
// consulting the registry.
func pinnedRouter(target *remote.Target, stderr *strings.Builder) *router {
	return &router{ctx: context.Background(), selection: &remote.Selection{Target: target}, stderr: stderr}
}

func TestPrintIgnores(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub", "results"), 0700); err != nil {
		t.Fatal(err)
	}
	target := &remote.Target{
		Session: "sync_x", Name: "gpu", Root: root, Path: "sub", Contained: true,
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results/", "*.log"}, VCS: true},
	}

	// The ignores are listed without paths.
	var output, errorOutput strings.Builder
	if code, err := printIgnores(pinnedRouter(target, &errorOutput), nil, &output); err != nil || code != 0 {
		t.Fatalf("listing failed: %d, %v", code, err)
	} else if !strings.HasPrefix(output.String(), "Session gpu ignores (Mutagen syntax):\n  results/\n  *.log\n  VCS directories") {
		t.Fatalf("incorrect listing: %q", output.String())
	}

	// Paths are checked as the kind of entry that they are locally, or as both
	// kinds if they don't exist locally, and the exit status reports whether
	// any of them is ignored.
	output.Reset()
	code, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"results", "results/run1.json", "train.py", "missing", "../other.log", ".."}, &output)
	if err != nil || code != 1 {
		t.Fatalf("checking failed: %d, %v", code, err)
	}
	expected := "" +
		"results: ignored\n" +
		"results/run1.json: not present locally; if a file or directory: ignored (inside results, which the session ignores)\n" +
		"train.py: not present locally; if a file or directory: synchronized\n" +
		"missing: not present locally; if a file or directory: synchronized\n" +
		"../other.log: not present locally; if a file or directory: ignored\n" +
		"..: synchronized (the session's root)\n"
	if output.String() != expected {
		t.Fatalf("incorrect checks:\n%s", output.String())
	}
	output.Reset()
	target.Ignores.Patterns = []string{"build/"}
	if code, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"build", "train.py"}, &output); err != nil || code != 1 {
		t.Fatalf("checking failed: %d, %v", code, err)
	} else if output.String() != "build: not present locally; if a directory: ignored; if a file: synchronized\ntrain.py: not present locally; if a file or directory: synchronized\n" {
		t.Fatalf("incorrect checks: %q", output.String())
	}
	if code, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"train.py"}, &output); err != nil || code != 0 {
		t.Fatalf("incorrect status without ignored paths: %d, %v", code, err)
	} else if _, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"../.."}, &output); err == nil {
		t.Fatal("path outside the root accepted")
	}

	// Sessions may ignore nothing.
	output.Reset()
	target.Ignores = &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen}
	if _, err := printIgnores(pinnedRouter(target, &errorOutput), nil, &output); err != nil || output.String() != "Session gpu ignores nothing.\n" {
		t.Fatalf("incorrect listing: %q, %v", output.String(), err)
	}
}

func TestPrintIgnoresAsPulled(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	} else if err := os.MkdirAll(filepath.Join(root, "results"), 0700); err != nil {
		t.Fatal(err)
	}
	composed, decomposed := "caf\u00e9", "cafe\u0301"
	target := &remote.Target{
		Session: "sync_x", Root: root, Contained: true,
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results/", "into-*", composed + ".txt", "caf*.log"}},
	}
	check := func(paths []string, expectedCode int, expected string) {
		t.Helper()
		var output, errorOutput strings.Builder
		if code, err := printIgnores(pinnedRouter(target, &errorOutput), paths, &output); err != nil || code != expectedCode {
			t.Fatalf("incorrect check result: %d, %v", code, err)
		} else if output.String() != expected {
			t.Fatalf("incorrect checks:\n%s", output.String())
		}
	}

	// Paths are only ignored if the session ignores them in every form in
	// which scans could see them, as remote --pull requires, so literal
	// patterns that match only one form of a name don't ignore it.
	check([]string{composed + ".txt", decomposed + ".txt", decomposed + ".log"}, 1, ""+
		composed+".txt: not present locally; if a file or directory: possibly synchronized (the session ignores it in only some Unicode normalization forms of its path)\n"+
		decomposed+".txt: not present locally; if a file or directory: possibly synchronized (the session ignores it in only some Unicode normalization forms of its path)\n"+
		decomposed+".log: not present locally; if a file or directory: ignored\n")
	check([]string{decomposed + ".txt"}, 0, decomposed+".txt: not present locally; if a file or directory: possibly synchronized (the session ignores it in only some Unicode normalization forms of its path)\n")

	// Paths that resolve through ignored symbolic links to synchronized paths
	// inside the root are synchronized as those paths.
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink("src", filepath.Join(root, "into-src")); err != nil {
		t.Fatal(err)
	} else if err := os.Symlink("results", filepath.Join(root, "into-results")); err != nil {
		t.Fatal(err)
	} else if err := os.Symlink(t.TempDir(), filepath.Join(root, "into-outside")); err != nil {
		t.Fatal(err)
	}
	check([]string{"into-src/a.py", "into-src/missing/a.py", "into-src/" + decomposed + ".txt"}, 0, ""+
		"into-src/a.py: not present locally; if a file or directory: synchronized as src/a.py (through a symbolic link)\n"+
		"into-src/missing/a.py: not present locally; if a file or directory: synchronized as src/missing/a.py (through a symbolic link)\n"+
		"into-src/"+decomposed+".txt: not present locally; if a file or directory: possibly synchronized as src/"+decomposed+".txt (through a symbolic link; the session ignores it in only some Unicode normalization forms of its path)\n")
	check([]string{"into-results/run1.json", "into-outside/run1.json", "into-src"}, 1, ""+
		"into-results/run1.json: not present locally; if a file or directory: ignored (inside into-results, which the session ignores)\n"+
		"into-outside/run1.json: not present locally; if a file or directory: ignored (inside into-outside, which the session ignores)\n"+
		"into-src: ignored\n")
}

func TestPrintIgnoresExistingTypesAndAccessError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	} else if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	target := &remote.Target{Root: root, Contained: true,
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"*/"}}}
	var output, errorOutput strings.Builder
	if code, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"file", "directory", "missing"}, &output); err != nil || code != 1 {
		t.Fatalf("incorrect result: %d, %v", code, err)
	} else if output.String() != "file: synchronized\ndirectory: ignored\nmissing: not present locally; if a directory: ignored; if a file: synchronized\n" {
		t.Fatal("incorrect actual and conditional types:", output.String())
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink("directory", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	} else if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"link"}, &output); err != nil || code != 0 || output.String() != "link: synchronized\n" {
		t.Fatalf("symbolic link treated as its target: %d, %v, %s", code, err, output.String())
	}
	output.Reset()
	if _, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"loop/child"}, &output); err == nil || !strings.Contains(err.Error(), "unable to inspect local path") {
		t.Fatal("access failure not reported:", err)
	} else if strings.Contains(output.String(), "not present locally") {
		t.Fatal("access failure treated as absent:", output.String())
	}
}

func TestPrintIgnoresInaccessiblePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX directory access semantics")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	target := &remote.Target{Root: root, Contained: true}
	var output, errorOutput strings.Builder
	if _, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"file/child"}, &output); err == nil || !strings.Contains(err.Error(), "unable to inspect local path") {
		t.Fatal("non-directory parent treated as absent:", err)
	}
	if os.Geteuid() == 0 {
		return
	}
	blocked := filepath.Join(root, "blocked")
	if err := os.Mkdir(blocked, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0700)
	output.Reset()
	if _, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"blocked/child"}, &output); err == nil || !os.IsPermission(errors.Unwrap(err)) {
		t.Fatal("permission failure not preserved:", err)
	} else if output.Len() != 0 {
		t.Fatal("permission failure produced a classification:", output.String())
	}
}
