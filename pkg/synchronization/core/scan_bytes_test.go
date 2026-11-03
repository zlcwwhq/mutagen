package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// testingByteIgnorer is an ignorer whose rules match bytes, so that it can
// evaluate names that aren't valid UTF-8 (see ignore.NonUTF8Ignorer), if it
// says so. It ignores paths ending in ".log" and directories whose names end in
// a 0xff byte, and it records the paths that it evaluates.
type testingByteIgnorer struct {
	// evaluatesNonUTF8 is the result of EvaluatesNonUTF8.
	evaluatesNonUTF8 bool
	// evaluated are the paths that have been evaluated.
	evaluated []string
}

// Ignore implements ignore.Ignorer.Ignore.
func (i *testingByteIgnorer) Ignore(path string, directory bool) (ignore.IgnoreStatus, bool) {
	i.evaluated = append(i.evaluated, path)
	if strings.HasSuffix(path, ".log") || (directory && strings.HasSuffix(path, "\xff")) {
		return ignore.IgnoreStatusIgnored, false
	}
	return ignore.IgnoreStatusNominal, false
}

// EvaluatesNonUTF8 implements ignore.NonUTF8Ignorer.EvaluatesNonUTF8.
func (i *testingByteIgnorer) EvaluatesNonUTF8() bool {
	return i.evaluatesNonUTF8
}

// TestScanNonUTF8NamesWithByteRules tests that content whose name isn't valid
// UTF-8 is untracked if an ignorer that can evaluate such names ignores it, and
// problematic otherwise, as it is if the ignorer can't evaluate such names, in
// which case it's never asked about them.
func TestScanNonUTF8NamesWithByteRules(t *testing.T) {
	// Create the content, skipping the test if the filesystem doesn't store the
	// names as they're given.
	root := t.TempDir()
	files := []string{"caf\xe9.log", "caf\xe9.txt", "f\xff", "d\xff/file", "valid.log"}
	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Skip("unable to create directory with non-UTF-8 name:", err)
		} else if err := os.WriteFile(path, []byte(tF1Content), 0600); err != nil {
			t.Skip("unable to create file with non-UTF-8 name:", err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "caf\xe9.txt")); err != nil {
		t.Skip("filesystem doesn't preserve non-UTF-8 names:", err)
	} else if names, err := filepath.Glob(filepath.Join(root, "caf*")); err != nil || len(names) != 2 ||
		!strings.HasSuffix(names[0], "caf\xe9.log") {
		t.Skip("filesystem doesn't preserve non-UTF-8 names")
	}

	// Define the expected results.
	tests := []struct {
		// evaluatesNonUTF8 is whether or not the ignorer can evaluate names that
		// aren't valid UTF-8.
		evaluatesNonUTF8 bool
		// expected is the expected content.
		expected map[string]*Entry
	}{
		{true, map[string]*Entry{
			"caf�.log (non-UTF-8)": tU,
			"caf�.txt (non-UTF-8)": tPInvalidUTF8(`"caf\xe9.txt"`),
			"f� (non-UTF-8)":       tPInvalidUTF8(`"f\xff"`),
			"d� (non-UTF-8)":       tU,
			"valid.log":            tU,
		}},
		{false, map[string]*Entry{
			"caf�.log (non-UTF-8)": tPInvalidUTF8(`"caf\xe9.log"`),
			"caf�.txt (non-UTF-8)": tPInvalidUTF8(`"caf\xe9.txt"`),
			"f� (non-UTF-8)":       tPInvalidUTF8(`"f\xff"`),
			"d� (non-UTF-8)":       tPInvalidUTF8(`"d\xff"`),
			"valid.log":            tU,
		}},
	}

	// Process test cases.
	for _, test := range tests {
		ignorer := &testingByteIgnorer{evaluatesNonUTF8: test.evaluatesNonUTF8}
		snapshot, _, _, err := Scan(
			context.Background(),
			root,
			nil, nil,
			newTestingHasher(), nil,
			ignorer, nil,
			behavior.ProbeMode_ProbeModeProbe,
			SymbolicLinkMode_SymbolicLinkModePortable,
			PermissionsMode_PermissionsModePortable,
		)
		if err != nil {
			t.Fatal("unable to scan:", err)
		}
		if expected := (&Entry{Contents: test.expected}); !snapshot.Content.Equal(expected, true) {
			t.Errorf("evaluation of non-UTF-8 names %v: incorrect content: %v != %v", test.evaluatesNonUTF8, snapshot.Content, expected)
		}
		var evaluatedNonUTF8 []string
		for _, path := range ignorer.evaluated {
			if path != strings.ToValidUTF8(path, "") {
				evaluatedNonUTF8 = append(evaluatedNonUTF8, path)
			}
		}
		if test.evaluatesNonUTF8 && len(evaluatedNonUTF8) != 4 {
			t.Errorf("incorrect non-UTF-8 paths evaluated: %q", evaluatedNonUTF8)
		} else if !test.evaluatesNonUTF8 && len(evaluatedNonUTF8) != 0 {
			t.Errorf("non-UTF-8 paths evaluated by ignorer that can't evaluate them: %q", evaluatedNonUTF8)
		}
	}
}
