package synchronization

import (
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
