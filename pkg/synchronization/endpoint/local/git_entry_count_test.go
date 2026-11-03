package local

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

const (
	// stagingLimitError is the error returned by Stage if it's asked to stage
	// more paths than the maximum entry count.
	stagingLimitError = "staging would exceed allowed entry count"
	// transitionLimitProblem is the problem reported for a change that
	// Transition refuses because of the maximum entry count.
	transitionLimitProblem = "applying change would exceed allowed entry count"
)

// newLimitedGitModeEndpoint creates an endpoint in Git mode for the specified
// root that's limited to the specified maximum entry count.
func newLimitedGitModeEndpoint(t *testing.T, root string, maximumEntryCount uint64) *endpoint {
	t.Helper()
	return newConfiguredGitModeEndpoint(t, root, &synchronization.Configuration{
		WatchMode:         synchronization.WatchMode_WatchModeNoWatch,
		MaximumEntryCount: maximumEntryCount,
	})
}

// newLimitedGitModeEndpoints creates a source endpoint and a target endpoint in
// Git mode, for repositories with the specified files, with the target limited
// to the specified maximum entry count and with the specified rules in its
// repository's exclude file, which aren't synchronized and don't count as
// entries. The target is created last, so the Mutagen data directory in the
// environment is the target's. It returns the endpoints and the target's root.
func newLimitedGitModeEndpoints(
	t *testing.T,
	sourceFiles, targetFiles map[string]string,
	rules string,
	maximumEntryCount uint64,
) (*endpoint, *endpoint, string) {
	t.Helper()
	isolateGit(t)
	sourceRoot, targetRoot := newGitRoot(t, sourceFiles), newGitRoot(t, targetFiles)
	initializeRepository(t, sourceRoot)
	initializeRepository(t, targetRoot)
	writeTree(t, targetRoot, map[string]string{".git/info/exclude": rules})
	source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
	return source, newLimitedGitModeEndpoint(t, targetRoot, maximumEntryCount), targetRoot
}

// verifyGitModeStagingEmpty verifies that the staging root of the endpoint in
// Git mode that was created last doesn't exist or is empty.
func verifyGitModeStagingEmpty(t *testing.T) {
	t.Helper()
	stagingRoot, err := pathForMutagenStagingRoot(gitModeSession, true)
	if err != nil {
		t.Fatal("unable to compute staging root:", err)
	}
	entries, err := os.ReadDir(stagingRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal("unable to read staging root:", err)
	} else if len(entries) > 0 {
		t.Errorf("staging root not empty (%d entries)", len(entries))
	}
}

// verifyEntryCountProblems verifies that a list of problems consists of exactly
// one warning for each of the specified excluded paths and one refusal for each
// of the specified paths whose changes exceed the maximum entry count.
func verifyEntryCountProblems(t *testing.T, problems []*core.Problem, warnings, refusals []string) {
	t.Helper()
	var actualWarnings, actualRefusals []string
	for _, problem := range problems {
		if !problem.Warning {
			if problem.Error != transitionLimitProblem {
				t.Errorf("unexpected failure: %v", problem)
			}
			actualRefusals = append(actualRefusals, problem.Path)
		} else if !strings.Contains(problem.Error, "ignored by this endpoint's own rules") {
			t.Errorf("unexpected warning: %v", problem)
		} else {
			actualWarnings = append(actualWarnings, problem.Path)
		}
	}
	slices.Sort(actualWarnings)
	slices.Sort(actualRefusals)
	if !slices.Equal(actualWarnings, warnings) {
		t.Errorf("incorrect warnings: %v != %v", actualWarnings, warnings)
	}
	if !slices.Equal(actualRefusals, refusals) {
		t.Errorf("incorrect refusals: %v != %v", actualRefusals, refusals)
	}
}

// entryNames returns the sorted paths of an entry's contents relative to the
// entry, in addition to an empty path for the entry itself, or nil if the entry
// is nil.
func entryNames(entry *core.Entry) []string {
	if entry == nil {
		return nil
	}
	names := []string{""}
	var visit func(string, *core.Entry)
	visit = func(prefix string, parent *core.Entry) {
		for name, content := range parent.Contents {
			names = append(names, prefix+name)
			visit(prefix+name+"/", content)
		}
	}
	visit("", entry)
	slices.Sort(names)
	return names
}

// TestEndpointGitModeStageMaximumEntryCount tests that Stage only holds the
// paths that the endpoint would write against the maximum entry count. Paths
// that its rules exclude are never going to be entries, and they're computed as
// changes in every cycle, so they mustn't fail staging for everything else.
func TestEndpointGitModeStageMaximumEntryCount(t *testing.T) {
	// Each test case limits the target to three entries and has it stage a list
	// of paths from a source that has all of them. The rule file beneath the
	// excluded directory is exempt from exclusion, so it counts as a path.
	logs := []string{"a.log", "b.log", "c.log", "d.log", "e.log"}
	rule := "outputs/.gitignore"
	tests := []struct {
		// name is the name of the test.
		name string
		// paths are the paths to stage.
		paths []string
		// expected are the paths that are expected to be requested, unless
		// staging is expected to fail.
		expected []string
		// expectError indicates that staging is expected to fail because of the
		// maximum entry count.
		expectError bool
	}{
		{
			name:     "excluded paths above the maximum",
			paths:    slices.Concat(logs, []string{"keep", "outputs/data", rule}),
			expected: []string{"keep", rule},
		},
		{
			name:  "only excluded paths above the maximum",
			paths: logs,
		},
		{
			name:     "receivable paths at the maximum with excluded paths",
			paths:    slices.Concat([]string{"keep"}, logs, []string{"other", rule}),
			expected: []string{"keep", "other", rule},
		},
		{
			name:        "receivable paths above the maximum",
			paths:       []string{"keep", "other", "third", rule},
			expectError: true,
		},
		{
			name:        "receivable paths above the maximum with excluded paths",
			paths:       slices.Concat(logs, []string{"keep", "other", "third", rule}),
			expectError: true,
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create the endpoints and find the digests of the paths to stage.
			files := map[string]string{"keep": "keep", "other": "other", "third": "third", "outputs/data": "data", rule: "*.tmp\n"}
			for _, path := range logs {
				files[path] = path
			}
			source, target, _ := newLimitedGitModeEndpoints(t, files, nil, "*.log\noutputs/\n", 3)
			snapshot := scanEndpoint(t, source, true)
			scanEndpoint(t, target, true)
			var digests [][]byte
			for _, path := range test.paths {
				digests = append(digests, entryAt(snapshot, path).Digest)
			}

			// Stage the paths and supply what's requested.
			paths, signatures, receiver, err := target.Stage(test.paths, digests)
			if test.expectError {
				if err == nil || err.Error() != stagingLimitError {
					t.Fatal("incorrect staging failure:", err)
				}

				// Nothing should have been staged, or prepared to be.
				verifyGitModeStagingEmpty(t)
			} else if err != nil {
				t.Fatal("staging failed:", err)
			} else if !slices.Equal(paths, test.expected) {
				t.Fatal("incorrect paths requested:", paths)
			} else if len(paths) > 0 {
				if err := source.Supply(paths, signatures, receiver); err != nil {
					t.Fatal("unable to supply files:", err)
				}
				for _, path := range paths {
					digest := entryAt(snapshot, path).Digest
					if staged, err := target.stager.Contains(path, digest); err != nil {
						t.Error("unable to query staging:", err)
					} else if !staged {
						t.Errorf("%s not staged", path)
					}
				}
			} else if receiver != nil {
				t.Error("receiver created without paths")
			}

			// Whatever the outcome, this has used up the scan.
			_, _, _, err = target.Stage(test.paths, digests)
			if err == nil || !strings.Contains(err.Error(), "without scan") {
				t.Error("repeated staging not refused:", err)
			}
		})
	}
}

// TestEndpointMaximumEntryCountStageOrdering tests the order of Stage's checks
// without Git mode, in which a refusal also uses up the scan.
func TestEndpointMaximumEntryCountStageOrdering(t *testing.T) {
	// Create the endpoint.
	_, target := newEntryCountEndpoints(t, t.TempDir(), t.TempDir(), 2)
	e := target.(*endpoint)
	scanEndpoint(t, e, true)

	// Verify that paths above the maximum are refused and nothing is staged.
	paths := []string{"a", "b", "c"}
	digests := [][]byte{{1}, {2}, {3}}
	if _, _, _, err := e.Stage(paths, digests); err == nil || err.Error() != stagingLimitError {
		t.Fatal("incorrect staging failure:", err)
	}
	verifyTargetStagingEmpty(t)

	// Verify that a repeated attempt is refused for the lack of a scan.
	if _, _, _, err := e.Stage(paths, digests); err == nil || !strings.Contains(err.Error(), "without scan") {
		t.Error("repeated staging not refused:", err)
	}
}

// TestEndpointGitModeTransitionMaximumEntryCount tests that transitions in Git
// mode are held against the maximum entry count, and classified, according to
// what the endpoint would actually write, using the same sequence of operations
// as the controller.
func TestEndpointGitModeTransitionMaximumEntryCount(t *testing.T) {
	tests := []struct {
		// name is the name of the test.
		name string
		// source is the source's files.
		source map[string]string
		// target is the target's initial files.
		target map[string]string
		// rules is the contents of the target's exclude file.
		rules string
		// maximum is the target's maximum entry count. The target's root is
		// always one of its entries.
		maximum uint64
		// warnings are the paths with expected warnings about exclusions,
		// sorted.
		warnings []string
		// refusals are the paths of changes that are expected to be refused for
		// exceeding the maximum, sorted.
		refusals []string
		// kept are the paths of changes that are expected to leave their old
		// content in place.
		kept []string
		// pruned maps the paths of changes that are expected to be applied in
		// part to what their results are expected to contain. All other changes
		// are expected to be applied in full.
		pruned map[string][]string
		// contents are files that are expected on the target.
		contents map[string]string
		// absent are paths that aren't expected on the target.
		absent []string
	}{
		{
			// The paths that aren't excluded fit, even though there's more of
			// them than the maximum in all, so staging mustn't fail.
			name:     "excluded paths above the maximum",
			source:   map[string]string{"a.log": "a", "b.log": "b", "c.log": "c", "d.log": "d", "keep": "keep"},
			rules:    "*.log\n",
			maximum:  2,
			warnings: []string{"a.log", "b.log", "c.log", "d.log"},
			kept:     []string{"a.log", "b.log", "c.log", "d.log"},
			contents: map[string]string{"keep": "keep"},
			absent:   []string{"a.log", "b.log", "c.log", "d.log"},
		},
		{
			// The change has five entries, of which three are excluded, and the
			// root has room for three more entries.
			name:     "pruned content fits",
			source:   map[string]string{"pkg/a.go": "a", "pkg/b.log": "b", "pkg/c.log": "c", "pkg/d.log": "d"},
			rules:    "*.log\n",
			maximum:  4,
			warnings: []string{"pkg/b.log", "pkg/c.log", "pkg/d.log"},
			pruned:   map[string][]string{"pkg": {"", "a.go"}},
			contents: map[string]string{"pkg/a.go": "a"},
			absent:   []string{"pkg/b.log", "pkg/c.log", "pkg/d.log"},
		},
		{
			// The root is at the maximum, and the change has more entries than
			// the old content does, but not once it's pruned.
			name:     "pruned content is no increase at the maximum",
			source:   map[string]string{"pkg/a.go": "new", "pkg/b.log": "b", "pkg/c.log": "c"},
			target:   map[string]string{"pkg/a.go": "old"},
			rules:    "*.log\n",
			maximum:  3,
			warnings: []string{"pkg/b.log", "pkg/c.log"},
			pruned:   map[string][]string{"pkg": {"", "a.go"}},
			contents: map[string]string{"pkg/a.go": "new"},
			absent:   []string{"pkg/b.log", "pkg/c.log"},
		},
		{
			// The same, along with a change that really does exceed the
			// maximum, which requires that the changes be classified.
			name:     "pruned content is no increase beside a refusal",
			source:   map[string]string{"pkg/a.go": "new", "pkg/b.log": "b", "z": "z"},
			target:   map[string]string{"pkg/a.go": "old"},
			rules:    "*.log\n",
			maximum:  3,
			warnings: []string{"pkg/b.log"},
			refusals: []string{"z"},
			kept:     []string{"z"},
			pruned:   map[string][]string{"pkg": {"", "a.go"}},
			contents: map[string]string{"pkg/a.go": "new"},
			absent:   []string{"pkg/b.log", "z"},
		},
		{
			// The pruned content has two more entries than the old content,
			// and the root only has room for one. The excluded part is reported
			// even so, since it isn't written whatever happens to the rest.
			name:     "pruned content doesn't fit",
			source:   map[string]string{"pkg/a": "a", "pkg/b": "b", "pkg/c.log": "c"},
			target:   map[string]string{"pkg": "original"},
			rules:    "*.log\n",
			maximum:  3,
			warnings: []string{"pkg/c.log"},
			refusals: []string{"pkg"},
			kept:     []string{"pkg"},
			contents: map[string]string{"pkg": "original"},
		},
		{
			// A change that's refused as a whole by the rules must not use up
			// capacity that a later change needs.
			name:     "excluded change isn't charged",
			source:   map[string]string{"private/a": "a", "z": "z"},
			rules:    "private/\n",
			maximum:  3,
			warnings: []string{"private"},
			kept:     []string{"private"},
			contents: map[string]string{"z": "z"},
			absent:   []string{"private"},
		},
		{
			// A rule file is exempt from exclusion, and nothing else beneath
			// the excluded directory counts against its delivery.
			name:     "rule file in an excluded directory fits",
			source:   map[string]string{"outputs/.gitignore": "*.tmp\n", "outputs/x": "x", "outputs/y": "y"},
			rules:    "outputs/\n",
			maximum:  3,
			warnings: []string{"outputs"},
			pruned:   map[string][]string{"outputs": {"", ".gitignore"}},
			contents: map[string]string{"outputs/.gitignore": "*.tmp\n"},
			absent:   []string{"outputs/x", "outputs/y"},
		},
		{
			// A rule file that doesn't fit is still refused like anything else.
			name:     "rule file in an excluded directory doesn't fit",
			source:   map[string]string{"keep": "keep", "outputs/.gitignore": "*.tmp\n", "outputs/x": "x"},
			target:   map[string]string{"keep": "keep"},
			rules:    "outputs/\n",
			maximum:  3,
			warnings: []string{"outputs"},
			refusals: []string{"outputs"},
			kept:     []string{"outputs"},
			absent:   []string{"outputs"},
		},
		{
			// Results have to line up with changes that are refused for
			// different reasons, applied in different phases, and left
			// untouched by pruning. The root has four entries and room for one
			// more, and it gets another from the deletion. The file is replaced
			// by an excluded directory, so it stays, and it doesn't use up
			// anything that the last change needs.
			name:     "results correspond to changes",
			source:   map[string]string{"a": "a", "m": "new", "n/file": "file", "y/x": "x", "z": "z"},
			target:   map[string]string{"gone": "gone", "m": "old", "n": "original"},
			rules:    "n/\n",
			maximum:  5,
			warnings: []string{"n"},
			refusals: []string{"y"},
			kept:     []string{"n", "y"},
			contents: map[string]string{"a": "a", "m": "new", "n": "original", "z": "z"},
			absent:   []string{"gone", "y"},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Propagate the source's content.
			source, target, targetRoot := newLimitedGitModeEndpoints(t, test.source, test.target, test.rules, test.maximum)
			changes, results, problems, err := propagateChanges(t, source, target, nil)
			if err != nil {
				t.Fatal("staging failed:", err)
			}
			verifyEntryCountProblems(t, problems, test.warnings, test.refusals)

			// Verify the result of each change, and that its new content, which
			// is shared with the source's snapshot, hasn't been modified.
			sourceSnapshot := scanEndpoint(t, source, true)
			for c, change := range changes {
				if !change.New.Equal(entryAt(sourceSnapshot, change.Path), true) {
					t.Errorf("new content of %q modified", change.Path)
				}
				if slices.Contains(test.kept, change.Path) {
					if results[c] != change.Old {
						t.Errorf("incorrect result for refused change %q: %v", change.Path, results[c])
					}
				} else if names, ok := test.pruned[change.Path]; ok {
					if actual := entryNames(results[c]); !slices.Equal(actual, names) {
						t.Errorf("incorrect result for pruned change %q: %v", change.Path, actual)
					}
				} else if results[c] != change.New {
					t.Errorf("incorrect result for change %q: %v", change.Path, results[c])
				}
			}

			// Verify the content on disk and that no staged files remain.
			for path, contents := range test.contents {
				verifyContents(t, targetRoot, path, contents)
			}
			for _, path := range test.absent {
				verifyAbsent(t, targetRoot, path)
			}
			verifyGitModeStagingEmpty(t)
		})
	}
}

// TestEndpointGitModeMaximumEntryCountWarnings tests that an excluded path is
// reported once by each transition operation, even if changes beneath it would
// be performed in different phases. It also verifies that refusals don't modify
// the changes.
func TestEndpointGitModeMaximumEntryCountWarnings(t *testing.T) {
	// Create an endpoint with room for one more entry.
	root := newGitRoot(t, map[string]string{"public": "public", ".git/info/exclude": "outputs/\n"})
	e := newLimitedGitModeEndpoint(t, root, 3)

	// Create changes beneath the excluded directory, of which the first and the
	// third have more entries than their old content and the second doesn't,
	// along with the deletion of the file that isn't excluded, which is never
	// affected by the rules.
	public := entryAt(scanEndpoint(t, e, true), "public")
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	other := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{2}}
	directory := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"file": file, "other": other}}
	changes := []*core.Change{
		{Path: "outputs/a", New: directory},
		{Path: "outputs/b", Old: file, New: other},
		{Path: "outputs/c", New: file},
		{Path: "public", Old: public},
	}
	var originals []*core.Entry
	for _, change := range changes {
		originals = append(originals, change.New.Copy(core.EntryCopyBehaviorDeep))
	}

	// Perform the transition and verify the warning.
	results, problems := transitionEndpoint(t, context.Background(), e, changes...)
	verifyEntryCountProblems(t, problems, []string{"outputs"}, nil)

	// Verify the results, which have to be the old content by identity, and
	// that the changes' new content hasn't been modified.
	for c, expected := range []*core.Entry{nil, file, nil, nil} {
		if results[c] != expected {
			t.Errorf("incorrect result for change %d: %v", c, results[c])
		}
		if !changes[c].New.Equal(originals[c], true) {
			t.Errorf("new content of change %d modified", c)
		}
	}
	verifyAbsent(t, root, "public")
	verifyAbsent(t, root, "outputs")
}

// TestEndpointGitModeMaximumEntryCountRuleEvaluation tests that rules are
// evaluated against the state of the disk before anything is written, whether
// or not the maximum entry count applies. Rule-only synchronization cycles keep
// a session from combining rule changes with other changes, so these are
// crafted.
func TestEndpointGitModeMaximumEntryCountRuleEvaluation(t *testing.T) {
	// Rules that are removed by a change in the same operation still apply to
	// the others, so a change that they refuse keeps its old content.
	for _, test := range []struct {
		name    string
		maximum uint64
	}{{"at the maximum", 3}, {"without a meaningful maximum", 100}} {
		t.Run("removed rules/"+test.name, func(t *testing.T) {
			root := newGitRoot(t, map[string]string{".gitignore": "new/\n", "new": "original"})
			e := newLimitedGitModeEndpoint(t, root, test.maximum)
			snapshot := scanEndpoint(t, e, true)
			old := entryAt(snapshot, "new")
			file := &core.Entry{Kind: core.EntryKind_File, Digest: old.Digest}
			results, problems, _, err := e.Transition(context.Background(), []*core.Change{
				{Path: ".gitignore", Old: entryAt(snapshot, ".gitignore")},
				{Path: "new", Old: old, New: &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"file": file}}},
			})
			if err != nil {
				t.Fatal("transition failed:", err)
			}
			verifyEntryCountProblems(t, problems, []string{"new"}, nil)
			if results[0] != nil {
				t.Error("rule file not removed:", results[0])
			} else if results[1] != old {
				t.Error("incorrect result for refused change:", results[1])
			}
			verifyAbsent(t, root, ".gitignore")
			verifyContents(t, root, "new", "original")
		})
	}

	// Rules that are written by a change in the same operation don't apply to
	// the others yet. The next scan takes them into account.
	for _, test := range []struct {
		name     string
		maximum  uint64
		refusals []string
	}{{"near the maximum", 4, []string{"z"}}, {"without a meaningful maximum", 100, nil}} {
		t.Run("written rules/"+test.name, func(t *testing.T) {
			// The source has to be told not to apply the rules itself.
			isolateGit(t)
			sourceRoot := newGitRoot(t, map[string]string{".gitignore": "pkg/\n", "pkg/a": "a", "z": "z"})
			targetRoot := newGitRoot(t, map[string]string{".gitignore": "# none\n"})
			initializeRepository(t, sourceRoot)
			initializeRepository(t, targetRoot)
			source := newGitModeEndpoint(t, sourceRoot, synchronization.WatchMode_WatchModeNoWatch, "!pkg")
			target := newLimitedGitModeEndpoint(t, targetRoot, test.maximum)
			_, _, problems, err := propagateChanges(t, source, target, nil)
			if err != nil {
				t.Fatal("staging failed:", err)
			}
			verifyEntryCountProblems(t, problems, nil, test.refusals)
			verifyContents(t, targetRoot, ".gitignore", "pkg/\n")
			verifyContents(t, targetRoot, "pkg/a", "a")
			verifyKinds(t, scanEndpoint(t, target, true), map[string]core.EntryKind{"pkg": core.EntryKind_Untracked})
		})
	}
}

// TestEndpointGitModeRemovalValidation tests that the validation of the entries
// that transitions remove covers all of them, including those that are refused
// by the endpoint's rules, and that it precedes everything else.
func TestEndpointGitModeRemovalValidation(t *testing.T) {
	root := newGitRoot(t, map[string]string{"public": "public", ".git/info/exclude": "outputs/\n"})
	e := newLimitedGitModeEndpoint(t, root, 3)
	scanEndpoint(t, e, true)
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	old := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"a": file, "b": file, "c": file}}
	_, _, _, err := e.Transition(context.Background(), []*core.Change{{Path: "outputs", Old: old, New: file}})
	if err == nil || !strings.Contains(err.Error(), "removing more entries than exist") {
		t.Fatal("transition not refused:", err)
	}
	verifyContents(t, root, "public", "public")
	verifyAbsent(t, root, "outputs")
}
