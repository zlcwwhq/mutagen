package local

import (
	"context"
	"os"
	"path/filepath"
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
