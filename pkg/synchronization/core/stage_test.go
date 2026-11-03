package core

import (
	"bytes"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/comparison"
)

// TestTransitionDependencies tests TransitionDependencies.
func TestTransitionDependencies(t *testing.T) {
	// Define test cases.
	tests := []struct {
		transitions     []*Change
		expectedPaths   []string
		expectedDigests [][]byte
	}{
		{nil, nil, nil},
		{[]*Change{{New: nil}}, nil, nil},
		{[]*Change{{New: tD0}}, nil, nil},
		{[]*Change{{New: tSA}}, nil, nil},
		{[]*Change{{New: tU}}, nil, nil},
		{[]*Change{{New: tP1}}, nil, nil},
		{[]*Change{{New: tF1}}, []string{""}, [][]byte{tF1.Digest}},
		{[]*Change{{New: tD1}}, []string{"file"}, [][]byte{tF1.Digest}},
		{[]*Change{{Old: tF3, New: tF3E}}, nil, nil},
		{[]*Change{{Old: tF3E, New: tF3}}, nil, nil},
	}

	// Process test cases.
	for i, test := range tests {
		// Compute transition dependencies and perform basic validation.
		paths, digests := TransitionDependencies(test.transitions)
		if len(paths) != len(digests) {
			t.Errorf("test index %d: path and digest counts differ: %d != %d",
				i, len(paths), len(digests),
			)
		}

		// Validate that paths match expected.
		if !comparison.StringSlicesEqual(paths, test.expectedPaths) {
			t.Errorf("test index %d: paths do not match expected", i)
		}

		// Validate that digest match expected.
		if len(digests) != len(test.expectedDigests) {
			t.Errorf("test index %d: digests do not match expected", i)
		} else {
			for d, digest := range digests {
				if !bytes.Equal(digest, test.expectedDigests[d]) {
					t.Errorf("test index %d: digests do not match expected", i)
					break
				}
			}
		}
	}
}

// TestTransitionDependenciesSubmodules tests that TransitionDependencies doesn't
// request the contents of submodules that transitions populate, since they come
// from clones, while it still requests the files that transitions write within
// existing submodules.
func TestTransitionDependenciesSubmodules(t *testing.T) {
	// Define test cases.
	submodule := testingSubmodule(tGL0, map[string]*Entry{"file": tF1, "directory": tD2})
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// transition is the transition.
		transition *Change
		// expectedPaths are the expected paths, in the order that they're
		// expected.
		expectedPaths []string
		// expectedDigests are the expected digests.
		expectedDigests [][]byte
	}{
		{
			description: "creation of submodule",
			transition:  &Change{Path: "sub", New: submodule},
		},
		{
			description: "population of empty directory",
			transition:  &Change{Path: "sub", Old: tD0, New: submodule},
		},
		{
			description: "replacement of file with submodule",
			transition:  &Change{Path: "sub", Old: tF2, New: submodule},
		},
		{
			description:     "creation of directory containing submodule",
			transition:      &Change{Path: "parent", New: &Entry{Contents: map[string]*Entry{"sub": submodule, "other": tF3}}},
			expectedPaths:   []string{"parent/other"},
			expectedDigests: [][]byte{tF3.Digest},
		},
		{
			description: "creation of root containing nested submodule",
			transition: &Change{New: &Entry{Contents: map[string]*Entry{
				"parent": nested("sub", submodule),
			}}},
		},
		{
			description:     "file within existing submodule",
			transition:      &Change{Path: "sub/file", Old: tF1, New: tF2},
			expectedPaths:   []string{"sub/file"},
			expectedDigests: [][]byte{tF2.Digest},
		},
		{
			description:     "directory within existing submodule",
			transition:      &Change{Path: "sub/directory", New: tD2},
			expectedPaths:   []string{"sub/directory/file"},
			expectedDigests: [][]byte{tF2.Digest},
		},
		{
			description: "commit of existing submodule",
			transition:  &Change{Path: "sub/.git", Old: tGL0, New: tGL1},
		},
		{
			description: "attachment of Git entry",
			transition:  &Change{Path: "sub/.git", New: tGL0},
		},
	}

	// Process test cases.
	for _, test := range tests {
		paths, digests := TransitionDependencies([]*Change{test.transition})
		if !comparison.StringSlicesEqual(paths, test.expectedPaths) {
			t.Errorf("%s: incorrect paths: %v != %v", test.description, paths, test.expectedPaths)
		}
		if len(digests) != len(test.expectedDigests) {
			t.Errorf("%s: incorrect number of digests: %d != %d", test.description, len(digests), len(test.expectedDigests))
		} else {
			for d, digest := range digests {
				if !bytes.Equal(digest, test.expectedDigests[d]) {
					t.Errorf("%s: incorrect digest at index %d", test.description, d)
				}
			}
		}
	}
}
