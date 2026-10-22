package cli

import (
	"context"
	"os"
	"path/filepath"
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
		"results/run1.json: ignored (inside results, which the session ignores)\n" +
		"train.py: synchronized\n" +
		"missing: synchronized\n" +
		"../other.log: ignored\n" +
		"..: synchronized (the session's root)\n"
	if output.String() != expected {
		t.Fatalf("incorrect checks:\n%s", output.String())
	}
	output.Reset()
	target.Ignores.Patterns = []string{"build/"}
	if code, err := printIgnores(pinnedRouter(target, &errorOutput), []string{"build", "train.py"}, &output); err != nil || code != 1 {
		t.Fatalf("checking failed: %d, %v", code, err)
	} else if output.String() != "build: synchronized as a file, ignored as a directory\ntrain.py: synchronized\n" {
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
