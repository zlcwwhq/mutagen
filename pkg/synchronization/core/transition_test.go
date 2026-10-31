package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

// testingDecomposeEntryIntoCreationChanges generates a list of creation changes
// from a single entry for the purposes of stress-testing Transition.
func testingDecomposeEntryIntoCreationChanges(entry *Entry) (changes []*Change) {
	entry.walk("", func(p string, e *Entry) {
		if e == nil {
			return
		}
		changes = append(changes, &Change{Path: p, New: e.Copy(EntryCopyBehaviorSlim)})
	}, false)
	return
}

// testingDecomposeEntryIntoRemovalChanges generates a list of removal changes
// from a single entry for the purposes of stress-testing Transition.
func testingDecomposeEntryIntoRemovalChanges(entry *Entry) (changes []*Change) {
	entry.walk("", func(p string, e *Entry) {
		if e == nil {
			return
		}
		changes = append(changes, &Change{Path: p, Old: e.Copy(EntryCopyBehaviorSlim)})
	}, true)
	return
}

// testingEntryWildcard is a special entry value that we use for wildcard
// matches in decomposed tests where we don't have ordering enforced.
var testingEntryWildcard = &Entry{Kind: -1}

// testingNWildcardEntries generates a slice of n testingEntryWildcard entries.
func testingNWildcardEntries(n uint) []*Entry {
	result := make([]*Entry, int(n))
	for i := 0; i < len(result); i++ {
		result[i] = testingEntryWildcard
	}
	return result
}

// TestTransition tests Transition.
func TestTransition(t *testing.T) {
	// Create contexts to use for tests.
	backgroundCtx := context.Background()
	cancelledCtx, cancel := context.WithCancel(backgroundCtx)
	cancel()

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// skip is a callback that can be used to skip a test on a particular
		// filesystem (or based on other runtime criteria).
		skip func(testingFilesystem) bool
		// baseline is the baseline content to create on disk.
		baseline *Entry
		// baselineContentMap is the content map for baseline.
		baselineContentMap testingContentMap
		// tweak is an optional callback that can be used to perform additional
		// tweaks to generated on-disk content. For more information about the
		// role of tweak, see the tweak member in testingContentManager.
		tweak func(string) error
		// retweak is an additional optional callback that can be used to
		// perform additional on-disk content modifications. Unlike tweak, it is
		// run between the time of the baseline scan (used to generate a scan
		// cache) and the transition operation, allowing for content and issues
		// that aren't accounted for in the baseline scan cache.
		retweak func(string) error
		// untweak is an optional callback that can be used to fix content for
		// successful removal by os.RemoveAll. For more information about the
		// role of untweak, see the untweak member in testingContentManager.
		untweak func(string) error
		// ctx is the context in which the operation should be performed.
		ctx context.Context
		// transitions are the transitions to apply
		transitions []*Change
		// transitionsContentMap is the content map necessary to perform the
		// operations specified in transitions.
		transitionsContentMap testingContentMap
		// symbolicLinkMode is the symbolic link mode to use for the test.
		symbolicLinkMode SymbolicLinkMode
		// expectedResults are the expected result entries.
		expectedResults []*Entry
		// expectedProblems are the expected problems. Their order is not
		// important since they'll be compared with testingProblemListsEqual.
		expectedProblems []*Problem
		// expectMissingFiles indicates whether or not files are expected to be
		// missing from the provider.
		expectMissingFiles bool
	}{
		// Test creation.
		{
			"file root creation",
			nil,
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: tF1}},
			tF1ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF1},
			nil,
			false,
		},
		{
			"directory root creation",
			nil,
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: tD1}},
			tD1ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tD1},
			nil,
			false,
		},
		{
			"complex directory root creation",
			func(f testingFilesystem) bool {
				return f.name == "FAT32"
			},
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: tDM}},
			tDMContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tDM},
			nil,
			false,
		},
		{
			"decomposed complex directory root creation",
			func(f testingFilesystem) bool {
				return f.name == "FAT32"
			},
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			testingDecomposeEntryIntoCreationChanges(tDM),
			tDMContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			testingNWildcardEntries(uint(tDM.Count())),
			nil,
			false,
		},

		// Test removal.
		{
			"file root removal",
			nil,
			tF1, tF1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Old: tF1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			nil,
			false,
		},
		{
			"directory root removal",
			nil,
			tD1, tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Old: tD1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			nil,
			false,
		},
		{
			"complex directory root removal",
			func(f testingFilesystem) bool {
				return f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Old: tDM}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			nil,
			false,
		},
		{
			"decomposed complex directory root removal",
			func(f testingFilesystem) bool {
				return f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil, nil, nil,
			backgroundCtx,
			testingDecomposeEntryIntoRemovalChanges(tDM),
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			testingNWildcardEntries(uint(tDM.Count())),
			nil,
			false,
		},

		// Test file swapping.
		{
			"file root swapping",
			nil,
			tF1, tF1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Old: tF1, New: tF2}},
			tF2ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF2},
			nil,
			false,
		},
		{
			"file content swapping",
			nil,
			tD1, tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "file", Old: tF1, New: tF2}},
			tD2ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF2},
			nil,
			false,
		},
		{
			"file swap to executable",
			func(f testingFilesystem) bool {
				return runtime.GOOS == "windows" || f.name == "FAT32"
			},
			tD1, tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "file", Old: tF1, New: executable(tF1)}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{executable(tF1)},
			nil,
			false,
		},
		{
			"file swap to non-executable",
			func(f testingFilesystem) bool {
				return runtime.GOOS == "windows" || f.name == "FAT32"
			},
			nested("file", executable(tF1)), tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "file", Old: executable(tF1), New: tF1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF1},
			nil,
			false,
		},
		{
			"inaccessible directory root",
			func(f testingFilesystem) bool {
				return runtime.GOOS == "windows" || f.name == "FAT32"
			},
			tD1, tD1ContentMap,
			nil,
			func(root string) error {
				return os.Chmod(root, 0300)
			},
			func(root string) error {
				return os.Chmod(root, 0700)
			},
			backgroundCtx,
			[]*Change{{Path: "file", Old: tF1, New: tF2}},
			tD2ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF1},
			[]*Problem{{Path: "file", Error: "*"}},
			false,
		},

		// Test traversal problems.
		{
			"file root traversal",
			nil,
			tF1, tF1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "subpath", New: tF2}},
			tF2ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			[]*Problem{{Path: "subpath", Error: "*"}},
			false,
		},
		{
			"subdirectory inaccessible",
			func(f testingFilesystem) bool {
				return runtime.GOOS == "windows" || f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil,
			func(root string) error {
				return os.Chmod(filepath.Join(root, "populated subdir"), 0300)
			},
			func(root string) error {
				return os.Chmod(filepath.Join(root, "populated subdir"), 0700)
			},
			backgroundCtx,
			[]*Change{{Path: "populated subdir/thing.txt", New: tF2}},
			testingContentMap{"populated subdir/thing.txt": []byte(tF2Content)},
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			[]*Problem{{Path: "populated subdir/thing.txt", Error: "*"}},
			false,
		},
		{
			"casing invalid",
			nil,
			tD1, tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "FILE", Old: tF1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF1},
			[]*Problem{{Path: "FILE", Error: "*"}},
			false,
		},
		{
			"parent casing invalid",
			nil,
			nested("subdir", tD1), testingContentMap{"subdir/file": []byte(tF1Content)},
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "SUBdir/file", Old: tF1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF1},
			[]*Problem{{Path: "SUBdir/file", Error: "*"}},
			false,
		},

		// Test creation problems.
		{
			"invalid root type creation",
			nil,
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: &Entry{Kind: -1}}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			[]*Problem{{Error: "*"}},
			false,
		},
		{
			"invalid content type creation",
			nil,
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: nested("child", &Entry{Kind: -1})}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tD0},
			[]*Problem{{Path: "child", Error: "*"}},
			false,
		},
		{
			"root already exists on file root creation",
			nil,
			tF1, tF1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: tF2}},
			tF2ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			[]*Problem{{Error: "*"}},
			false,
		},
		{
			"root already exists on directory root creation",
			nil,
			tF1, tF1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: tDM}},
			tDMContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			[]*Problem{{Error: "*"}},
			false,
		},
		{
			"symbolic link creation over existing content",
			func(f testingFilesystem) bool {
				return f.name == "FAT32"
			},
			tD1, tD1ContentMap,
			nil,
			func(root string) error {
				return os.Symlink("file", filepath.Join(root, "link"))
			},
			nil,
			backgroundCtx,
			[]*Change{{Path: "link", New: tSR}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			[]*Problem{{Path: "link", Error: "*"}},
			false,
		},

		// Test removal problems.
		{
			"invalid root type removal",
			nil,
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Old: &Entry{Kind: -1}}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{{Kind: -1}},
			[]*Problem{{Error: "*"}},
			false,
		},
		{
			"invalid content type removal",
			nil,
			tD1, tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Old: nested("file", &Entry{Kind: -1})}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nested("file", &Entry{Kind: -1})},
			[]*Problem{{Path: "file", Error: "*"}},
			false,
		},
		{
			"modified root file removal",
			nil,
			tF1, tF1ContentMap,
			nil,
			func(root string) error {
				soon := time.Now().Add(10 * time.Second)
				return os.Chtimes(root, soon, soon)
			},
			nil,
			backgroundCtx,
			[]*Change{{Old: tF1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF1},
			[]*Problem{{Error: "*"}},
			false,
		},
		{
			"assorted directory content removal issues",
			func(f testingFilesystem) bool {
				return runtime.GOOS == "windows" || f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil,
			func(root string) error {
				if err := os.Mkdir(filepath.Join(root, "extra_content"), 0700); err != nil {
					return err
				}
				if err := os.Remove(filepath.Join(root, "file link")); err != nil {
					return err
				}
				if err := os.Symlink("executable file", filepath.Join(root, "file link")); err != nil {
					return err
				}
				soon := time.Now().Add(10 * time.Second)
				if err := os.Chtimes(filepath.Join(root, "executable file"), soon, soon); err != nil {
					return err
				}
				return os.Chmod(filepath.Join(root, "populated subdir"), 0300)
			},
			func(root string) error {
				return os.Chmod(filepath.Join(root, "populated subdir"), 0700)
			},
			backgroundCtx,
			[]*Change{{Old: tDM}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{{
				Contents: map[string]*Entry{
					"executable file":  tF3E,
					"file link":        tSR,
					"populated subdir": tD1,
				},
			}},
			[]*Problem{
				{Path: "extra_content", Error: "*"},
				{Path: "executable file", Error: "*"},
				{Path: "file link", Error: "*"},
				{Path: "populated subdir", Error: "*"},
			},
			false,
		},
		{
			"symbolic link removal with symbolic links ignored",
			func(f testingFilesystem) bool {
				return f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "file link", Old: tSR}},
			tDMContentMap,
			SymbolicLinkMode_SymbolicLinkModeIgnore,
			[]*Entry{tSR},
			[]*Problem{{Path: "file link", Error: "*"}},
			false,
		},
		{
			"symbolic link removal with modified symbolic link target",
			func(f testingFilesystem) bool {
				return f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil,
			func(root string) error {
				if err := os.Remove(filepath.Join(root, "file link")); err != nil {
					return err
				}
				return os.Symlink("executable file", filepath.Join(root, "file link"))
			},
			nil,
			backgroundCtx,
			[]*Change{{Path: "file link", Old: tSR}},
			tDMContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tSR},
			[]*Problem{{Path: "file link", Error: "*"}},
			false,
		},
		{
			"symbolic link removal with modified (to (invalid) absolute) symbolic link target",
			func(f testingFilesystem) bool {
				return runtime.GOOS == "windows" || f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil,
			func(root string) error {
				if err := os.Remove(filepath.Join(root, "file link")); err != nil {
					return err
				}
				return os.Symlink("/", filepath.Join(root, "file link"))
			},
			nil,
			backgroundCtx,
			[]*Change{{Path: "file link", Old: tSR}},
			tDMContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tSR},
			[]*Problem{{Path: "file link", Error: "*"}},
			false,
		},
		{
			"inaccessible directory removal",
			func(f testingFilesystem) bool {
				return runtime.GOOS == "windows" || f.name == "FAT32"
			},
			tDM, tDMContentMap,
			nil,
			func(root string) error {
				return os.Chmod(filepath.Join(root, "populated subdir"), 0300)
			},
			func(root string) error {
				return os.Chmod(filepath.Join(root, "populated subdir"), 0700)
			},
			backgroundCtx,
			[]*Change{{Path: "populated subdir", Old: tD1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tD1},
			[]*Problem{{Path: "populated subdir", Error: "*"}},
			false,
		},

		// Test file swapping problems.
		{
			"file root swapping on modified root",
			nil,
			tF1, tF1ContentMap,
			nil,
			func(root string) error {
				soon := time.Now().Add(10 * time.Second)
				return os.Chtimes(root, soon, soon)
			},
			nil,
			backgroundCtx,
			[]*Change{{Old: tF1, New: tF2}},
			tF2ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tF1},
			[]*Problem{{Error: "*"}},
			false,
		},

		// Test with a cancelled context.
		{
			"cancelled context",
			nil,
			tD1, tD1ContentMap,
			nil, nil, nil,
			cancelledCtx,
			[]*Change{{Old: tD1, New: tF1}},
			tF1ContentMap,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tD1},
			[]*Problem{{Error: errTransitionCancelled.Error()}},
			false,
		},

		// Test missing provider files.
		{
			"provider missing files",
			nil,
			nil, nil,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{New: tD1}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{tD0},
			[]*Problem{{Path: "file", Error: "*"}},
			true,
		},

		// Test symbolic link creation problems.
		{
			"disallowed symbolic link",
			nil,
			tD1, tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "link", New: &Entry{Kind: EntryKind_SymbolicLink, Target: "file"}}},
			nil,
			SymbolicLinkMode_SymbolicLinkModeIgnore,
			[]*Entry{nil},
			[]*Problem{{Path: "link", Error: "*"}},
			false,
		},
		{
			"invalid absolute symbolic link",
			nil,
			tD1, tD1ContentMap,
			nil, nil, nil,
			backgroundCtx,
			[]*Change{{Path: "link", New: &Entry{Kind: EntryKind_SymbolicLink, Target: "/file"}}},
			nil,
			SymbolicLinkMode_SymbolicLinkModePortable,
			[]*Entry{nil},
			[]*Problem{{Path: "link", Error: "*"}},
			false,
		},
	}

	// Create a hasher that we can use for testing.
	hasher := newTestingHasher()

	// Create a temporary directory that transition content providers can use
	// for staging. We'll put this on the OS temporary directory so that we test
	// same-device staging for the OS filesystem and cross-device staging for
	// test filesystems.
	transitionStagingStorage := t.TempDir()

	// Process test cases for every filesystem.
	for _, filesystem := range testingFilesystems {
		for _, test := range tests {
			// Check if this test is skipped on this platform or filesystem.
			if test.skip != nil && test.skip(filesystem) {
				continue
			}

			// Generate content for this test. We'll use storage on the target
			// filesystem for this staging so that we also get same-device
			// staging tests for test filesystems.
			generator := &testingContentManager{
				storage:            filesystem.storage,
				baseline:           test.baseline,
				baselineContentMap: test.baselineContentMap,
				tweak:              test.tweak,
				untweak:            test.untweak,
			}
			root, err := generator.generate()
			if err != nil {
				t.Errorf("%s: unable to generate test content on %s filesystem: %v",
					test.description, filesystem.name, err,
				)
				continue
			}

			// Define a cleanup function that will report errors.
			cleanup := func() {
				if err := generator.remove(); err != nil {
					t.Errorf("%s: unable to remove test content on %s filesystem: %v",
						test.description, filesystem.name, err,
					)
				}
			}

			// Create an ignorer that doesn't ignore anything.
			ignorer, err := mutagenignore.NewIgnorer(nil)
			if err != nil {
				t.Fatalf("%s: unable to create ignorer for %s filesystem: %v",
					test.description, filesystem.name, err,
				)
			}

			// Perform a scan to extract filesystem behavior and a cache.
			snapshot, cache, _, err := Scan(
				backgroundCtx,
				root,
				nil, nil,
				hasher, nil,
				ignorer, nil,
				behavior.ProbeMode_ProbeModeProbe,
				test.symbolicLinkMode,
				PermissionsMode_PermissionsModePortable,
			)
			if err != nil {
				t.Errorf("%s: unable to perform scan of baseline on %s filesystem: %v",
					test.description, filesystem.name, err,
				)
				cleanup()
				continue
			}

			// Perform retweaking operations, if necessary.
			if test.retweak != nil {
				if err := test.retweak(root); err != nil {
					t.Errorf("%s: unable to retweak root on %s filesystem: %v",
						test.description, filesystem.name, err,
					)
				}
			}

			// Perform the transition operation.
			provider := &testingProvider{
				storage:    transitionStagingStorage,
				contentMap: test.transitionsContentMap,
				hasher:     newTestingHasher(),
			}
			results, problems, missingFiles := Transition(
				test.ctx,
				root,
				test.transitions,
				cache,
				newTestingHasher(),
				test.symbolicLinkMode,
				0600,
				0700,
				nil,
				snapshot.DecomposesUnicode,
				provider,
			)

			// Check results.
			if len(results) != len(test.expectedResults) {
				t.Errorf("%s: length of results does not match expected on %s filesystem: %d != %d",
					test.description, filesystem.name, len(results), len(test.expectedResults),
				)
			} else {
				for r, result := range results {
					if test.expectedResults[r] != testingEntryWildcard && !result.Equal(test.expectedResults[r], true) {
						t.Errorf("%s: result %d does not match expected on %s filesystem",
							test.description, r, filesystem.name,
						)
					}
				}
			}

			// Check problems.
			if !testingProblemListsEqual(problems, test.expectedProblems) {
				t.Errorf("%s: problems do not match expected on %s filesystem", test.description, filesystem.name)
			}

			// Check missing file status.
			if missingFiles && !test.expectMissingFiles {
				t.Errorf("%s: unexpectedly missing staged files with %s filesystem", test.description, filesystem.name)
			} else if !missingFiles && test.expectMissingFiles {
				t.Errorf("%s: expected missing staged files with %s filesystem", test.description, filesystem.name)
			}

			// Perform cleanup.
			cleanup()
		}
	}
}

// TestTransitionGitLink tests that Transition refuses to remove submodules,
// even as part of a directory or a replacement, and that it leaves existing
// content in place when it can't fetch a submodule.
func TestTransitionGitLink(t *testing.T) {
	// Define the problems that we expect to see.
	const (
		removalRefused   = "submodule isn't removed automatically; remove it manually"
		fetchUnavailable = "submodule fetching is unavailable"
	)

	// Define a directory containing a submodule at a lower level, along with
	// what remains of it after a removal.
	nestedDirectory := &Entry{Contents: map[string]*Entry{
		"file":   tF1,
		"nested": tDG,
	}}
	nestedDirectoryReduced := &Entry{Contents: map[string]*Entry{"nested": tDGO}}

	// Define test cases. Each operates on the same on-disk content, in which the
	// directories that stand in for submodules are ordinary directories. Paths
	// in present and absent are relative to the root and indicate the on-disk
	// content expected afterward.
	tests := []struct {
		description      string
		transitions      []*Change
		contentMap       testingContentMap
		expectedResults  []*Entry
		expectedProblems []*Problem
		present          []string
		absent           []string
	}{
		{
			description:      "submodule removal",
			transitions:      []*Change{{Path: "directory/submodule", Old: tG1}},
			expectedResults:  []*Entry{tG1},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: removalRefused}},
			present:          []string{"directory/submodule/file"},
		},
		{
			description:      "removal of missing submodule",
			transitions:      []*Change{{Path: "missing", Old: tG1}},
			expectedResults:  []*Entry{tG1},
			expectedProblems: []*Problem{{Path: "missing", Error: removalRefused}},
		},
		{
			description:      "directory removal containing submodule",
			transitions:      []*Change{{Path: "directory", Old: tDG}},
			expectedResults:  []*Entry{tDGO},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: removalRefused}},
			present:          []string{"directory/submodule/file"},
			absent:           []string{"directory/file"},
		},
		{
			description:      "directory removal containing nested submodule",
			transitions:      []*Change{{Path: "parent", Old: nestedDirectory}},
			expectedResults:  []*Entry{nestedDirectoryReduced},
			expectedProblems: []*Problem{{Path: "parent/nested/submodule", Error: removalRefused}},
			present:          []string{"parent/nested/submodule/file"},
			absent:           []string{"parent/file", "parent/nested/file"},
		},
		{
			description:      "submodule replacement with file",
			transitions:      []*Change{{Path: "directory/submodule", Old: tG1, New: tF2}},
			contentMap:       testingContentMap{"directory/submodule": []byte(tF2Content)},
			expectedResults:  []*Entry{tG1},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: removalRefused}},
			present:          []string{"directory/submodule/file"},
		},
		{
			description:      "submodule replacement with directory",
			transitions:      []*Change{{Path: "directory/submodule", Old: tG1, New: tD0}},
			expectedResults:  []*Entry{tG1},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: removalRefused}},
			present:          []string{"directory/submodule/file"},
		},
		{
			description:      "directory replacement with file containing submodule",
			transitions:      []*Change{{Path: "directory", Old: tDG, New: tF2}},
			contentMap:       testingContentMap{"directory": []byte(tF2Content)},
			expectedResults:  []*Entry{tDGO},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: removalRefused}},
			present:          []string{"directory/submodule/file"},
			absent:           []string{"directory/file"},
		},
		{
			description:      "submodule creation",
			transitions:      []*Change{{Path: "created", New: tG1}},
			expectedResults:  []*Entry{nil},
			expectedProblems: []*Problem{{Path: "created", Error: fetchUnavailable}},
			absent:           []string{"created"},
		},
		{
			description:      "directory creation containing submodule",
			transitions:      []*Change{{Path: "created", New: tDG}},
			contentMap:       testingContentMap{"created/file": []byte(tF1Content)},
			expectedResults:  []*Entry{tD1},
			expectedProblems: []*Problem{{Path: "created/submodule", Error: fetchUnavailable}},
			present:          []string{"created/file"},
			absent:           []string{"created/submodule"},
		},
		{
			description:      "submodule move",
			transitions:      []*Change{{Path: "directory/submodule", Old: tG1, New: tG2}},
			expectedResults:  []*Entry{tG1},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: fetchUnavailable}},
			present:          []string{"directory/submodule/file"},
		},
		{
			description:      "empty directory replacement with submodule",
			transitions:      []*Change{{Path: "empty", Old: tD0, New: tG1}},
			expectedResults:  []*Entry{tD0},
			expectedProblems: []*Problem{{Path: "empty", Error: fetchUnavailable}},
			present:          []string{"empty"},
		},
		{
			description:      "directory replacement with submodule",
			transitions:      []*Change{{Path: "directory", Old: tDG, New: tG2}},
			expectedResults:  []*Entry{tDG},
			expectedProblems: []*Problem{{Path: "directory", Error: fetchUnavailable}},
			present:          []string{"directory/file", "directory/submodule/file"},
		},
		{
			description:      "file replacement with submodule",
			transitions:      []*Change{{Path: "replaced", Old: tF1, New: tG1}},
			expectedResults:  []*Entry{tF1},
			expectedProblems: []*Problem{{Path: "replaced", Error: fetchUnavailable}},
			present:          []string{"replaced"},
		},
		{
			description: "failures alongside other transitions",
			transitions: []*Change{
				{Path: "directory/submodule", Old: tG1, New: tG2},
				{Path: "replaced", Old: tF1},
			},
			expectedResults:  []*Entry{tG1, nil},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: fetchUnavailable}},
			present:          []string{"directory/submodule/file"},
			absent:           []string{"replaced"},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the on-disk content.
			root := filepath.Join(t.TempDir(), "root")
			for _, path := range []string{
				"directory/submodule",
				"parent/nested/submodule",
				"empty",
			} {
				if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
					t.Fatal("unable to create directory:", err)
				}
			}
			for _, path := range []string{
				"replaced",
				"directory/file",
				"directory/submodule/file",
				"parent/file",
				"parent/nested/file",
				"parent/nested/submodule/file",
			} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(tF1Content), 0600); err != nil {
					t.Fatal("unable to create file:", err)
				}
			}

			// Create an ignorer that doesn't ignore anything.
			ignorer, err := mutagenignore.NewIgnorer(nil)
			if err != nil {
				t.Fatal("unable to create ignorer:", err)
			}

			// Perform a scan to extract filesystem behavior and a cache.
			snapshot, cache, _, err := Scan(
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
				t.Fatal("unable to perform scan of baseline:", err)
			}

			// Copy the transitions' old entries so that we can verify below that
			// removals don't modify them.
			originals := make([]*Entry, len(test.transitions))
			for i, transition := range test.transitions {
				originals[i] = transition.Old.Copy(EntryCopyBehaviorDeep)
			}

			// Perform the transitions.
			results, problems, missingFiles := Transition(
				context.Background(),
				root,
				test.transitions,
				cache,
				newTestingHasher(),
				SymbolicLinkMode_SymbolicLinkModePortable,
				0600,
				0700,
				nil,
				snapshot.DecomposesUnicode,
				&testingProvider{
					storage:    t.TempDir(),
					contentMap: test.contentMap,
					hasher:     newTestingHasher(),
				},
			)

			// Check results.
			if len(results) != len(test.expectedResults) {
				t.Fatalf("length of results does not match expected: %d != %d", len(results), len(test.expectedResults))
			}
			for r, result := range results {
				if !result.Equal(test.expectedResults[r], true) {
					t.Errorf("result %d does not match expected: %v != %v", r, result, test.expectedResults[r])
				}
				if err := result.EnsureValid(true); err != nil {
					t.Errorf("result %d is invalid: %v", r, err)
				}
			}

			// Check problems.
			if !testingProblemListsEqual(problems, test.expectedProblems) {
				t.Errorf("problems do not match expected: %v != %v", problems, test.expectedProblems)
			}

			// Check missing file status.
			if missingFiles {
				t.Error("unexpectedly missing staged files")
			}

			// Verify that the old entries weren't modified.
			for i, transition := range test.transitions {
				if !transition.Old.Equal(originals[i], true) {
					t.Errorf("old entry %d was modified", i)
				}
			}

			// Check the on-disk content.
			for _, path := range test.present {
				if _, err := os.Lstat(filepath.Join(root, path)); err != nil {
					t.Errorf("expected content is missing (%s): %v", path, err)
				}
			}
			for _, path := range test.absent {
				if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
					t.Errorf("unexpected content is present (%s): %v", path, err)
				}
			}
		})
	}
}

// TestTransitionPortableSymbolicLinkEmptyComponents tests that Transition in
// portable symbolic link mode refuses to create a symbolic link whose target
// escapes the synchronization root through empty components (leaving nothing
// on disk), while still creating a symbolic link whose target contains empty
// components but stays within the root.
func TestTransitionPortableSymbolicLinkEmptyComponents(t *testing.T) {
	// Create an empty synchronization root.
	root := t.TempDir()

	// Create an ignorer that doesn't ignore anything.
	ignorer, err := mutagenignore.NewIgnorer(nil)
	if err != nil {
		t.Fatal("unable to create ignorer:", err)
	}

	// Perform a scan to extract filesystem behavior and a cache.
	snapshot, cache, _, err := Scan(
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
		t.Fatal("unable to perform scan of baseline:", err)
	}

	// Attempt to create both symbolic links.
	escape := &Entry{Kind: EntryKind_SymbolicLink, Target: "child//../../outside"}
	inside := &Entry{Kind: EntryKind_SymbolicLink, Target: "child//../inside"}
	provider := &testingProvider{
		storage: t.TempDir(),
		hasher:  newTestingHasher(),
	}
	results, problems, missingFiles := Transition(
		context.Background(),
		root,
		[]*Change{
			{Path: "escape", New: escape},
			{Path: "inside", New: inside},
		},
		cache,
		newTestingHasher(),
		SymbolicLinkMode_SymbolicLinkModePortable,
		0600,
		0700,
		nil,
		snapshot.DecomposesUnicode,
		provider,
	)

	// Check results.
	if len(results) != 2 {
		t.Fatalf("length of results does not match expected: %d != 2", len(results))
	}
	if !results[0].Equal(nil, true) {
		t.Error("escaping symbolic link creation result not empty:", results[0])
	}
	if !results[1].Equal(inside, true) {
		t.Errorf("non-escaping symbolic link creation result does not match expected: %v != %v",
			results[1], inside,
		)
	}

	// Check problems.
	if !testingProblemListsEqual(problems, []*Problem{{Path: "escape", Error: "*"}}) {
		t.Error("problems do not match expected:", problems)
	}

	// Check missing file status.
	if missingFiles {
		t.Error("unexpectedly missing staged files")
	}

	// Verify that the escaping symbolic link wasn't created on disk and that
	// the non-escaping one was.
	if _, err := os.Lstat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Error("escaping symbolic link exists on disk or can't be checked:", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "inside")); err != nil {
		t.Error("unable to find non-escaping symbolic link on disk:", err)
	}
}
