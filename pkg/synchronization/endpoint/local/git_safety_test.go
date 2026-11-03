package local

import (
	"context"
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

// TestEndpointGitModeNestedRepositoryRuleFiles checks that the rule files of a
// nested repository without an origin, which is synchronized as a plain
// directory, are delivered like any others, including into a directory that the
// receiver excludes, while its .git entry stays where it is.
func TestEndpointGitModeNestedRepositoryRuleFiles(t *testing.T) {
	isolateGit(t)
	for _, test := range []struct{ name, rules, excluded string }{
		{name: "not excluded"},
		{name: "excluded", rules: "nested/\n", excluded: "nested"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourceRoot := newGitRoot(t, nil)
			initializeRepository(t, filepath.Join(sourceRoot, "nested"))
			writeTree(t, sourceRoot, map[string]string{
				"nested/.gitignore":     "*.tmp\n",
				"nested/data":           "data",
				"nested/sub/.gitignore": "*.log\n",
			})
			targetRoot := newGitRoot(t, map[string]string{".git/info/exclude": test.rules})
			source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
			target := newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch)
			snapshot := scanEndpoint(t, source, true)
			verifyKinds(t, snapshot, map[string]core.EntryKind{
				"nested":      core.EntryKind_Directory,
				"nested/.git": core.EntryKind_Untracked,
			})

			// Use the changes that reconciliation computes, whose new content
			// doesn't include untracked entries, unlike propagateChanges.
			_, _, changes, conflicts := core.ReconcileGitMode(nil, snapshot.Content, scanEndpoint(t, target, true).Content, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
			if len(conflicts) != 0 || len(changes) != 1 {
				t.Fatalf("incorrect reconciliation: %v, %v", changes, conflicts)
			}
			paths, digests := core.TransitionDependencies(changes)
			filtered, signatures, receiver, err := target.Stage(paths, digests)
			if err != nil {
				t.Fatal("staging failed:", err)
			} else if err := source.Supply(filtered, signatures, receiver); err != nil {
				t.Fatal(err)
			}
			_, problems, missing, err := target.Transition(context.Background(), changes)
			if err != nil {
				t.Fatal("transition failed:", err)
			} else if missing {
				t.Error("staged files missing")
			}
			verifyContents(t, targetRoot, "nested/.gitignore", "*.tmp\n")
			verifyContents(t, targetRoot, "nested/sub/.gitignore", "*.log\n")
			verifyAbsent(t, targetRoot, "nested/.git")
			if test.excluded != "" {
				verifyExclusionWarning(t, problems, test.excluded)
				verifyAbsent(t, targetRoot, "nested/data")
			} else {
				if len(problems) != 0 {
					t.Error("unexpected problems:", problems)
				}
				verifyContents(t, targetRoot, "nested/data", "data")
			}
		})
	}
}

// TestEndpointGitModeGitEntriesRefused checks changes at or beneath entries
// named .git, which scans never produce, but which a malfunctioning or hostile
// controller could construct. There's no exception for rule files there, so
// nothing is requested or written, however a change is shaped. The refusals are
// failures that don't suggest anything that couldn't work, and they don't
// affect anything else.
func TestEndpointGitModeGitEntriesRefused(t *testing.T) {
	isolateGit(t)
	const contents = "*.tmp\n"
	directory := func(contents map[string]*core.Entry) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}

	// setup creates a source and a target. The target's Git directories have
	// the parents of everything that the changes below try to write, since
	// they'd fail for lack of a parent otherwise, whether or not they're
	// refused. It also returns an entry for a file that the source can supply.
	setup := func(t *testing.T) (*endpoint, *endpoint, *core.Entry) {
		sourceRoot := newGitRoot(t, map[string]string{"public": contents})
		targetRoot := newGitRoot(t, map[string]string{"other/file": "file"})
		initializeRepository(t, targetRoot, "--template=")
		initializeRepository(t, filepath.Join(targetRoot, "nested"), "--template=")
		writeTree(t, targetRoot, map[string]string{
			".GIT/sample":          "sample",
			".git/hooks/sample":    "sample",
			".git/info/sample":     "sample",
			".git/x/sample":        "sample",
			"nested/.git/x/sample": "sample",
		})
		source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
		target := newGitModeEndpoint(t, targetRoot, synchronization.WatchMode_WatchModeNoWatch)
		file := entryAt(scanEndpoint(t, source, true), "public")
		scanEndpoint(t, target, true)
		return source, target, file
	}

	// apply stages the content of changes, which the source has for every path,
	// and transitions the target, verifying that it only requests some of the
	// paths and that its refusals are the expected failures.
	apply := func(t *testing.T, source, target *endpoint, changes []*core.Change, requested, refused []string) []*core.Entry {
		t.Helper()
		paths, digests := core.TransitionDependencies(changes)
		for _, path := range paths {
			writeTree(t, source.root, map[string]string{path: contents})
		}
		filtered, signatures, receiver, err := target.Stage(paths, digests)
		if err != nil {
			t.Fatal("staging failed:", err)
		}
		if actual := slices.Sorted(slices.Values(filtered)); !slices.Equal(actual, requested) {
			t.Errorf("incorrect paths requested: %v != %v", actual, requested)
		}
		if len(filtered) > 0 {
			if err := source.Supply(filtered, signatures, receiver); err != nil {
				t.Fatal(err)
			}
		}
		results, problems, missing, err := target.Transition(context.Background(), changes)
		if err != nil {
			t.Fatal("transition failed:", err)
		} else if missing {
			t.Error("staged files missing")
		}
		var actual []string
		for _, problem := range problems {
			actual = append(actual, problem.Path)
			if problem.Warning {
				t.Error("refusal reported as a warning:", problem)
			}
			if !strings.Contains(problem.Error, "never synchronized") {
				t.Error("incorrect refusal:", problem)
			}
			if strings.Contains(problem.Error, "ignore.paths") || strings.Contains(problem.Error, "!") {
				t.Error("refusal suggests a pattern, which can't work:", problem)
			}
		}
		if !slices.Equal(actual, refused) {
			t.Errorf("incorrect refusals: %v != %v", actual, refused)
		}
		return results
	}

	// Staging must not request even the bytes of what would be refused. Nothing
	// else asks for a receiver here, so there mustn't be one.
	t.Run("staging", func(t *testing.T) {
		_, target, file := setup(t)
		paths := []string{
			".GIT/.gitignore",
			".git/.gitignore",
			".git/hooks/.gitignore",
			".git/hooks/pre-commit",
			".git/info/exclude",
			".git/x/.gitignore",
			"nested/.git/x/.gitignore",
		}
		digests := make([][]byte, len(paths))
		for p := range paths {
			digests[p] = file.Digest
		}
		filtered, _, receiver, err := target.Stage(paths, digests)
		if err != nil || len(filtered) != 0 || receiver != nil {
			t.Fatalf("files in Git directories requested: %v, %v", filtered, err)
		}
	})

	// Changes at the paths of individual files.
	t.Run("files", func(t *testing.T) {
		source, target, file := setup(t)
		root := target.root
		refused := []string{
			".GIT/.gitignore",
			".git/.gitignore",
			".git/hooks/.gitignore",
			".git/hooks/pre-commit",
			".git/info/exclude",
			".git/x/.gitignore",
			"nested/.git/x/.gitignore",
		}
		var changes []*core.Change
		for _, path := range refused {
			changes = append(changes, &core.Change{Path: path, New: file})
		}
		changes = append(changes, &core.Change{Path: "nested/.gitignore", New: file}, &core.Change{Path: "public", New: file})
		results := apply(t, source, target, changes,
			[]string{"nested/.gitignore", "public"},
			[]string{".GIT", ".git", "nested/.git"},
		)
		for c, path := range refused {
			verifyAbsent(t, root, path)
			if results[c] != nil {
				t.Errorf("incorrect result for %s: %v", path, results[c])
			}
		}
		verifyContents(t, root, "nested/.gitignore", contents)
		verifyContents(t, root, "public", contents)
		if !results[len(refused)].Equal(file, true) || !results[len(refused)+1].Equal(file, true) {
			t.Error("incorrect results for other changes:", results[len(refused):])
		}
	})

	// Changes that create directories, either beneath a Git entry, as a Git
	// entry, or with a Git entry among their contents.
	t.Run("directories", func(t *testing.T) {
		source, target, file := setup(t)
		root := target.root
		rules := directory(map[string]*core.Entry{".gitignore": file})
		results := apply(t, source, target,
			[]*core.Change{
				{Path: ".git/y", New: rules},
				{Path: "new", New: directory(map[string]*core.Entry{
					".git": directory(map[string]*core.Entry{
						".gitignore": file,
						"hooks":      directory(map[string]*core.Entry{".gitignore": file, "pre-commit": file}),
						"info":       directory(map[string]*core.Entry{"exclude": file}),
					}),
					".gitignore": file,
					"public":     file,
				})},
				{Path: "other/.git", New: rules},
				{Path: "public", New: file},
			},
			[]string{"new/.gitignore", "new/public", "public"},
			[]string{".git", "new/.git", "other/.git"},
		)
		for _, path := range []string{".git/y", "new/.git", "other/.git"} {
			verifyAbsent(t, root, path)
		}
		verifyContents(t, root, "new/.gitignore", contents)
		verifyContents(t, root, "new/public", contents)
		verifyContents(t, root, "public", contents)
		expected := []*core.Entry{nil, directory(map[string]*core.Entry{".gitignore": file, "public": file}), nil, file}
		for r, result := range results {
			if !result.Equal(expected[r], true) {
				t.Errorf("incorrect result %d: %v", r, result)
			}
		}
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
	gitLink := &core.Entry{Kind: core.EntryKind_GitLink, Digest: []byte{1}, Url: "https://example.org/repository.git"}
	moved := &core.Entry{Kind: core.EntryKind_GitLink, Digest: []byte{2}, Url: "https://example.org/repository.git"}
	submodule := func(contents map[string]*core.Entry) *core.Entry {
		result := directory(map[string]*core.Entry{core.GitLinkName: gitLink})
		for name, content := range contents {
			result.Contents[name] = content
		}
		return result
	}

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
			name:     "rule file replaced by a directory that only rules for directories exclude",
			rules:    ".gitignore/\n",
			changes:  []*core.Change{{Path: ".gitignore", Old: file, New: directory(map[string]*core.Entry{".gitignore": other, "sub": directory(map[string]*core.Entry{".gitignore": other})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{".gitignore": other, "sub": directory(map[string]*core.Entry{".gitignore": other})})},
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
		{
			name: "rule files in Git directories",
			changes: []*core.Change{
				{Path: ".git/.gitignore", New: file},
				{Path: ".git/hooks/.gitignore", New: file},
				{Path: ".git/info/exclude", New: file},
				{Path: ".git/x/.gitignore", Old: other, New: file},
				{Path: "a/.git/x/.gitignore", New: file},
				{Path: ".GIT/.gitignore", New: file},
				{Path: "public", New: file},
			},
			expected: []*core.Entry{nil, nil, nil, nil, nil, nil, file},
			failures: failures{".git", "a/.git", ".GIT"},
		},
		{
			name:     "other content in Git directories",
			changes:  []*core.Change{{Path: ".git/hooks/pre-commit", New: file}, {Path: "a/.git/config", New: other}, {Path: ".git/hooks/link", New: link}},
			expected: []*core.Entry{nil, nil, nil},
			failures: failures{".git", "a/.git"},
		},
		{
			name:     "Git entries of any kind",
			changes:  []*core.Change{{Path: ".git", New: file}, {Path: "a/.git", New: link}, {Path: "b/.Git", New: file}, {Path: "c/.git", New: directory(nil)}},
			expected: []*core.Entry{nil, nil, nil, nil},
			failures: failures{".git", "a/.git", "b/.Git", "c/.git"},
		},
		{
			name: "directory named .git with rule files",
			changes: []*core.Change{{Path: ".git", New: directory(map[string]*core.Entry{
				".gitignore": file,
				"hooks":      directory(map[string]*core.Entry{".gitignore": other, "pre-commit": file}),
				"info":       directory(map[string]*core.Entry{"exclude": file}),
			})}},
			expected: []*core.Entry{nil},
			failures: failures{".git"},
		},
		{
			name:     "directory beneath a Git entry with rule files",
			changes:  []*core.Change{{Path: "a/.git/x", New: directory(map[string]*core.Entry{".gitignore": file})}},
			expected: []*core.Entry{nil},
			failures: failures{"a/.git"},
		},
		{
			name:     "Git entry among other content",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{".git": directory(map[string]*core.Entry{".gitignore": file}), ".gitignore": file, "a": file})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{".gitignore": file, "a": file})},
			failures: failures{"pkg/.git"},
		},
		{
			name:     "Git entry deep in other content",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{"a": file, "sub": directory(map[string]*core.Entry{".GIT": directory(map[string]*core.Entry{".gitignore": file}), "b": file})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"a": file, "sub": directory(map[string]*core.Entry{"b": file})})},
			failures: failures{"pkg/sub/.GIT"},
		},
		{
			name:     "Git entry in root",
			changes:  []*core.Change{{Path: "", New: directory(map[string]*core.Entry{".git": directory(map[string]*core.Entry{"info": directory(map[string]*core.Entry{"exclude": file})}), "a": file})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"a": file})},
			failures: failures{".git"},
		},
		{
			name:     "Git entry in excluded directory with rule files",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs", New: directory(map[string]*core.Entry{".git": directory(map[string]*core.Entry{".gitignore": other}), ".gitignore": file})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{".gitignore": file})},
			failures: failures{"outputs/.git"},
		},
		{
			name:     "Git entry in excluded directory without rule files",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs", New: directory(map[string]*core.Entry{".git": directory(map[string]*core.Entry{".gitignore": other})})}},
			expected: []*core.Entry{nil},
			warnings: warnings{"outputs"},
			failures: failures{"outputs/.git"},
		},
		{
			name:     "rule file beneath Git entry beneath excluded path",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs/.git/.gitignore", New: file}, {Path: "outputs/.gitignore", New: other}},
			expected: []*core.Entry{nil, other},
			failures: failures{"outputs/.git"},
		},
		{
			name:     "rule files beneath names that resemble a Git entry",
			rules:    ".gitx/\nx.git/\n",
			changes:  []*core.Change{{Path: ".gitx/.gitignore", New: file}, {Path: "x.git/.gitignore", New: file}},
			expected: []*core.Entry{file, file},
		},
		{
			name:     "other content beneath names that resemble a Git entry",
			rules:    ".gitx/\n",
			changes:  []*core.Change{{Path: ".gitx/file", New: file}, {Path: "x.git/file", New: file}},
			expected: []*core.Entry{nil, file},
			warnings: warnings{".gitx"},
		},
		{
			name:     "submodule Git entries attached and followed",
			rules:    "*.log\n",
			changes:  []*core.Change{{Path: "sub/.git", New: gitLink}, {Path: "a/b/.git", Old: gitLink, New: moved}},
			expected: []*core.Entry{gitLink, moved},
		},
		{
			name:     "submodule Git entries in excluded directories",
			rules:    "excluded/\n",
			changes:  []*core.Change{{Path: "excluded/.git", New: gitLink}, {Path: "excluded/sub/.git", Old: gitLink, New: moved}, {Path: "public", New: file}},
			expected: []*core.Entry{nil, nil, file},
			warnings: warnings{"excluded"},
		},
		{
			name: "submodule Git entries where there can't be any",
			changes: []*core.Change{
				{Path: ".git", New: gitLink},
				{Path: "a/.git/b/.git", New: gitLink},
				{Path: "b/.GIT", New: gitLink},
				{Path: "c/.git/x", New: gitLink},
				{Path: "d/.git", Old: directory(nil), New: gitLink},
			},
			expected: []*core.Entry{nil, nil, nil, nil, nil},
			failures: failures{".git", "a/.git", "b/.GIT", "c/.git", "d/.git"},
		},
		{
			name:     "other content at submodule Git entries",
			changes:  []*core.Change{{Path: "a/.git", New: file}, {Path: "b/.git", Old: gitLink, New: directory(nil)}, {Path: "c/.git/HEAD", New: file}},
			expected: []*core.Entry{nil, nil, nil},
			failures: failures{"a/.git", "b/.git", "c/.git"},
		},
		{
			name:     "submodule created whole",
			rules:    "*.log\nbuild/\n",
			changes:  []*core.Change{{Path: "sub", New: submodule(map[string]*core.Entry{"a.log": file, "b": file, "build": directory(map[string]*core.Entry{"x": file})})}},
			expected: []*core.Entry{submodule(map[string]*core.Entry{"a.log": file, "b": file, "build": directory(map[string]*core.Entry{"x": file})})},
		},
		{
			name:     "submodule replacing other content",
			rules:    "*.log\n",
			changes:  []*core.Change{{Path: "sub", Old: directory(map[string]*core.Entry{"file": file}), New: submodule(map[string]*core.Entry{"a.log": file})}},
			expected: []*core.Entry{submodule(map[string]*core.Entry{"a.log": file})},
		},
		{
			name:     "submodule created within a directory",
			rules:    "*.log\n",
			changes:  []*core.Change{{Path: "pkg", New: directory(map[string]*core.Entry{"c.log": file, "sub": submodule(map[string]*core.Entry{"a.log": file})})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{"sub": submodule(map[string]*core.Entry{"a.log": file})})},
			warnings: warnings{"pkg/c.log"},
		},
		{
			name:     "excluded submodules",
			rules:    "sub/\npkg/\n",
			changes:  []*core.Change{{Path: "sub", New: submodule(nil)}, {Path: "pkg/inner", New: submodule(nil)}, {Path: "public", New: file}},
			expected: []*core.Entry{nil, nil, file},
			warnings: warnings{"sub", "pkg"},
		},
		{
			name:     "submodule in excluded directory with rule files",
			rules:    "outputs/\n",
			changes:  []*core.Change{{Path: "outputs", New: directory(map[string]*core.Entry{".gitignore": file, "sub": submodule(nil)})}},
			expected: []*core.Entry{directory(map[string]*core.Entry{".gitignore": file})},
			warnings: warnings{"outputs"},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if slices.ContainsFunc(test.failures, func(path string) bool { return strings.Contains(path, long) }) {
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

// TestPruneGitTransitionsOldContent tests that pruneGitTransitions refuses
// changes that would remove a Git entry or anything beneath one. The only Git
// entries that the ancestor or the synchronizable part of a scan can have are
// the GitLink entries of submodules, which are never removed, but which can be
// attached to a directory or moved to another commit. Anything else could only
// be requested by a malfunctioning or compromised controller.
func TestPruneGitTransitionsOldContent(t *testing.T) {
	// Create the entries for tests.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	link := &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "target"}
	untracked := &core.Entry{Kind: core.EntryKind_Untracked}
	directory := func(contents map[string]*core.Entry) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	gitLink := &core.Entry{Kind: core.EntryKind_GitLink, Digest: []byte{1}, Url: "https://example.org/repository.git"}
	moved := &core.Entry{Kind: core.EntryKind_GitLink, Digest: []byte{2}, Url: "https://example.org/repository.git"}
	submodule := directory(map[string]*core.Entry{".git": gitLink, "file": file})

	// Define test cases.
	tests := []struct {
		name     string
		change   *core.Change
		refused  bool
		failures []string
	}{
		{"Git entry", &core.Change{Path: ".git", Old: directory(nil)}, true, []string{".git"}},
		{"untracked Git entry", &core.Change{Path: ".git", Old: untracked}, true, []string{".git"}},
		{"nested Git entry", &core.Change{Path: "a/.Git", Old: link}, true, []string{"a/.Git"}},
		{"directory beneath Git entry", &core.Change{Path: ".git/hooks", Old: directory(nil)}, true, []string{".git"}},
		{"symbolic link beneath Git entry", &core.Change{Path: "a/.GIT/link", Old: link}, true, []string{"a/.GIT"}},
		{"file beneath Git entry", &core.Change{Path: ".git/HEAD", Old: file}, true, []string{".git"}},
		{"Git entry in deletion", &core.Change{Path: "a", Old: directory(map[string]*core.Entry{".git": directory(nil), "b": file})}, true, []string{"a/.git"}},
		{"Git entry deep in deletion", &core.Change{Path: "a", Old: directory(map[string]*core.Entry{"b": directory(map[string]*core.Entry{".GIT": link})})}, true, []string{"a/b/.GIT"}},
		{"Git entry in type change", &core.Change{Path: "a", Old: directory(map[string]*core.Entry{".git": directory(nil)}), New: file}, true, []string{"a/.git"}},
		{"Git entry in root replacement", &core.Change{Old: directory(map[string]*core.Entry{".git": file}), New: directory(map[string]*core.Entry{"a": file})}, true, []string{".git"}},
		{"Git entry in nothing but old content", &core.Change{Path: "a", Old: directory(map[string]*core.Entry{".git": link}), New: directory(nil)}, true, []string{"a/.git"}},
		{"untracked Git entry in deletion", &core.Change{Path: "a", Old: directory(map[string]*core.Entry{".git": untracked, "b": file})}, true, []string{"a/.git"}},
		{"names that resemble a Git entry", &core.Change{Path: ".gitx", Old: directory(map[string]*core.Entry{"x.git": file, ".git2": link})}, false, nil},
		{"unrelated deletion", &core.Change{Path: "a", Old: file}, false, nil},
		{"submodule Git entry", &core.Change{Path: "sub/.git", Old: gitLink}, true, []string{"sub/.git"}},
		{"submodule Git entry replaced", &core.Change{Path: "sub/.git", Old: gitLink, New: file}, true, []string{"sub/.git"}},
		{"submodule in deletion", &core.Change{Path: "sub", Old: submodule}, true, []string{"sub/.git"}},
		{"submodule deep in deletion", &core.Change{Path: "a", Old: directory(map[string]*core.Entry{"b": submodule})}, true, []string{"a/b/.git"}},
		{"submodule in type change", &core.Change{Path: "sub", Old: submodule, New: file}, true, []string{"sub/.git"}},
		{"submodule replaced by submodule", &core.Change{Path: "sub", Old: submodule, New: directory(map[string]*core.Entry{".git": moved})}, true, []string{"sub/.git"}},
		{"Git entry replaced by submodule Git entry", &core.Change{Path: "sub/.git", Old: directory(nil), New: gitLink}, true, []string{"sub/.git"}},
		{"untracked Git entry replaced by submodule Git entry", &core.Change{Path: "sub/.git", Old: untracked, New: gitLink}, true, []string{"sub/.git"}},
		{"submodule Git entry attached", &core.Change{Path: "sub/.git", New: gitLink}, false, nil},
		{"submodule Git entry followed", &core.Change{Path: "sub/.git", Old: gitLink, New: moved}, false, nil},
	}

	// Process test cases. Each is accompanied by a change that's unaffected.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unaffected := &core.Change{Path: "unaffected", New: file}
			results, problems := pruneGitTransitions(gitignore.NewIgnorer(t.TempDir(), nil), []*core.Change{test.change, unaffected})
			if test.refused && results[0] != nil {
				t.Error("change not refused:", results[0])
			} else if !test.refused && results[0] != test.change {
				t.Error("change not left as is:", results[0])
			}
			if results[1] != unaffected {
				t.Error("unaffected change not left as is:", results[1])
			}
			var failures []string
			for _, problem := range problems {
				if problem.Warning {
					t.Error("refusal reported as a warning:", problem)
				}
				failures = append(failures, problem.Path)
			}
			if !slices.Equal(failures, test.failures) {
				t.Errorf("incorrect failures: %v != %v", failures, test.failures)
			}
		})
	}
}

// TestPruneGitTransitionsSubmoduleMessages tests that refusing to remove a
// submodule, which is expected, says what to do about it, rather than treating
// it as a sign of a malfunction, as other changes to Git entries are.
func TestPruneGitTransitionsSubmoduleMessages(t *testing.T) {
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	gitLink := &core.Entry{Kind: core.EntryKind_GitLink, Digest: []byte{1}, Url: "https://example.org/repository.git"}
	submodule := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{".git": gitLink}}
	_, problems := pruneGitTransitions(gitignore.NewIgnorer(t.TempDir(), nil), []*core.Change{
		{Path: "a/.git", Old: gitLink},
		{Path: "b", Old: submodule, New: file},
		{Path: "c/.git", Old: gitLink, New: file},
		{Path: "d/.git", New: file},
		{Path: "e/.git", Old: &core.Entry{Kind: core.EntryKind_Directory}, New: gitLink},
	})
	expected := []struct {
		path      string
		submodule bool
	}{{"a/.git", true}, {"b/.git", true}, {"c/.git", true}, {"d/.git", false}, {"e/.git", false}}
	if len(problems) != len(expected) {
		t.Fatal("incorrect problems:", problems)
	}
	for p, problem := range problems {
		if problem.Path != expected[p].path {
			t.Errorf("incorrect path for problem %d: %s", p, problem.Path)
		} else if problem.Warning {
			t.Error("refusal reported as a warning:", problem)
		} else if expected[p].submodule != strings.Contains(problem.Error, "submodule isn't removed automatically") {
			t.Error("incorrect message:", problem)
		} else if expected[p].submodule == strings.Contains(problem.Error, "compromised") {
			t.Error("incorrect message:", problem)
		}
	}
}

// TestEndpointGitModeRemovalsFromGitEntries tests that crafted changes don't
// remove anything from Git entries on disk, including the empty directories and
// symbolic links that removals don't need any cached information about, and that
// a change that's refused for this doesn't remove anything else either.
func TestEndpointGitModeRemovalsFromGitEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links may require privileges")
	}

	// Create Git entries with content that could be removed, along with enough
	// other content for the endpoint to accept that many entries being removed.
	isolateGit(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a": "a", "b": "b", "c": "c", "d": "d", "e": "e", "f": "f", "plain/file": "file"})
	initializeRepository(t, root)
	initializeRepository(t, filepath.Join(root, "plain"))
	for _, path := range []string{".git/refs/tags", "nested/.git/refs", "typed/.git"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(path)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("HEAD", filepath.Join(root, ".git", "link")); err != nil {
		t.Fatal(err)
	}
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanEndpoint(t, e, true)

	// Create changes that would remove that content.
	empty := &core.Entry{Kind: core.EntryKind_Directory}
	directory := func(contents map[string]*core.Entry) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	refs := directory(map[string]*core.Entry{"refs": empty})
	changes := []*core.Change{
		{Path: ".git/refs/tags", Old: empty},
		{Path: ".git/link", Old: &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "HEAD"}},
		{Path: "nested", Old: directory(map[string]*core.Entry{".git": refs})},
		{Path: "typed", Old: directory(map[string]*core.Entry{".git": empty}), New: empty},
		{Path: "plain", Old: entryAt(snapshot, "plain")},
	}

	// Perform the changes.
	results, problems, _, err := e.Transition(context.Background(), changes)
	if err != nil {
		t.Fatal("transition failed:", err)
	}

	// Everything is still there.
	for c := range changes {
		if !results[c].Equal(changes[c].Old, true) {
			t.Errorf("change %d not reported as refused: %v", c, results[c])
		}
	}
	for _, path := range []string{".git/refs/tags", ".git/link", "nested/.git/refs", "typed/.git", "plain/.git", "plain/file"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Errorf("%s removed or can't be checked: %v", path, err)
		}
	}

	// The refusals are failures that describe the Git entries.
	var reported []string
	for _, problem := range problems {
		reported = append(reported, problem.Path)
		if problem.Warning {
			t.Error("problem reported as a warning:", problem)
		}
	}
	if expected := []string{".git", "nested/.git", "typed/.git", "plain/.git"}; !slices.Equal(reported, expected) {
		t.Errorf("incorrect problems: %v != %v", reported, expected)
	}
}

// TestEndpointGitModePathsOutsideRoot tests that a crafted path that leads out
// of the root doesn't reveal anything about the rules there, and that it doesn't
// affect anything there.
func TestEndpointGitModePathsOutsideRoot(t *testing.T) {
	// Create rules outside of the root that exclude what the changes create.
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	writeTree(t, parent, map[string]string{".gitignore": "secret\n", "root/public": "public"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	scanEndpoint(t, e, true)

	// Perform the changes.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	changes := []*core.Change{
		{Path: "../secret", New: file},
		{Path: "../other", New: &core.Entry{Kind: core.EntryKind_Directory}},
		{Path: "../secret/file", New: file},
	}
	results, problems, _, err := e.Transition(context.Background(), changes)
	if err != nil {
		t.Fatal("transition failed:", err)
	}

	// All of them are refused, as failures that don't depend on any rules.
	for c, result := range results {
		if result != nil {
			t.Errorf("change %d applied: %v", c, result)
		}
	}
	reported := make(map[string]bool)
	for _, problem := range problems {
		reported[problem.Path] = true
		if problem.Warning {
			t.Error("rules outside of the root used to warn:", problem)
		}
	}
	for _, change := range changes {
		if !reported[change.Path] {
			t.Errorf("no problem for %s: %v", change.Path, problems)
		}
	}
	verifyAbsent(t, parent, "secret")
	verifyAbsent(t, parent, "other")
}
