package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestLocator(t *testing.T) {
	root := t.TempDir()
	inside := &locator{root: root, base: "sub", contained: true}
	outside := &locator{root: root}

	// Paths are displayed relative to the working directory inside the root.
	for _, test := range []struct {
		locator           *locator
		relative, display string
	}{
		{inside, "sub/a", "a"},
		{inside, "other/b", filepath.Join("..", "other", "b")},
		{inside, "", ".."},
		{inside, "x\x1b", filepath.Join("..", "x^[")},
		{outside, "sub/a", filepath.Join(root, "sub", "a")},
	} {
		if display := test.locator.display(test.relative); display != test.display {
			t.Errorf("%q displayed as %q instead of %q", test.relative, display, test.display)
		}
	}

	// Arguments are resolved against the working directory inside the root,
	// and against the root otherwise.
	for _, test := range []struct {
		locator            *locator
		argument, relative string
	}{
		{inside, "a", "sub/a"},
		{inside, ".", "sub"},
		{inside, filepath.Join("..", "x"), "x"},
		{inside, "..", ""},
		{inside, filepath.Join(root, "sub", "a"), "sub/a"},
		{outside, "a", "a"},
	} {
		if relative, err := test.locator.resolve(test.argument); err != nil || relative != test.relative {
			t.Errorf("%q resolved to %q instead of %q: %v", test.argument, relative, test.relative, err)
		}
	}
	for _, argument := range []string{filepath.Join("..", ".."), filepath.Dir(root), filepath.Join(root+"-sibling", "a")} {
		if _, err := inside.resolve(argument); err == nil {
			t.Errorf("%q outside the root accepted", argument)
		}
	}
}

func TestReporter(t *testing.T) {
	// Reports are described from the submitting endpoint's perspective.
	var output strings.Builder
	reports := newReporter(&output, &remote.Target{Root: t.TempDir(), Alpha: true, Path: "sub", Contained: true})
	conflict := remote.Conflict{
		Root:  "sub/out",
		Alpha: []remote.Change{{Path: "sub/out", Old: "directory"}},
		Beta: []remote.Change{
			{Path: "sub/out/log", Old: "file", New: "file"},
			{Path: "sub/out/new", New: "symbolic link"},
			{Path: "sub/out/run", Old: "file", New: "executable file"},
		},
	}
	report := &remote.Report{
		Conflicts: []remote.Conflict{conflict},
		Problems:  []remote.Problem{{Alpha: true, Path: "sub/secret", Error: "permission denied"}, {Path: "data", Error: "unreadable"}},
	}
	reports.print(report)
	expected := "remote: warning: these conflicts aren't synchronized until one side's copy is deleted (see remote --diff):\n" +
		"remote:   out: deleted directory locally, modified file " + filepath.Join("out", "log") + " remotely, " +
		"created symbolic link " + filepath.Join("out", "new") + " remotely, changed file to executable file " + filepath.Join("out", "run") + " remotely\n" +
		"remote: warning: this content couldn't be synchronized:\n" +
		"remote:   secret (local): permission denied\n" +
		"remote:   " + filepath.Join("..", "data") + " (remote): unreadable\n"
	if output.String() != expected {
		t.Fatalf("incorrect report:\n%s\nexpected:\n%s", output.String(), expected)
	}

	// Only new lines are printed by subsequent reports.
	output.Reset()
	reports.print(report)
	if output.Len() != 0 {
		t.Fatal("report repeated:", output.String())
	}
	report.Conflicts = append(report.Conflicts, remote.Conflict{
		Root:  "sub/a",
		Alpha: []remote.Change{{Path: "sub/a", New: "file"}},
		Beta:  []remote.Change{{Path: "sub/a", New: "file"}},
	})
	report.ExcludedConflicts = 3
	reports.print(report)
	expected = "remote: warning: these conflicts aren't synchronized until one side's copy is deleted (see remote --diff):\n" +
		"remote:   a: created file locally, created file remotely\n" +
		"remote:   ...and 3 more conflicts\n"
	if output.String() != expected {
		t.Fatalf("incorrect subsequent report:\n%s\nexpected:\n%s", output.String(), expected)
	}

	// Beta's perspective is the reverse, and the paths of conflicting changes
	// are listed once each.
	output.Reset()
	newReporter(&output, &remote.Target{Root: t.TempDir(), Alpha: false}).print(&remote.Report{Conflicts: []remote.Conflict{conflict}})
	if !strings.Contains(output.String(), "deleted directory remotely") {
		t.Fatal("beta perspective not applied:", output.String())
	} else if paths := conflictPaths(report); strings.Join(paths, ",") != "sub/out,sub/out/log,sub/out/new,sub/out/run,sub/a" {
		t.Fatal("incorrect conflict paths:", paths)
	}
}
