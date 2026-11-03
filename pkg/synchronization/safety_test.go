package synchronization

import (
	"slices"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TODO: Implement tests for additional functions.

// TestOneEndpointEmptiedRoot tests oneEndpointEmptiedRoot with and without Git
// mode.
func TestOneEndpointEmptiedRoot(t *testing.T) {
	// Define entries.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	untracked := &core.Entry{Kind: core.EntryKind_Untracked}
	problematic := &core.Entry{Kind: core.EntryKind_Problematic, Problem: "problem"}
	directory := func(contents map[string]*core.Entry) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	empty := directory(nil)
	content := directory(map[string]*core.Entry{"a": file, "b": file})
	repository := directory(map[string]*core.Entry{".git": untracked})
	otherUntracked := directory(map[string]*core.Entry{".git": untracked, "output": untracked})
	repositoryWithContent := directory(map[string]*core.Entry{".git": untracked, "a": file, "b": file})
	hiddenByRule := directory(map[string]*core.Entry{"a": untracked, "b": untracked})
	partlyHidden := directory(map[string]*core.Entry{".git": untracked, "a": untracked})
	repositoryWithProblem := directory(map[string]*core.Entry{".git": untracked, "c": problematic})

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// ancestor is the root ancestor entry.
		ancestor *core.Entry
		// alpha is the root alpha entry.
		alpha *core.Entry
		// beta is the root beta entry.
		beta *core.Entry
		// expected is the expected result without Git mode.
		expected bool
		// expectedGitMode is the expected result in Git mode.
		expectedGitMode bool
	}{
		{"nil ancestor", nil, empty, content, false, false},
		{"file ancestor", file, empty, content, false, false},
		{"nil alpha", content, nil, content, false, false},
		{"nil beta", content, content, nil, false, false},
		{"file alpha", content, file, content, false, false},
		{"trivial ancestor", directory(map[string]*core.Entry{"a": file}), empty, content, false, false},
		{"neither emptied", content, content, content, false, false},
		{"alpha emptied", content, empty, content, true, true},
		{"beta emptied", content, content, empty, true, true},
		{"both emptied", content, empty, empty, false, false},
		{"alpha emptied except for repository", content, repository, repositoryWithContent, false, true},
		{"beta emptied except for repository", content, repositoryWithContent, repository, false, true},
		{"both emptied except for repository", content, repository, repository, false, false},
		{"alpha emptied except for repository, beta emptied", content, repository, empty, true, false},
		{"alpha emptied except for untracked content", content, otherUntracked, content, false, true},
		{"beta emptied except for untracked content", content, content, otherUntracked, false, true},
		{"alpha hidden by rule", content, hiddenByRule, content, false, false},
		{"beta hidden by rule", content, content, hiddenByRule, false, false},
		{"alpha partly hidden by rule", content, partlyHidden, content, false, false},
		{"beta partly hidden by rule", content, content, partlyHidden, false, false},
		{"alpha has problem", content, repositoryWithProblem, content, false, false},
		{"beta has problem", content, content, repositoryWithProblem, false, false},
	}

	// Process test cases.
	for _, test := range tests {
		if result := oneEndpointEmptiedRoot(test.ancestor, test.alpha, test.beta, false); result != test.expected {
			t.Errorf("%s: incorrect result without Git mode: %t != %t", test.description, result, test.expected)
		}
		if result := oneEndpointEmptiedRoot(test.ancestor, test.alpha, test.beta, true); result != test.expectedGitMode {
			t.Errorf("%s: incorrect result in Git mode: %t != %t", test.description, result, test.expectedGitMode)
		}
	}
}

// TestIgnoreRuleDeliveries tests ignoreRuleDeliveries.
func TestIgnoreRuleDeliveries(t *testing.T) {
	// Define entries.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	other := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{2}}
	link := &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "file"}
	directory := func(contents map[string]*core.Entry) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	onlyRules := directory(map[string]*core.Entry{".gitignore": file})
	withRules := directory(map[string]*core.Entry{"a": file, ".gitignore": file})
	withoutRules := directory(map[string]*core.Entry{"a": file})
	exclude := directory(map[string]*core.Entry{".git": directory(map[string]*core.Entry{
		"info": directory(map[string]*core.Entry{"exclude": file, "other": file}),
	})})
	excludeOnly := directory(map[string]*core.Entry{".git": directory(map[string]*core.Entry{
		"info": directory(map[string]*core.Entry{"exclude": file}),
	})})

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// changes are the changes.
		changes []*core.Change
		// expected are the expected deliveries.
		expected []*core.Change
	}{
		// Changes that have nothing to do with rule files.
		{"no changes", nil, nil},
		{"unrelated file", []*core.Change{{Path: "a", New: file}}, nil},
		{"unrelated deletion", []*core.Change{{Path: "a", Old: file}}, nil},
		{
			"similar names",
			[]*core.Change{
				{Path: ".gitignore2", New: file},
				{Path: "a.gitignore", New: file},
				{Path: ".gitignore/a", New: file},
				{Path: "exclude", New: file},
				{Path: "sub/.git/info/exclude", New: file},
			},
			nil,
		},

		// Changes at rule paths, which are included as is.
		{"rule file created", []*core.Change{{Path: ".gitignore", New: file}}, []*core.Change{{Path: ".gitignore", New: file}}},
		{"rule file modified", []*core.Change{{Path: ".gitignore", Old: file, New: other}}, []*core.Change{{Path: ".gitignore", Old: file, New: other}}},
		{"rule file removed", []*core.Change{{Path: ".gitignore", Old: file}}, []*core.Change{{Path: ".gitignore", Old: file}}},
		{"nested rule file created", []*core.Change{{Path: "pkg/sub/.gitignore", New: file}}, []*core.Change{{Path: "pkg/sub/.gitignore", New: file}}},
		{"nested rule file removed", []*core.Change{{Path: "pkg/sub/.gitignore", Old: file}}, []*core.Change{{Path: "pkg/sub/.gitignore", Old: file}}},
		{"exclude file created", []*core.Change{{Path: ".git/info/exclude", New: file}}, []*core.Change{{Path: ".git/info/exclude", New: file}}},
		{"symbolic link replaced by rule file", []*core.Change{{Path: ".gitignore", Old: link, New: file}}, []*core.Change{{Path: ".gitignore", Old: link, New: file}}},
		{"directory replaced by rule file", []*core.Change{{Path: ".gitignore", Old: withoutRules, New: file}}, []*core.Change{{Path: ".gitignore", Old: withoutRules, New: file}}},

		// Changes at rule paths that don't involve regular files.
		{"rule file symbolic link created", []*core.Change{{Path: ".gitignore", New: link}}, nil},
		{"rule file symbolic link removed", []*core.Change{{Path: ".gitignore", Old: link}}, nil},
		{"rule file replaced by symbolic link", []*core.Change{{Path: ".gitignore", Old: file, New: link}}, nil},
		{"rule file replaced by directory", []*core.Change{{Path: ".gitignore", Old: file, New: withoutRules}}, nil},
		{"directory named as rule file removed", []*core.Change{{Path: ".gitignore", Old: withoutRules}}, nil},

		// Creation of directories, which are pruned.
		{"directory without rule files", []*core.Change{{Path: "d", New: withoutRules}}, nil},
		{"empty directory", []*core.Change{{Path: "d", New: directory(nil)}}, nil},
		{"directory with rule file", []*core.Change{{Path: "d", New: withRules}}, []*core.Change{{Path: "d", New: onlyRules}}},
		{"directory that only has rule file", []*core.Change{{Path: "d", New: onlyRules}}, []*core.Change{{Path: "d", New: onlyRules}}},
		{"root with rule file", []*core.Change{{New: withRules}}, []*core.Change{{New: onlyRules}}},
		{"root without rule files", []*core.Change{{New: withoutRules}}, nil},
		{"file root", []*core.Change{{New: file}}, nil},
		{
			"nested contents",
			[]*core.Change{{
				Path: "d/e",
				New: directory(map[string]*core.Entry{
					".gitignore": file,
					"a":          file,
					"empty":      directory(nil),
					"no-rules":   withoutRules,
					"rules":      withRules,
					"deep": directory(map[string]*core.Entry{
						"a":    file,
						"deep": withRules,
					}),
				}),
			}},
			[]*core.Change{{
				Path: "d/e",
				New: directory(map[string]*core.Entry{
					".gitignore": file,
					"rules":      onlyRules,
					"deep":       directory(map[string]*core.Entry{"deep": onlyRules}),
				}),
			}},
		},
		{"directory with rule file symbolic link", []*core.Change{{Path: "d", New: directory(map[string]*core.Entry{".gitignore": link})}}, nil},
		{"directory with directory named as rule file", []*core.Change{{Path: "d", New: directory(map[string]*core.Entry{".gitignore": withoutRules})}}, nil},
		{
			"directory named as rule file containing a rule file",
			[]*core.Change{{Path: ".gitignore", New: directory(map[string]*core.Entry{".gitignore": file, "a": file})}},
			[]*core.Change{{Path: ".gitignore", New: onlyRules}},
		},
		{"root with exclude file", []*core.Change{{New: exclude}}, []*core.Change{{New: excludeOnly}}},
		{"directory with name of exclude file beneath it", []*core.Change{{Path: "d", New: exclude}}, nil},
		{"directory with exclude file parents", []*core.Change{{Path: ".git", New: directory(map[string]*core.Entry{"info": withoutRules})}}, nil},
		{
			"created directory containing exclude file",
			[]*core.Change{{Path: ".git/info", New: directory(map[string]*core.Entry{"exclude": file, "other": file})}},
			[]*core.Change{{Path: ".git/info", New: directory(map[string]*core.Entry{"exclude": file})}},
		},

		// Other changes involving directories with rule files.
		{"directory with rule file deleted", []*core.Change{{Path: "d", Old: withRules}}, nil},
		{"file replaced by directory with rule file", []*core.Change{{Path: "d", Old: file, New: withRules}}, nil},
		{"symbolic link replaced by directory with rule file", []*core.Change{{Path: "d", Old: link, New: withRules}}, nil},
		{"directory replaced by directory with rule file", []*core.Change{{Path: "d", Old: withoutRules, New: withRules}}, nil},
		{"root replaced by directory with rule file", []*core.Change{{Old: file, New: withRules}}, nil},

		// Mixtures, which retain their order.
		{
			"mixture",
			[]*core.Change{
				{Path: "a", New: file},
				{Path: "b/.gitignore", New: file},
				{Path: "c", Old: file},
				{Path: "d", New: withRules},
				{Path: "e", Old: withRules},
				{Path: "f/.gitignore", Old: file},
				{Path: "g", New: withoutRules},
			},
			[]*core.Change{
				{Path: "b/.gitignore", New: file},
				{Path: "d", New: onlyRules},
				{Path: "f/.gitignore", Old: file},
			},
		},
	}

	// Process test cases.
	for _, test := range tests {
		result := ignoreRuleDeliveries(test.changes)
		if len(result) != len(test.expected) {
			t.Errorf("%s: incorrect delivery count: %d != %d", test.description, len(result), len(test.expected))
			continue
		}
		for r, delivery := range result {
			expected := test.expected[r]
			if delivery.Path != expected.Path {
				t.Errorf("%s: incorrect path: %q != %q", test.description, delivery.Path, expected.Path)
			}
			if !delivery.Old.Equal(expected.Old, true) {
				t.Errorf("%s: incorrect old entry", test.description)
			}
			if !delivery.New.Equal(expected.New, true) {
				t.Errorf("%s: incorrect new entry", test.description)
			}
			if err := delivery.EnsureValid(true); err != nil {
				t.Errorf("%s: invalid delivery: %v", test.description, err)
			}
		}
	}
}

// TestIgnoreRuleDeliveriesImmutability tests that ignoreRuleDeliveries doesn't
// modify the entries that it's given, which are shared with the snapshots that
// they come from, and that the entries it creates aren't shared with them.
func TestIgnoreRuleDeliveriesImmutability(t *testing.T) {
	// Define an entry and record what it looks like.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	sub := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{".gitignore": file, "a": file}}
	entry := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"sub": sub, "b": file}}
	original := entry.Copy(core.EntryCopyBehaviorDeep)

	// Compute the delivery and verify that the original is unmodified.
	deliveries := ignoreRuleDeliveries([]*core.Change{{Path: "d", New: entry}})
	if len(deliveries) != 1 {
		t.Fatal("incorrect delivery count:", len(deliveries))
	}
	if !entry.Equal(original, true) {
		t.Error("original entry modified")
	}

	// Verify that directories aren't shared, by identity or content map.
	pruned := deliveries[0].New
	if pruned == entry || pruned.Contents["sub"] == sub {
		t.Error("original directories shared")
	}
	pruned.Contents["sub"].Contents["new"] = file
	pruned.Contents["new"] = file
	if !entry.Equal(original, true) {
		t.Error("original directory contents shared")
	}
}

// TestRuleOnlyTransitions tests ruleOnlyTransitions.
func TestRuleOnlyTransitions(t *testing.T) {
	// Define changes.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	directory := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{".gitignore": file, "a": file}}
	rule := &core.Change{Path: ".gitignore", New: file}
	removal := &core.Change{Path: "sub/.gitignore", Old: file}
	created := &core.Change{Path: "created", New: directory}
	deleted := &core.Change{Path: "deleted", Old: directory}
	plain := &core.Change{Path: "plain", New: file}
	otherPlain := &core.Change{Path: "other", Old: file}

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// alpha are the transitions for alpha.
		alpha []*core.Change
		// beta are the transitions for beta.
		beta []*core.Change
		// expectedAlpha are the paths of the expected transitions for alpha.
		expectedAlpha []string
		// expectedBeta are the paths of the expected transitions for beta.
		expectedBeta []string
		// expectedRuleOnly is whether or not the cycle should be rule-only.
		expectedRuleOnly bool
	}{
		{"no transitions", nil, nil, nil, nil, false},
		{"no deliveries", []*core.Change{plain}, []*core.Change{otherPlain, deleted}, []string{"plain"}, []string{"other", "deleted"}, false},
		{"beta receives rules", []*core.Change{plain}, []*core.Change{otherPlain, rule}, nil, []string{".gitignore"}, true},
		{"alpha receives rules", []*core.Change{plain, rule}, []*core.Change{otherPlain}, []string{".gitignore"}, nil, true},
		{"beta loses rules", []*core.Change{plain}, []*core.Change{otherPlain, removal}, nil, []string{"sub/.gitignore"}, true},
		{"alpha receives directory", []*core.Change{created, plain}, []*core.Change{otherPlain}, []string{"created"}, nil, true},
		{"both receive rules", []*core.Change{plain, rule}, []*core.Change{created, otherPlain}, []string{".gitignore"}, []string{"created"}, true},
		{"nothing else to drop on alpha", []*core.Change{rule}, nil, []string{".gitignore"}, nil, true},
		{"nothing else to drop on beta", nil, []*core.Change{removal}, nil, []string{"sub/.gitignore"}, true},
		{"directory deletion with rules doesn't count", []*core.Change{deleted}, []*core.Change{plain}, []string{"deleted"}, []string{"plain"}, false},
		{"directory deletion waits for rules", []*core.Change{deleted, rule}, nil, []string{".gitignore"}, nil, true},
	}

	// Process test cases.
	for _, test := range tests {
		alpha, beta, ruleOnly := ruleOnlyTransitions(test.alpha, test.beta)
		if ruleOnly != test.expectedRuleOnly {
			t.Errorf("%s: incorrect rule-only result: %t != %t", test.description, ruleOnly, test.expectedRuleOnly)
		}
		paths := func(changes []*core.Change) (result []string) {
			for _, change := range changes {
				result = append(result, change.Path)
			}
			return
		}
		if result := paths(alpha); !slices.Equal(result, test.expectedAlpha) {
			t.Errorf("%s: incorrect alpha transitions: %v != %v", test.description, result, test.expectedAlpha)
		}
		if result := paths(beta); !slices.Equal(result, test.expectedBeta) {
			t.Errorf("%s: incorrect beta transitions: %v != %v", test.description, result, test.expectedBeta)
		}
	}
}

// TestRuleOnlyCycleRemainder tests that pruned deliveries work with the parts
// of the synchronization cycle that follow them, including staging, ancestor
// updates and validation, and that the next cycle reconciles the remainder of
// what was pruned. Transitions are modeled by their expected results, as they
// are in the core package's reconciliation tests. Real transitions are covered
// by integration tests.
func TestRuleOnlyCycleRemainder(t *testing.T) {
	// Define entries.
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	directory := func(contents map[string]*core.Entry) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: contents}
	}
	content := directory(map[string]*core.Entry{
		".gitignore": file,
		"a":          file,
		"plain":      directory(map[string]*core.Entry{"z": file}),
		"sub": directory(map[string]*core.Entry{
			".gitignore": file,
			"x":          file,
			"deep":       directory(map[string]*core.Entry{".gitignore": file, "y": file}),
		}),
	})
	mode := core.SynchronizationMode_SynchronizationModeTwoWaySafe

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// ancestor is the initial ancestor, which is also the initial beta.
		ancestor *core.Entry
		// expectedStaged are the paths that the first cycle should stage.
		expectedStaged []string
		// expectedTransitioned are the paths of the second cycle's transitions.
		expectedTransitioned []string
	}{
		{
			"empty root",
			directory(nil),
			[]string{".gitignore", "sub/.gitignore", "sub/deep/.gitignore"},
			[]string{"a", "plain", "sub/deep/y", "sub/x"},
		},
		{
			"missing root",
			nil,
			[]string{".gitignore", "sub/.gitignore", "sub/deep/.gitignore"},
			[]string{"a", "plain", "sub/deep/y", "sub/x"},
		},
		{
			"existing directory",
			directory(map[string]*core.Entry{"sub": directory(map[string]*core.Entry{"x": file})}),
			[]string{".gitignore", "sub/.gitignore", "sub/deep/.gitignore"},
			[]string{"a", "plain", "sub/deep/y"},
		},
	}

	// Process test cases.
	for _, test := range tests {
		ancestor, beta := test.ancestor, test.ancestor

		// apply models a transition, including its propagation to the ancestor.
		apply := func(ancestorChanges, transitions []*core.Change) {
			t.Helper()
			var err error
			if beta, err = core.Apply(beta, transitions); err != nil {
				t.Fatalf("%s: unable to apply transitions: %v", test.description, err)
			}
			if ancestor, err = core.Apply(ancestor, append(ancestorChanges, transitions...)); err != nil {
				t.Fatalf("%s: unable to apply changes to ancestor: %v", test.description, err)
			}
			if err = ancestor.EnsureValid(true); err != nil {
				t.Fatalf("%s: invalid ancestor: %v", test.description, err)
			}
		}

		// The first cycle delivers only rule files.
		ancestorChanges, αTransitions, βTransitions, conflicts := core.ReconcileGitMode(ancestor, content, beta, mode)
		if len(conflicts) != 0 || len(αTransitions) != 0 {
			t.Fatalf("%s: unexpected first reconciliation: %v, %v", test.description, conflicts, αTransitions)
		}
		αTransitions, βTransitions, ruleOnly := ruleOnlyTransitions(αTransitions, βTransitions)
		if !ruleOnly || len(αTransitions) != 0 {
			t.Fatalf("%s: first cycle isn't rule-only: %t, %v", test.description, ruleOnly, αTransitions)
		}
		staged, _ := core.TransitionDependencies(βTransitions)
		slices.Sort(staged)
		if !slices.Equal(staged, test.expectedStaged) {
			t.Errorf("%s: incorrect staged paths: %v != %v", test.description, staged, test.expectedStaged)
		}
		apply(ancestorChanges, βTransitions)

		// The second cycle delivers the rest.
		ancestorChanges, αTransitions, βTransitions, conflicts = core.ReconcileGitMode(ancestor, content, beta, mode)
		if len(conflicts) != 0 || len(αTransitions) != 0 {
			t.Fatalf("%s: unexpected second reconciliation: %v, %v", test.description, conflicts, αTransitions)
		}
		αTransitions, βTransitions, ruleOnly = ruleOnlyTransitions(αTransitions, βTransitions)
		if ruleOnly {
			t.Errorf("%s: second cycle is rule-only", test.description)
		}
		var transitioned []string
		for _, transition := range βTransitions {
			transitioned = append(transitioned, transition.Path)
		}
		slices.Sort(transitioned)
		if !slices.Equal(transitioned, test.expectedTransitioned) {
			t.Errorf("%s: incorrect transitions: %v != %v", test.description, transitioned, test.expectedTransitioned)
		}
		apply(ancestorChanges, βTransitions)

		// Everything has then settled.
		if !beta.Equal(content, true) {
			t.Errorf("%s: beta doesn't match alpha", test.description)
		}
		if !ancestor.Equal(content, true) {
			t.Errorf("%s: ancestor doesn't match alpha", test.description)
		}
		ancestorChanges, αTransitions, βTransitions, conflicts = core.ReconcileGitMode(ancestor, content, beta, mode)
		if len(ancestorChanges) != 0 || len(αTransitions) != 0 || len(βTransitions) != 0 || len(conflicts) != 0 {
			t.Errorf("%s: unsettled after second cycle", test.description)
		}
	}
}

// TestFilteredPathsAreSubset tests that filteredPathsAreSubset returns a
// correct assessment for a variety of test cases.
func TestFilteredPathsAreSubset(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		filteredPaths []string
		originalPaths []string
		expected      bool
	}{
		{nil, nil, true},
		{nil, []string{}, true},
		{[]string{}, []string{}, true},
		{[]string{}, nil, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a"}, []string{"a", "b"}, true},
		{[]string{"b"}, []string{"a", "b"}, true},
		{[]string{"c"}, nil, false},
		{[]string{"c"}, []string{}, false},
		{[]string{"c"}, []string{"a"}, false},
		{[]string{"c"}, []string{"a", "b"}, false},
		{[]string{"a", "b"}, []string{"a", "b"}, true},
		{[]string{"b", "a"}, []string{"a", "b"}, false},
	}

	// Run test cases.
	for c, testCase := range testCases {
		if result := filteredPathsAreSubset(
			testCase.filteredPaths,
			testCase.originalPaths,
		); result != testCase.expected {
			t.Errorf(
				"result did not match expected for test case %d: %t != %t",
				c,
				result,
				testCase.expected,
			)
		}
	}
}
