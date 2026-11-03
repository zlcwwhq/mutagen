package synchronization

import (
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestExportProblemsPreservesWarnings tests that exported problems remain
// identifiable as warnings.
func TestExportProblemsPreservesWarnings(t *testing.T) {
	exported := exportProblems([]*core.Problem{
		{Path: "first", Error: "error"},
		{Path: "second", Error: "warning", Warning: true},
	})
	if len(exported) != 2 {
		t.Fatal("incorrect exported problem count:", len(exported))
	}
	if exported[0] != (Problem{Path: "first", Error: "error"}) {
		t.Errorf("incorrect exported problem: %+v", exported[0])
	}
	if exported[1] != (Problem{Path: "second", Error: "warning", Warning: true}) {
		t.Errorf("incorrect exported warning: %+v", exported[1])
	}
}

// TODO: Implement additional tests.
