package core

import (
	"testing"
)

// TestProblemEnsureValid tests Problem.EnsureValid.
func TestProblemEnsureValid(t *testing.T) {
	// Define test cases.
	tests := []struct {
		problem  *Problem
		expected bool
	}{
		{nil, false},
		{&Problem{}, false},
		{&Problem{Path: "/some/path"}, false},
		{&Problem{Error: "some root error"}, true},
		{&Problem{Path: "/some/path", Error: "some path error"}, true},
		{&Problem{Path: "/some/path", Warning: true}, false},
		{&Problem{Path: "/some/path", Error: "some path warning", Warning: true}, true},
	}

	// Process test cases.
	for i, test := range tests {
		if err := test.problem.EnsureValid(); err == nil && !test.expected {
			t.Errorf("test index %d: problem unexpectedly classified as valid", i)
		} else if err != nil && test.expected {
			t.Errorf("test index %d: problem unexpectedly classified as invalid: %v", i, err)
		}
	}
}

// TestCopyProblemsPreservesWarnings tests that CopyProblems and SortProblems
// preserve the distinction between warnings and other problems.
func TestCopyProblemsPreservesWarnings(t *testing.T) {
	copied := CopyProblems([]*Problem{{Path: "b", Error: "error", Warning: true}, {Path: "a", Error: "error"}})
	SortProblems(copied)
	if copied[0].Path != "a" || copied[0].Warning {
		t.Error("problem incorrectly indicated as warning")
	}
	if copied[1].Path != "b" || !copied[1].Warning {
		t.Error("warning not preserved")
	}
}

// TODO: Implement TestCopyProblems.

// TODO: Implement TestSortProblems.
