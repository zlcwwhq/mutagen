package cli

import (
	"fmt"
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
	reports := newReporter(&output, &remote.Target{Root: t.TempDir(), Alpha: true, Path: "sub", Contained: true}, "remote")
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
		Conflicts:  []remote.Conflict{conflict},
		Problems:   []remote.Problem{{Alpha: true, Path: "sub/secret", Error: "permission denied"}, {Path: "data", Error: "unreadable"}},
		Resolution: true,
	}
	reports.print(report)
	expected := "remote: warning: these conflicts aren't synchronized until resolved (see remote --diff and remote --help conflicts):\n" +
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
	expected = "remote: warning: these conflicts aren't synchronized until resolved (see remote --diff and remote --help conflicts):\n" +
		"remote:   a: created file locally, created file remotely\n" +
		"remote:   ...and 3 more conflicts\n"
	if output.String() != expected {
		t.Fatalf("incorrect subsequent report:\n%s\nexpected:\n%s", output.String(), expected)
	}

	// Beta's perspective is the reverse, and the paths of conflicting changes
	// are listed once each. Sessions that don't support resolution don't
	// refer to it. Suggested commands keep the node that selected the
	// destination.
	output.Reset()
	newReporter(&output, &remote.Target{Root: t.TempDir(), Alpha: false}, "remote --node gpu").print(&remote.Report{Conflicts: []remote.Conflict{conflict}})
	if !strings.Contains(output.String(), "deleted directory remotely") {
		t.Fatal("beta perspective not applied:", output.String())
	} else if !strings.Contains(output.String(), "one side's copy is deleted (see remote --node gpu --diff):") {
		t.Fatal("resolution referenced without support:", output.String())
	} else if paths := conflictPaths(report); strings.Join(paths, ",") != "sub/out,sub/out/log,sub/out/new,sub/out/run,sub/a" {
		t.Fatal("incorrect conflict paths:", paths)
	}
}

func TestTruncatedConflict(t *testing.T) {
	conflict := remote.Conflict{
		Root:            "d",
		Beta:            []remote.Change{{Path: "d/file", Old: "file", New: "file"}},
		ExcludedChanges: 42,
	}
	report := &remote.Report{Conflicts: []remote.Conflict{conflict}}
	var output strings.Builder
	newReporter(&output, &remote.Target{Root: t.TempDir(), Contained: true}, "remote --node gpu").print(report)
	if !strings.Contains(output.String(), "42 more changes") || !strings.Contains(output.String(), "specify paths with remote --node gpu --diff") {
		t.Fatal("omissions weren't explained:", output.String())
	} else if paths := conflictPaths(report); strings.Join(paths, ",") != "d,d/file" {
		t.Fatal("truncated conflict root wasn't compared:", paths)
	}
}

func TestConflictRootSummary(t *testing.T) {
	// Listings retain actual change history, including omitted changes, and
	// use the warning formatter from either endpoint's perspective. Control
	// characters in roots and descendant paths are neutralized only for text.
	conflict := &remote.ConflictRoot{
		Path:            "sub/d\x1b",
		AlphaChanges:    []remote.Change{{Path: "sub/d\x1b", Old: "directory", New: "file"}},
		BetaChanges:     []remote.Change{{Path: "sub/d\x1b/child\x1b", New: "symbolic link"}},
		ExcludedChanges: 42,
	}
	for _, alpha := range []bool{true, false} {
		reports := newReporter(nil, &remote.Target{Root: t.TempDir(), Alpha: alpha, Contained: true, Path: "sub"}, "remote")
		summary := reports.describeConflict(conflict.Changes())
		side := "locally"
		if !alpha {
			side = "remotely"
		}
		if !strings.HasPrefix(summary, "d^[: ") || !strings.Contains(summary, "changed directory to file "+side) ||
			!strings.Contains(summary, "created symbolic link "+filepath.Join("d^[", "child^[")) ||
			!strings.Contains(summary, "42 more changes") || strings.Contains(summary, "\x1b") {
			t.Fatalf("incorrect summary: %q", summary)
		}
	}
}

func TestListedConflictChangesLimited(t *testing.T) {
	// Text listings describe a bounded number of each endpoint's changes,
	// keeping the first ones, such as the root's, and counting the rest with
	// those that the daemon already omitted, without modifying the listing.
	root := &remote.ConflictRoot{Path: "out", AlphaChanges: []remote.Change{{Path: "out", Old: "directory"}}, ExcludedChanges: 3}
	for i := range 11 {
		root.AlphaChanges = append(root.AlphaChanges, remote.Change{Path: fmt.Sprintf("out/%d", i), Old: "file"})
	}
	root.BetaChanges = []remote.Change{{Path: "out/0", Old: "file", New: "file"}, {Path: "out/1", Old: "file", New: "file"}}
	limited := limitChanges(root.Changes(), listedChanges)
	if len(limited.Alpha) != listedChanges || limited.Alpha[0].Path != "out" || len(limited.Beta) != 2 ||
		limited.ExcludedChanges != 3+12-listedChanges {
		t.Fatalf("incorrect limit: %+v", limited)
	} else if len(root.AlphaChanges) != 12 || root.ExcludedChanges != 3 {
		t.Fatal("listing modified:", root)
	}
	reports := newReporter(nil, &remote.Target{Root: t.TempDir(), Alpha: true, Contained: true}, "remote")
	if summary := reports.describeConflict(limited); !strings.HasPrefix(summary, "out: deleted directory locally") ||
		!strings.Contains(summary, "...and 10 more changes") || strings.Contains(summary, "out/5") {
		t.Fatalf("incorrect summary: %q", summary)
	}
}
