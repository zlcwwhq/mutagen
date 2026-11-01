package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

// verifyExclusionWarning verifies that a list of problems consists of one
// warning about an excluded path.
func verifyExclusionWarning(t *testing.T, problems []*core.Problem, path string) {
	t.Helper()
	verifySingleProblem(t, problems, path, "ignored by this endpoint's own rules", `"!`+path+`"`)
	if !problems[0].Warning {
		t.Error("exclusion not reported as a warning")
	}
}

// verifyAbsent verifies that a path doesn't exist in a root.
func verifyAbsent(t *testing.T, root, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
		t.Errorf("%s exists or can't be checked: %v", path, err)
	}
}

// verifyContents verifies the contents of a file in a root.
func verifyContents(t *testing.T, root, path, expected string) {
	t.Helper()
	if contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path))); err != nil {
		t.Errorf("unable to read %s: %v", path, err)
	} else if string(contents) != expected {
		t.Errorf("incorrect contents for %s: %q != %q", path, contents, expected)
	}
}

// TestEndpointGitModeUpdateIgnoredCollision ensures that Git cannot overwrite
// ignored local files when an incoming commit starts tracking them.
func TestEndpointGitModeUpdateIgnoredCollision(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	first := upstream.head
	second := upstream.commit("local.db", "upstream")
	upstream.push()
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	ctx := context.Background()
	if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: upstream.entry(first)}); len(problems) != 0 {
		t.Fatal(problems)
	}
	directory := filepath.Join(root, "sub")
	writeTree(t, directory, map[string]string{".git/info/exclude": "local.db\n", "local.db": "local database"})
	old := upstream.entry(first)
	results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: old, New: upstream.entry(second)})
	if results[0] != old {
		t.Error("failed update changed the result")
	}
	verifySingleProblem(t, problems, "sub", "unable to check out", "local.db")
	if contents, err := os.ReadFile(filepath.Join(directory, "local.db")); err != nil || string(contents) != "local database" {
		t.Fatalf("local database lost: %q, %v", contents, err)
	}
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != first {
		t.Fatal("HEAD changed after refused update:", head)
	}
}

// TestEndpointGitModeIncomingIgnored checks receiving rules for paths absent
// from scans, including descendants of a directory created in one transition.
func TestEndpointGitModeIncomingIgnored(t *testing.T) {
	isolateGit(t)
	for _, test := range []struct{ name, path, rule, excluded string }{
		{"file", "private", "private\n", "private"},
		{"directory", "private/secret.env", "private/\n", "private"},
		{"descendant", "new/private", "new/private\n", "new/private"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourceRoot := newGitRoot(t, map[string]string{test.path: "secret", "public": "public"})
			targetRoot := newGitRoot(t, nil)
			initializeRepository(t, sourceRoot)
			initializeRepository(t, targetRoot)
			writeTree(t, targetRoot, map[string]string{".git/info/exclude": test.rule})
			source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
			target := newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch)

			// Staging must not request even the bytes of the ignored incoming
			// file.
			snapshot := scanEndpoint(t, source, true)
			scanEndpoint(t, target, true)
			digest := entryAt(snapshot, test.path).Digest
			paths, _, receiver, err := target.Stage([]string{test.path}, [][]byte{digest})
			if err != nil || len(paths) != 0 || receiver != nil {
				t.Fatalf("ignored file requested: %v, %v", paths, err)
			}

			// The rest of the content isn't affected.
			_, _, problems, err := propagateChanges(t, source, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			verifyExclusionWarning(t, problems, test.excluded)
			verifyAbsent(t, targetRoot, test.path)
			verifyContents(t, targetRoot, "public", "public")
		})
	}
}

// TestEndpointGitModeUnreadableRules verifies that read failures hide the
// affected subtree from reconciliation, while unrelated directories still scan.
func TestEndpointGitModeUnreadableRules(t *testing.T) {
	isolateGit(t)
	for _, rules := range []string{".gitignore", "nested/.gitignore", ".git/info/exclude"} {
		t.Run(rules, func(t *testing.T) {
			root := newGitRoot(t, map[string]string{"secret.env": "root secret", "nested/secret.env": "nested secret", "public": "public"})
			initializeRepository(t, root)
			writeTree(t, root, map[string]string{rules: "secret.env\n"})
			rulePath := filepath.Join(root, filepath.FromSlash(rules))
			if err := os.Chmod(rulePath, 0); err != nil {
				t.Skip(err)
			}
			t.Cleanup(func() { os.Chmod(rulePath, 0600) })
			if file, err := os.Open(rulePath); err == nil {
				file.Close()
				t.Skip("process can read permission-denied files")
			}
			e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
			snapshot := scanEndpoint(t, e, true)
			blocked := ""
			if rules == "nested/.gitignore" {
				blocked = "nested"
			}
			entry := snapshot.Content
			if blocked != "" {
				entry = entryAt(snapshot, blocked)
			}
			if entry == nil || entry.Kind != core.EntryKind_Problematic {
				t.Fatalf("subtree was not blocked: %v", entry)
			}
			if blocked != "" && entryAt(snapshot, "public").Kind != core.EntryKind_File {
				t.Error("unaffected file not scanned")
			}
			// Reconciliation cannot send secrets from the blocked subtree.
			empty := &core.Entry{Kind: core.EntryKind_Directory}
			_, _, changes, _ := core.ReconcileGitMode(nil, snapshot.Content, empty, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
			for _, change := range changes {
				if change.Path == blocked || change.New.GetContents()["secret.env"] != nil {
					t.Fatal("blocked subtree entered changes:", change)
				}
			}
			if err := os.Chmod(rulePath, 0600); err != nil {
				t.Fatal(err)
			}
			recovered := scanEndpoint(t, e, true).Content
			if blocked != "" {
				recovered = recovered.Contents[blocked]
			}
			if recovered.Kind != core.EntryKind_Directory {
				t.Error("scan did not recover after permissions were restored")
			}
		})
	}
}

// TestEndpointGitModeIncomingReplacement checks the replacement of an existing
// file with a directory, with descendants that are and aren't excluded, and
// with session overrides.
func TestEndpointGitModeIncomingReplacement(t *testing.T) {
	isolateGit(t)
	setup := func(t *testing.T, rule string, patterns ...string) (*endpoint, *endpoint, string) {
		sourceRoot := newGitRoot(t, map[string]string{"new/private": "incoming"})
		targetRoot := newGitRoot(t, map[string]string{"new": "original"})
		initializeRepository(t, sourceRoot)
		initializeRepository(t, targetRoot)
		writeTree(t, targetRoot, map[string]string{".git/info/exclude": rule})
		source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
		return source, newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch, patterns...), targetRoot
	}

	// A directory whose only child is excluded replaces the file anyway, since
	// the type change on the other endpoint doesn't depend on the child.
	t.Run("child excluded", func(t *testing.T) {
		source, target, targetRoot := setup(t, "new/private\n")
		_, results, problems, err := propagateChanges(t, source, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		verifyExclusionWarning(t, problems, "new/private")
		if !results[0].Equal(&core.Entry{Kind: core.EntryKind_Directory}, true) {
			t.Errorf("incorrect result: %v", results[0])
		}
		if entries, err := os.ReadDir(filepath.Join(targetRoot, "new")); err != nil || len(entries) != 0 {
			t.Errorf("directory not created empty: %v, %v", entries, err)
		}
	})

	// If the change's own path is excluded, then the old content stays.
	t.Run("own path excluded", func(t *testing.T) {
		source, target, targetRoot := setup(t, "new/\n")
		problems, err := propagate(t, source, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		verifyExclusionWarning(t, problems, "new")
		verifyContents(t, targetRoot, "new", "original")
	})

	// Session patterns take precedence over the rules.
	t.Run("overridden", func(t *testing.T) {
		source, target, targetRoot := setup(t, "new/private\n", "!new/private")
		problems, err := propagate(t, source, target, nil)
		if err != nil {
			t.Fatal(err)
		} else if len(problems) != 0 {
			t.Fatal(problems)
		}
		verifyContents(t, targetRoot, "new/private", "incoming")
	})
}

// TestEndpointGitModeIncomingRuleFiles checks that rule files reach a directory
// that's excluded, as far as staging and transitioning are concerned, without
// anything else, and that rule files in it don't cost a warning by themselves.
func TestEndpointGitModeIncomingRuleFiles(t *testing.T) {
	isolateGit(t)
	sourceRoot := newGitRoot(t, map[string]string{
		"outputs/.gitignore":        "*.tmp\n",
		"outputs/data":              "data",
		"outputs/nested/.gitignore": "*.tmp\n",
		"outputs/nested/data":       "data",
		"outputs/other/data":        "data",
		"rules/.gitignore":          "*.tmp\n",
		"public":                    "public",
	})
	targetRoot := newGitRoot(t, nil)
	writeTree(t, targetRoot, map[string]string{".git/info/exclude": "outputs/\nrules/\n"})
	source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
	target := newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch)

	// Only what will be written is requested.
	snapshot := scanEndpoint(t, source, true)
	scanEndpoint(t, target, true)
	paths := []string{"outputs/.gitignore", "outputs/data", "outputs/nested/.gitignore", "outputs/nested/data", "outputs/other/data", "rules/.gitignore", "public"}
	var digests [][]byte
	for _, path := range paths {
		digests = append(digests, entryAt(snapshot, path).Digest)
	}
	filtered, signatures, receiver, err := target.Stage(paths, digests)
	if err != nil {
		t.Fatal(err)
	} else if !slices.Equal(filtered, []string{"outputs/.gitignore", "outputs/nested/.gitignore", "rules/.gitignore", "public"}) {
		t.Fatal("incorrect paths requested:", filtered)
	} else if err := source.Supply(filtered, signatures, receiver); err != nil {
		t.Fatal(err)
	}

	// There's one warning for the directory with other content, and none for
	// the one that only contains a rule file.
	results, problems, missing, err := target.Transition(context.Background(), []*core.Change{
		{Path: "outputs", New: entryAt(snapshot, "outputs")},
		{Path: "public", New: entryAt(snapshot, "public")},
		{Path: "rules", New: entryAt(snapshot, "rules")},
	})
	if err != nil {
		t.Fatal(err)
	} else if missing {
		t.Error("staged files missing")
	}
	verifyExclusionWarning(t, problems, "outputs")
	expected := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{
		".gitignore": entryAt(snapshot, "outputs/.gitignore"),
		"nested": {Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{
			".gitignore": entryAt(snapshot, "outputs/nested/.gitignore"),
		}},
	}}
	if !results[0].Equal(expected, true) {
		t.Error("incorrect result for directory with other content:", results[0])
	} else if results[1] != entryAt(snapshot, "public") {
		t.Error("incorrect result for unaffected file:", results[1])
	} else if !results[2].Equal(entryAt(snapshot, "rules"), true) {
		t.Error("incorrect result for directory with only a rule file:", results[2])
	}
	verifyContents(t, targetRoot, "outputs/.gitignore", "*.tmp\n")
	verifyContents(t, targetRoot, "outputs/nested/.gitignore", "*.tmp\n")
	verifyContents(t, targetRoot, "rules/.gitignore", "*.tmp\n")
	verifyContents(t, targetRoot, "public", "public")
	for _, path := range []string{"outputs/data", "outputs/nested/data", "outputs/other"} {
		verifyAbsent(t, targetRoot, path)
	}

	// The directories are thus no longer tracked on this endpoint.
	verifyKinds(t, scanEndpoint(t, target, true), map[string]core.EntryKind{
		"outputs": core.EntryKind_Untracked,
		"rules":   core.EntryKind_Untracked,
		"public":  core.EntryKind_File,
	})
}

// TestEndpointGitModeRefusalResults checks that a refusal doesn't affect the
// results of the changes around it, which must correspond to the changes.
func TestEndpointGitModeRefusalResults(t *testing.T) {
	isolateGit(t)
	sourceRoot := newGitRoot(t, map[string]string{"a": "a", "m": "m", "n/file": "file", "z": "z"})
	targetRoot := newGitRoot(t, map[string]string{"gone": "gone", "m": "m"})
	initializeRepository(t, sourceRoot)
	initializeRepository(t, targetRoot)
	writeTree(t, targetRoot, map[string]string{".git/info/exclude": "n/\n"})
	source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
	target := newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch)
	changes, results, problems, err := propagateChanges(t, source, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	verifyExclusionWarning(t, problems, "n")
	var paths []string
	for c, change := range changes {
		paths = append(paths, change.Path)
		expected := change.New
		if change.Path == "n" {
			expected = change.Old
		}
		if !results[c].Equal(expected, true) {
			t.Errorf("incorrect result for %s: %v", change.Path, results[c])
		}
	}
	if !slices.Equal(paths, []string{"a", "gone", "n", "z"}) {
		t.Error("incorrect changes:", paths)
	}
	verifyContents(t, targetRoot, "a", "a")
	verifyContents(t, targetRoot, "z", "z")
	verifyAbsent(t, targetRoot, "gone")
	verifyAbsent(t, targetRoot, "n")
}

// TestEndpointGitModeUnevaluablePaths checks that a path that can't be
// evaluated, because the rules that apply to it can't be read, is just not
// staged, without affecting anything else in the cycle, and that it's reported
// as a failure by the transition. A directory whose name is too long stands in
// for all of the reasons for this, and it doesn't depend on the privileges of
// the process. It only results in a failure beneath a directory that exists,
// since one that's missing is an obstacle to reading its rules first, which
// isn't a failure.
func TestEndpointGitModeUnevaluablePaths(t *testing.T) {
	isolateGit(t)
	sourceRoot := newGitRoot(t, map[string]string{"public": "public", "pkg/file": "file"})
	targetRoot := newGitRoot(t, nil)
	long := strings.Repeat("d", 300)
	if _, err := os.Lstat(filepath.Join(targetRoot, long)); err == nil || os.IsNotExist(err) {
		t.Skip("this system doesn't reject long names:", err)
	}
	if err := os.Mkdir(filepath.Join(targetRoot, "pkg"), 0700); err != nil {
		t.Fatal(err)
	}
	source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
	target := newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, source, true)
	targetSnapshot := scanEndpoint(t, target, true)

	// Staging leaves out the paths that can't be evaluated, of which there's
	// one at the top level and one in a directory with other content.
	public, file := entryAt(snapshot, "public"), entryAt(snapshot, "pkg/file")
	paths, signatures, receiver, err := target.Stage(
		[]string{long + "/file", "pkg/" + long + "/file", "pkg/file", "public"},
		[][]byte{file.Digest, file.Digest, file.Digest, public.Digest},
	)
	if err != nil {
		t.Fatal("staging failed:", err)
	} else if !slices.Equal(paths, []string{"pkg/file", "public"}) {
		t.Fatal("incorrect paths requested:", paths)
	} else if err := source.Supply(paths, signatures, receiver); err != nil {
		t.Fatal(err)
	}

	// The transition reports them, as failures, and applies everything else.
	unevaluable := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"file": file}}
	results, problems, missing, err := target.Transition(context.Background(), []*core.Change{
		{Path: long, New: unevaluable},
		{
			Path: "pkg",
			Old:  entryAt(targetSnapshot, "pkg"),
			New:  &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{long: unevaluable, "file": file}},
		},
		{Path: "public", New: public},
	})
	if err != nil {
		t.Fatal(err)
	} else if missing {
		t.Error("staged files missing")
	}
	if len(problems) != 2 {
		t.Fatal("incorrect number of problems:", problems)
	}
	for p, path := range []string{long, "pkg/" + long} {
		if problems[p].Path != path {
			t.Errorf("incorrect problem path: %q != %q", problems[p].Path, path)
		} else if problems[p].Warning {
			t.Error("failure reported as a warning:", problems[p])
		} else if !strings.Contains(problems[p].Error, "unable to read ignore rules") {
			t.Error("incorrect problem:", problems[p])
		}
	}
	if results[0] != nil {
		t.Error("incorrect result for path that can't be evaluated:", results[0])
	} else if !results[1].Equal(&core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"file": file}}, true) {
		t.Error("incorrect result for directory with other content:", results[1])
	} else if results[2] != public {
		t.Error("incorrect result for unaffected file:", results[2])
	}
	verifyContents(t, targetRoot, "pkg/file", "file")
	verifyContents(t, targetRoot, "public", "public")
}

// TestPruneGitTransitions tests pruneGitTransitions.
func TestPruneGitTransitions(t *testing.T) {
	// Create the entries for tests.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	other := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{2}}
	link := &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "target"}
	submodule := &core.Entry{Kind: core.EntryKind_GitLink, Digest: make([]byte, 20), Url: "https://example.org/repository.git"}
	directory := func(contents map[string]*core.Entry) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	long := strings.Repeat("d", 300)

	// Define test cases. If the expected new content is nil, then that means
	// that the change should be refused, and a change is expected not to be
	// recreated if its new content is the expected content by identity.
	type warnings []string
	type failures []string
	tests := []struct {
		name     string
		rules    string
		existing string
		changes  []*core.Change
		expected []*core.Entry
		warnings warnings
		failures failures
	}{
		{
			name:     "nothing excluded leaves changes as they are",
			rules:    "*.log\n",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{"a": file, "sub": directory(map[string]*core.Entry{"b": file})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"a": file, "sub": directory(map[string]*core.Entry{"b": file})})},
		},
		{
			name:     "excluded sibling",
			rules:    "*.log\n",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{"a.go": file, "b.go": file, "debug.log": file})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"a.go": file, "b.go": file})},
			warnings: warnings{"pkg/debug.log"},
		},
		{
			name:     "root",
			rules:    "*.log\n",
			changes:  []*core.Change{{Path: "", New: directory(map[string]*core.Entry{"a.go": file, "debug.log": file})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"a.go": file})},
			warnings: warnings{"debug.log"},
		},
		{
			name:     "excluded directory is reported once at its own path",
			rules:    "build/\n",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{"a": file, "build": directory(map[string]*core.Entry{"x": file, "y": file, "z": directory(map[string]*core.Entry{"w": file})})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"a": file})},
			warnings: warnings{"pkg/build"},
		},
		{
			name:     "empty excluded directory",
			rules:    "build/\n",
			changes:  []*core.Change{{Path: "build", New: directory(nil)}},
			expected: []*core.Entry{nil},
			warnings: warnings{"build"},
		},
		{
			name:     "directory left without contents is still created",
			rules:    "*.log\n",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{"sub": directory(map[string]*core.Entry{"debug.log": file})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"sub": directory(nil)})},
			warnings: warnings{"pkg/sub/debug.log"},
		},
		{
			name:     "excluded directory with nothing but rule files",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs", New: directory(map[string]*core.Entry{".gitignore": file, "sub": directory(map[string]*core.Entry{".gitignore": other})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{".gitignore": file, "sub": directory(map[string]*core.Entry{".gitignore": other})})},
		},
		{
			name:     "excluded directory with rule files and other content",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs", New: directory(map[string]*core.Entry{".gitignore": file, "data": file, "empty": directory(nil), "other": directory(map[string]*core.Entry{"data": file}), "sub": directory(map[string]*core.Entry{".gitignore": other, "data": file})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{".gitignore": file, "sub": directory(map[string]*core.Entry{".gitignore": other})})},
			warnings: warnings{"outputs"},
		},
		{
			name:     "rule file that's excluded itself",
			rules:    ".gitignore\n",
			changes:  []*core.Change{{Path: ".gitignore", New: file}, {Path: "sub/.gitignore", New: file}},
			expected: []*core.Entry{file, file},
		},
		{
			name:     "only regular files provide rules",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs", New: directory(map[string]*core.Entry{".gitignore": link})}, {Path: "outputs/sub", New: directory(map[string]*core.Entry{".gitignore": directory(nil)})}},
			expected: []*core.Entry{nil, nil},
			warnings: warnings{"outputs"},
		},
		{
			name:     "excluded symbolic link",
			rules:    "link\n",
			changes:  []*core.Change{{Path: "link", New: link}},
			expected: []*core.Entry{nil},
			warnings: warnings{"link"},
		},
		{
			name:     "excluded submodule is treated as a directory",
			rules:    "sub/\n",
			changes:  []*core.Change{{Path: "sub", New: submodule}, {Path: "pkg", New: directory(map[string]*core.Entry{"a": file, "sub": submodule})}},
			expected: []*core.Entry{nil, directory(map[string]*core.Entry{"a": file})},
			warnings: warnings{"sub", "pkg/sub"},
		},
		{
			name:     "changes beneath the same excluded path are reported once",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs/a", New: file}, {Path: "public", New: file}, {Path: "outputs/b", New: file}},
			expected: []*core.Entry{nil, file, nil},
			warnings: warnings{"outputs"},
		},
		{
			name:     "deletions aren't affected",
			rules:    "*.log\noutputs/\n",
			changes:  []*core.Change{{Path: "debug.log", Old: file}, {Path: "outputs", Old: directory(map[string]*core.Entry{"a": file})}},
			expected: []*core.Entry{nil, nil},
		},
		{
			name:     "old content is retained",
			rules:    "new/private\n",
			changes:  []*core.Change{{Path: "new", Old: file, New: directory(map[string]*core.Entry{"private": file, "public": file})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"public": file})},
			warnings: warnings{"new/private"},
		},
		{
			name:     "failure at the path of a change",
			changes:  []*core.Change{{Path: long, New: directory(map[string]*core.Entry{"a": file})}, {Path: long + "/a", New: file}, {Path: "public", New: file}},
			expected: []*core.Entry{nil, nil, file},
			failures: failures{long, long + "/a"},
		},
		{
			name:     "failure beneath the path of a change",
			rules:    "*.log\n",
			existing: "pkg",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{"a": file, "debug.log": file, long: directory(map[string]*core.Entry{"b": file})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"a": file})},
			warnings: warnings{"pkg/debug.log"},
			failures: failures{"pkg/" + long},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.failures != nil {
				if _, err := os.Lstat(filepath.Join(root, long)); err == nil || os.IsNotExist(err) {
					t.Skip("this system doesn't reject long names:", err)
				}
			}
			writeTree(t, root, map[string]string{".git/info/exclude": test.rules})
			if test.existing != "" {
				if err := os.Mkdir(filepath.Join(root, test.existing), 0700); err != nil {
					t.Fatal(err)
				}
			}

			// Prune, retaining copies of the changes' content, which must not
			// be modified.
			var originals []*core.Entry
			for _, change := range test.changes {
				originals = append(originals, change.New.Copy(core.EntryCopyBehaviorDeep))
			}
			results, problems := pruneGitTransitions(gitignore.NewIgnorer(root, nil), test.changes)
			for c, change := range test.changes {
				if !change.New.Equal(originals[c], true) {
					t.Errorf("new content of change %d modified", c)
				}
			}

			// Verify the changes that remain.
			if len(results) != len(test.changes) {
				t.Fatal("incorrect number of results:", results)
			}
			for c, change := range test.changes {
				if change.New == nil {
					if results[c] != change {
						t.Errorf("deletion %d not left as is: %v", c, results[c])
					}
				} else if test.expected[c] == nil {
					if results[c] != nil {
						t.Errorf("change %d not refused: %v", c, results[c])
					}
				} else if results[c] == nil {
					t.Errorf("change %d refused", c)
				} else if results[c].Path != change.Path || results[c].Old != change.Old {
					t.Errorf("change %d has incorrect path or old content: %v", c, results[c])
				} else if !results[c].New.Equal(test.expected[c], true) {
					t.Errorf("change %d has incorrect new content: %v", c, results[c].New)
				} else if err := results[c].New.EnsureValid(true); err != nil {
					t.Errorf("change %d has invalid new content: %v", c, err)
				} else if change.New.Equal(results[c].New, true) && results[c] != change {
					t.Errorf("change %d was recreated without being modified", c)
				}
			}

			// Verify the problems.
			var actualWarnings warnings
			var actualFailures failures
			for _, problem := range problems {
				if problem.Warning {
					actualWarnings = append(actualWarnings, problem.Path)
				} else {
					actualFailures = append(actualFailures, problem.Path)
				}
			}
			if !slices.Equal(actualWarnings, test.warnings) {
				t.Errorf("incorrect warnings: %v != %v", actualWarnings, test.warnings)
			}
			if !slices.Equal(actualFailures, test.failures) {
				t.Errorf("incorrect failures: %v != %v", actualFailures, test.failures)
			}
		})
	}
}

// addSubmodule adds a repository to the upstream as a submodule at a path,
// using the specified name if it isn't empty, and pushes the result.
func (u *gitUpstream) addSubmodule(nested *gitUpstream, path, name string) {
	u.t.Helper()
	arguments := []string{"submodule", "add", "--quiet"}
	if name != "" {
		arguments = append(arguments, "--name", name)
	}
	mustGit(u.t, u.work, append(arguments, "--", nested.url, path)...)
	u.commitAll("add " + path)
}

// commitAll commits everything that's been staged and pushes the result.
func (u *gitUpstream) commitAll(message string) {
	u.t.Helper()
	mustGit(u.t, u.work, "commit", "--quiet", "--no-gpg-sign", "-m", message)
	u.head = mustGit(u.t, u.work, "rev-parse", "HEAD")
	u.push()
}

// advanceSubmodule moves a submodule of the upstream to the latest commit of its
// own upstream's main branch and pushes the result.
func (u *gitUpstream) advanceSubmodule(path string) {
	u.t.Helper()
	mustGit(u.t, filepath.Join(u.work, path), "pull", "--quiet", "origin", "main")
	mustGit(u.t, u.work, "add", "--", path)
	u.commitAll("update " + path)
}

// ignoreLocally adds a file to the exclude file of the repository in a directory
// and creates it there.
func ignoreLocally(t *testing.T, directory, name, contents string) {
	t.Helper()
	exclude := mustGit(t, directory, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(directory, exclude)
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(exclude, []byte(name+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeTree(t, directory, map[string]string{name: contents})
}

// TestEndpointGitModeNestedIgnoredCollision ensures that updating the submodules
// nested in a submodule can't overwrite ignored local files when an incoming
// commit starts tracking them, however deep they are and whatever the
// configuration of the repositories asks for. The submodule's own commit is in
// place by then, so that isn't refused, but the nested submodule stays where it
// is.
func TestEndpointGitModeNestedIgnoredCollision(t *testing.T) {
	for _, test := range []struct {
		// name is the name of the test.
		name string
		// deep indicates that the colliding repository is nested in a nested
		// submodule rather than directly in the submodule.
		deep bool
		// prepareUpstream changes the upstream before the first commit is used.
		prepareUpstream func(*testing.T, *gitUpstream)
		// prepareClone changes the clone before the update.
		prepareClone func(*testing.T, string)
	}{
		{name: "nested"},
		{name: "deeply nested", deep: true},
		{
			// With this setting, a plain checkout updates submodules itself,
			// using a command that overwrites ignored files.
			name: "recursive checkout configured",
			deep: true,
			prepareClone: func(t *testing.T, directory string) {
				mustGit(t, directory, "config", "submodule.recurse", "true")
				mustGit(t, filepath.Join(directory, "nested"), "config", "submodule.recurse", "true")
			},
		},
		{
			// A repository can ask for this by itself, and a merge overwrites
			// ignored files too.
			name: "merge requested by repository",
			prepareUpstream: func(t *testing.T, u *gitUpstream) {
				mustGit(t, u.work, "config", "--file", ".gitmodules", "submodule.nested.update", "merge")
				mustGit(t, u.work, "add", "--", ".gitmodules")
				u.commitAll("merge nested")
			},
		},
		{
			name: "merge configured",
			prepareClone: func(t *testing.T, directory string) {
				mustGit(t, directory, "config", "submodule.nested.update", "merge")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Create the upstreams.
			leaf := newGitUpstream(t, "leaf")
			upstream := newGitUpstream(t, "upstream")
			var nested *gitUpstream
			relative := "nested"
			if test.deep {
				nested = newGitUpstream(t, "nested")
				nested.addSubmodule(leaf, "deep", "")
				upstream.addSubmodule(nested, "nested", "")
				relative = "nested/deep"
			} else {
				upstream.addSubmodule(leaf, "nested", "")
			}
			if test.prepareUpstream != nil {
				test.prepareUpstream(t, upstream)
			}

			// Populate the submodule.
			root := newGitRoot(t, nil)
			e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
			ctx := context.Background()
			first := upstream.entry(upstream.head)
			if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: first}); len(problems) != 0 {
				t.Fatal(problems)
			}
			directory := filepath.Join(root, "sub")
			colliding := filepath.Join(directory, filepath.FromSlash(relative))
			head := mustGit(t, colliding, "rev-parse", "HEAD")
			ignoreLocally(t, colliding, "local.db", "local database")
			if test.prepareClone != nil {
				test.prepareClone(t, directory)
			}

			// Have the repository at the bottom start tracking the file.
			leaf.commit("local.db", "upstream")
			leaf.push()
			if test.deep {
				nested.advanceSubmodule("deep")
			}
			upstream.advanceSubmodule("nested")
			second := upstream.entry(upstream.head)

			// The update is refused where it would overwrite the file.
			results, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: second})
			if results[0] != second {
				t.Error("incorrect result:", results[0])
			}
			verifySingleProblem(t, problems, "sub", "unable to update nested submodules", "local.db", relative+":")
			verifyContents(t, directory, relative+"/local.db", "local database")
			if current := mustGit(t, colliding, "rev-parse", "HEAD"); current != head {
				t.Error("nested submodule moved despite refusal:", current)
			}
			if current := mustGit(t, directory, "rev-parse", "HEAD"); current != upstream.head {
				t.Error("submodule's own commit isn't in place:", current)
			}
		})
	}
}

// TestEndpointGitModeNestedRefusalIsolated tests that a nested submodule that's
// refused doesn't hold up its siblings, and that it's the only one that's
// protected in the way that its name allows: a path that could be read as a
// pattern must not match anything else.
func TestEndpointGitModeNestedRefusalIsolated(t *testing.T) {
	// Use a name that patterns can match, where that's possible.
	blockedPath := "blocked"
	if runtime.GOOS != "windows" {
		blockedPath = "blocked*"
	}

	// Create the upstreams.
	blocked := newGitUpstream(t, "blocked")
	alsoBlocked := newGitUpstream(t, "also-blocked")
	other := newGitUpstream(t, "other")
	upstream := newGitUpstream(t, "upstream")
	upstream.addSubmodule(blocked, blockedPath, "")
	upstream.addSubmodule(alsoBlocked, "blocked-too", "")
	upstream.addSubmodule(other, "other", "")

	// Populate the submodule and create files that will collide.
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	ctx := context.Background()
	first := upstream.entry(upstream.head)
	if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: first}); len(problems) != 0 {
		t.Fatal(problems)
	}
	directory := filepath.Join(root, "sub")
	ignoreLocally(t, filepath.Join(directory, blockedPath), "local.db", "first database")
	ignoreLocally(t, filepath.Join(directory, "blocked-too"), "local.db", "second database")

	// Move all of them forward.
	blocked.commit("local.db", "upstream")
	blocked.push()
	alsoBlocked.commit("local.db", "upstream")
	alsoBlocked.push()
	other.commit("second", "second")
	other.push()
	upstream.advanceSubmodule(blockedPath)
	upstream.advanceSubmodule("blocked-too")
	upstream.advanceSubmodule("other")

	// Both of the refusals are reported, and the sibling is updated.
	_, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: upstream.entry(upstream.head)})
	verifySingleProblem(t, problems, "sub", "unable to update nested submodules", blockedPath+":", "blocked-too:")
	verifyContents(t, directory, blockedPath+"/local.db", "first database")
	verifyContents(t, directory, "blocked-too/local.db", "second database")
	if head := mustGit(t, filepath.Join(directory, "other"), "rev-parse", "HEAD"); head != other.head {
		t.Error("sibling wasn't updated:", head)
	}
}

// TestEndpointGitModeNestedUpdateNone tests that a nested submodule whose update
// is disabled stays as it is, along with what's nested in it.
func TestEndpointGitModeNestedUpdateNone(t *testing.T) {
	// Create upstreams with a submodule that has one nested in it.
	leaf := newGitUpstream(t, "leaf")
	nested := newGitUpstream(t, "nested")
	upstream := newGitUpstream(t, "upstream")
	nested.addSubmodule(leaf, "deep", "")
	upstream.addSubmodule(nested, "nested", "")

	// Disabling updates in the repository means that it's never populated.
	t.Run("repository", func(t *testing.T) {
		disabled := newGitUpstream(t, "disabled")
		upstream.addSubmodule(disabled, "disabled", "")
		mustGit(t, upstream.work, "config", "--file", ".gitmodules", "submodule.disabled.update", "none")
		mustGit(t, upstream.work, "add", "--", ".gitmodules")
		upstream.commitAll("disable updates")
		root := newGitRoot(t, nil)
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		ctx := context.Background()
		if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: upstream.entry(upstream.head)}); len(problems) != 0 {
			t.Fatal(problems)
		}
		verifyAbsent(t, root, "sub/disabled/file")
		verifyContents(t, root, "sub/nested/deep/file", "first")
	})

	// Disabling updates locally leaves it where it is.
	t.Run("configuration", func(t *testing.T) {
		root := newGitRoot(t, nil)
		e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
		ctx := context.Background()
		first := upstream.entry(upstream.head)
		if _, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", New: first}); len(problems) != 0 {
			t.Fatal(problems)
		}
		directory := filepath.Join(root, "sub")
		mustGit(t, directory, "config", "submodule.nested.update", "none")
		nestedHead := mustGit(t, filepath.Join(directory, "nested"), "rev-parse", "HEAD")
		leafHead := mustGit(t, filepath.Join(directory, "nested", "deep"), "rev-parse", "HEAD")
		leaf.commit("second", "second")
		leaf.push()
		nested.advanceSubmodule("deep")
		upstream.advanceSubmodule("nested")
		_, problems := transitionEndpoint(t, ctx, e, &core.Change{Path: "sub", Old: first, New: upstream.entry(upstream.head)})
		if len(problems) != 0 {
			t.Fatal(problems)
		}
		if head := mustGit(t, filepath.Join(directory, "nested"), "rev-parse", "HEAD"); head != nestedHead {
			t.Error("disabled submodule moved:", head)
		}
		if head := mustGit(t, filepath.Join(directory, "nested", "deep"), "rev-parse", "HEAD"); head != leafHead {
			t.Error("submodule nested in a disabled submodule moved:", head)
		}
	})
}

// TestEndpointGitModeNestedUnprotectableName tests that a nested submodule whose
// name can't be used to protect its checkout isn't updated, because Git would
// silently take the configuration for something else and use a plain checkout,
// and that it doesn't hold up its siblings.
func TestEndpointGitModeNestedUnprotectableName(t *testing.T) {
	odd := newGitUpstream(t, "odd")
	other := newGitUpstream(t, "other")
	upstream := newGitUpstream(t, "upstream")
	upstream.addSubmodule(odd, "odd", "odd=name")
	upstream.addSubmodule(other, "other", "")
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	_, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", New: upstream.entry(upstream.head)})
	verifySingleProblem(t, problems, "sub", "unable to update nested submodules", "odd:", "equals sign")
	verifyAbsent(t, root, "sub/odd/file")
	verifyContents(t, root, "sub/other/file", "first")
}

// TestEndpointGitModeNestedFailureLimit tests that the problem for failures to
// update nested submodules doesn't grow with their number.
func TestEndpointGitModeNestedFailureLimit(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	var bares []string
	for n := 0; n < maximumNestedFailures+2; n++ {
		nested := newGitUpstream(t, fmt.Sprintf("nested-%d", n))
		upstream.addSubmodule(nested, fmt.Sprintf("nested-%d", n), "")
		bares = append(bares, nested.bare)
	}
	for _, bare := range bares {
		if err := os.RemoveAll(bare); err != nil {
			t.Fatal(err)
		}
	}
	root := newGitRoot(t, nil)
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	_, problems := transitionEndpoint(t, context.Background(), e, &core.Change{Path: "sub", New: upstream.entry(upstream.head)})
	verifySingleProblem(t, problems, "sub", "unable to update nested submodules", "nested-0:", "and 2 more")
	if strings.Contains(problems[0].Error, fmt.Sprintf("nested-%d:", maximumNestedFailures)) {
		t.Error("too many failures described:", problems[0].Error)
	}
}

// TestEndpointGitModeSymbolicLinkReplacement tests that a directory replaces a
// symbolic link without the rules at the link's target (or the fact that it
// doesn't have one) playing a part.
func TestEndpointGitModeSymbolicLinkReplacement(t *testing.T) {
	isolateGit(t)
	for _, test := range []struct{ name, target string }{
		{"cycle", "pkg"},
		{"dangling", "missing"},
		{"rules at target", "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rules := map[string]string{"other/.gitignore": "file.txt\n"}
			sourceRoot := newGitRoot(t, map[string]string{"pkg/file.txt": "incoming", "pkg/nested/file.txt": "nested"})
			targetRoot := newGitRoot(t, nil)
			writeTree(t, sourceRoot, rules)
			writeTree(t, targetRoot, rules)
			if err := os.Symlink(test.target, filepath.Join(targetRoot, "pkg")); err != nil {
				t.Skip("unable to create symbolic link:", err)
			}
			source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
			target := newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch)
			problems, err := propagate(t, source, target, nil)
			if err != nil {
				t.Fatal(err)
			} else if len(problems) != 0 {
				t.Fatal("unexpected problems:", problems)
			}
			if info, err := os.Lstat(filepath.Join(targetRoot, "pkg")); err != nil || !info.IsDir() {
				t.Errorf("symbolic link not replaced: %v, %v", info, err)
			}
			verifyContents(t, targetRoot, "pkg/file.txt", "incoming")
			verifyContents(t, targetRoot, "pkg/nested/file.txt", "nested")
			verifyAbsent(t, targetRoot, "other/file.txt")
		})
	}
}
