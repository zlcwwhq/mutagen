package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

func TestSymbolicLinkEmptyTargetInvalid(t *testing.T) {
	if _, err := normalizeSymbolicLinkAndEnsurePortable("file", ""); err == nil {
		t.Fatal("symbolic link with empty target treated as portable")
	}
}

const testLongSymbolicLinkTarget = `dlaksjdflkajsfdlkajsdlfkjlkjlkajslfdkjlaksdj
dlaksjdflkajsfdlkajsdlfkjlkjlkajslfdkjlaksdjflkajasldkfakrjkjasdkfhajsfdhjasdhfa
dlaksjdflkajsfdlkajsdlfkjlkjlkajslfdkjlaksdjflkajasldkfakrjkjasdkfhajsfdhjasdhfa
dlaksjdflkajsfdlkajsdlfkjlkjlkajslfdkjlaksdjflkajasldkfakrjkjasdkfhajsfdhjasdhfa
dlaksjdflkajsfdlkajsdlfkjlkjlkajslfdkjlaksdjflkajasldkfakrjkjasdkfhajsfdhjasdhfa
dlaksjdflkajsfdlkajsdlfkjlkjlkajslfdkjlaksdjflkajasldkfakrjkjasdkfhajsfdhjasdhf`

func TestSymbolicLinkTooLongInvalid(t *testing.T) {
	if _, err := normalizeSymbolicLinkAndEnsurePortable("file", testLongSymbolicLinkTarget); err == nil {
		t.Fatal("symbolic link with overly long target treated as portable")
	}
}

func TestSymbolicLinkWithColonInvalid(t *testing.T) {
	if _, err := normalizeSymbolicLinkAndEnsurePortable("file", "target:path"); err == nil {
		t.Fatal("symbolic link with colon in target treated as portable")
	}
}

func TestSymbolicLinkAbsoluteInvalid(t *testing.T) {
	if _, err := normalizeSymbolicLinkAndEnsurePortable("file", "/target"); err == nil {
		t.Fatal("symbolic link with absolute target treated as portable")
	}
}

func TestSymbolicLinkEscapesInvalid(t *testing.T) {
	if _, err := normalizeSymbolicLinkAndEnsurePortable("file", "../target"); err == nil {
		t.Fatal("symbolic link that escapes root treated as portable")
	}
}

func TestSymbolicLinkEscapesDeeperInvalid(t *testing.T) {
	if _, err := normalizeSymbolicLinkAndEnsurePortable("directory/symlink path", "../../target"); err == nil {
		t.Fatal("symbolic link that escapes root treated as portable")
	}
}

func TestSymbolicLinkSameDirectoryValid(t *testing.T) {
	if target, err := normalizeSymbolicLinkAndEnsurePortable("file", "other"); err != nil {
		t.Fatal("portable symbolic link treated as invalid:", err)
	} else if target != "other" {
		t.Error("normalized symbolic link target incorrect:", target, "!=", "other")
	}
}

func TestSymbolicLinkDotSameDirectoryValid(t *testing.T) {
	if target, err := normalizeSymbolicLinkAndEnsurePortable("file", "./other"); err != nil {
		t.Fatal("portable symbolic link treated as invalid:", err)
	} else if target != "./other" {
		t.Error("normalized symbolic link target incorrect:", target, "!=", "./other")
	}
}

func TestSymbolicLinkDotSubdirectoryValid(t *testing.T) {
	if target, err := normalizeSymbolicLinkAndEnsurePortable("file", "subdirectory/other"); err != nil {
		t.Fatal("portable symbolic link treated as invalid:", err)
	} else if target != "subdirectory/other" {
		t.Error("normalized symbolic link target incorrect:", target, "!=", "subdirectory/other")
	}
}

func TestSymbolicLinkEscapesThroughEmptyComponentsInvalid(t *testing.T) {
	// Define test cases. Each target escapes the synchronization root when
	// resolved, but would appear to stay within it if the empty components
	// produced by repeated slashes were counted as names.
	tests := []struct {
		path   string
		target string
	}{
		{"link", "child//../../outside"},
		{"link", "child///../../outside"},
		{"link", "a//b//../../../outside"},
		{"link", "child/..//../"},
		{"directory/link", "child//../../../outside"},
		{"directory/subdirectory/link", "a////../../../../outside"},
	}

	// Process test cases.
	for _, test := range tests {
		if _, err := normalizeSymbolicLinkAndEnsurePortable(test.path, test.target); err == nil {
			t.Errorf("symbolic link at %q with target %q that escapes root treated as portable",
				test.path, test.target,
			)
		}
	}
}

func TestSymbolicLinkEmptyComponentsValid(t *testing.T) {
	// Define test cases. Each target contains empty or "." components but stays
	// within the synchronization root, so it must be returned unchanged (the
	// transitioner requires that normalization doesn't alter targets that it
	// creates).
	tests := []struct {
		path   string
		target string
	}{
		{"link", "child//../inside"},
		{"link", "child/"},
		{"directory/link", "../sibling/"},
		{"directory/link", "..//./child"},
		{"link", ".//./child//./"},
	}

	// Process test cases.
	for _, test := range tests {
		if target, err := normalizeSymbolicLinkAndEnsurePortable(test.path, test.target); err != nil {
			t.Errorf("portable symbolic link at %q with target %q treated as invalid: %v",
				test.path, test.target, err,
			)
		} else if target != test.target {
			t.Errorf("normalized symbolic link target incorrect: %q != %q", target, test.target)
		}
	}
}

// TestScanPortableSymbolicLinkEmptyComponents tests that a scan in portable
// symbolic link mode reports an on-disk symbolic link whose target escapes the
// synchronization root through empty components as problematic, while
// recording a symbolic link whose target contains empty components but stays
// within the root with its target unchanged.
func TestScanPortableSymbolicLinkEmptyComponents(t *testing.T) {
	// Create a synchronization root containing both symbolic links.
	root := t.TempDir()
	if err := os.Symlink("child//../../outside", filepath.Join(root, "escape")); err != nil {
		t.Fatal("unable to create escaping symbolic link:", err)
	}
	if err := os.Symlink("child//../inside", filepath.Join(root, "inside")); err != nil {
		t.Fatal("unable to create non-escaping symbolic link:", err)
	}

	// Create an ignorer that doesn't ignore anything.
	ignorer, err := mutagenignore.NewIgnorer(nil)
	if err != nil {
		t.Fatal("unable to create ignorer:", err)
	}

	// Perform a scan in portable symbolic link mode.
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
		t.Fatal("unable to perform scan:", err)
	}

	// Verify that only the escaping symbolic link was marked as problematic.
	expected := &Entry{Contents: map[string]*Entry{
		"escape": {Kind: EntryKind_Problematic, Problem: "*"},
		"inside": {Kind: EntryKind_SymbolicLink, Target: "child//../inside"},
	}}
	if !snapshot.Content.Equal(expected, true) {
		t.Errorf("scan result not equal to expected: %v != %v", snapshot.Content, expected)
	}
	if snapshot.SymbolicLinks != 1 {
		t.Errorf("scan symbolic link count not equal to expected: %d != 1", snapshot.SymbolicLinks)
	}
}
