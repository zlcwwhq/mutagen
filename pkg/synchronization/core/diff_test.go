package core

import (
	"testing"
)

// TestDiff tests Diff.
func TestDiff(t *testing.T) {
	// Define test cases.
	tests := []struct {
		path     string
		base     *Entry
		target   *Entry
		expected []*Change
	}{
		{"", tN, tN, nil},
		{"", tN, tF1, []*Change{{New: tF1}}},
		{"", tD0, tD1, []*Change{{Path: "file", New: tF1}}},
		{"", tD1, tD0, []*Change{{Path: "file", Old: tF1}}},
		{"sub", tN, tF1, []*Change{{Path: "sub", New: tF1}}},
		{"sub", tD0, tD1, []*Change{{Path: "sub/file", New: tF1}}},
		{"sub", tD1, tD0, []*Change{{Path: "sub/file", Old: tF1}}},
		{"", tG1, tG1U, nil},
		{"", tG1, tG2, []*Change{{Old: tG1, New: tG2}}},
		{"", tG1, tD0, []*Change{{Old: tG1, New: tD0}}},
		{"", tD0, tDGO, []*Change{{Path: "submodule", New: tG1}}},
	}

	// Process test cases.
	for i, test := range tests {
		if delta := diff(test.path, test.base, test.target); !testingChangeListsEqual(delta, test.expected) {
			t.Errorf("test index %d: diff result does not match expected", i)
		}
	}
}
