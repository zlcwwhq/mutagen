package core

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
)

// testingGitTransitionRoot creates a synchronization root containing the
// specified content (see testingWriteContent) and scans it, returning the
// root's path and the cache that the scan yields, which transitions use to
// verify files before removing or replacing them.
func testingGitTransitionRoot(t *testing.T, content map[string]string) (string, *Cache) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal("unable to create root:", err)
	}
	testingWriteContent(t, root, content)
	_, cache, _, err := Scan(
		context.Background(),
		root,
		nil, nil,
		newTestingHasher(), nil,
		&testingGitIgnorer{}, nil,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
	)
	if err != nil {
		t.Fatal("unable to scan root:", err)
	}
	return root, cache
}

// testingTransitionGitMode performs transitions in Git mode, with files
// provided from the specified content map. It verifies that no files are
// reported missing and that no temporary directories are left behind.
func testingTransitionGitMode(
	t *testing.T,
	ctx context.Context,
	root string,
	cache *Cache,
	gitProvider GitProvider,
	contentMap testingContentMap,
	transitions ...*Change,
) ([]*Entry, []*Problem) {
	t.Helper()
	provider := &testingProvider{storage: t.TempDir(), contentMap: contentMap, hasher: newTestingHasher()}
	results, problems, missingFiles := TransitionGitMode(
		ctx,
		root,
		transitions,
		cache,
		newTestingHasher(),
		SymbolicLinkMode_SymbolicLinkModePortable,
		0600,
		0700,
		nil,
		false,
		provider,
		gitProvider,
	)
	if missingFiles {
		t.Error("files reported missing")
	}
	if len(results) != len(transitions) {
		t.Fatalf("incorrect number of results: %d != %d", len(results), len(transitions))
	}
	if temporary := testingTemporaryClones(t, root); len(temporary) > 0 {
		t.Error("temporary directories left behind:", temporary)
	}
	return results, problems
}

// testingVerifyProblems verifies that a list of problems matches the expected
// list (see testingProblemListsEqual), including whether or not each is a
// warning.
func testingVerifyProblems(t *testing.T, problems, expected []*Problem) {
	t.Helper()
	if !testingProblemListsEqual(problems, expected) {
		t.Errorf("incorrect problems: %v != %v", problems, expected)
		return
	}
	warnings := make(map[string]bool, len(expected))
	for _, problem := range expected {
		warnings[problem.Path] = problem.Warning
	}
	for _, problem := range problems {
		if problem.Warning != warnings[problem.Path] {
			t.Errorf("incorrect warning classification for problem: %v", problem)
		}
	}
}

// testingVerifyFile verifies that a file exists within a root with the
// specified content.
func testingVerifyFile(t *testing.T, root, path, expected string) {
	t.Helper()
	if content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path))); err != nil {
		t.Errorf("unable to read %s: %v", path, err)
	} else if string(content) != expected {
		t.Errorf("incorrect content in %s: %q != %q", path, content, expected)
	}
}

// testingVerifyMissing verifies that nothing exists at a path within a root,
// either because the path doesn't exist or because a parent isn't a directory.
func testingVerifyMissing(t *testing.T, root, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
		t.Errorf("content exists at %s", path)
	}
}

// testingVerifyTemporaryDirectory verifies that a directory that a provider
// method was called with is a temporary directory within the specified parent.
func testingVerifyTemporaryDirectory(t *testing.T, call testingGitCall, parent string) {
	t.Helper()
	if filepath.Dir(call.directory) != parent {
		t.Errorf("%s called in incorrect directory: %s", call.method, call.directory)
	} else if !strings.HasPrefix(filepath.Base(call.directory), gitLinkTemporaryNamePrefix) {
		t.Errorf("%s called in non-temporary directory: %s", call.method, call.directory)
	}
}

// testingPopulationTarget is the content of a submodule as alpha has it, which
// a population receives as its target. Its working tree differs from what the
// clone contains, which the population doesn't use.
var testingPopulationTarget = testingSubmodule(testingSeeded(tGL0, "alpha"), map[string]*Entry{
	"a": tF2,
	"c": tF3,
})

// newTestingPopulationProvider creates a provider whose clones contain a few
// files, one of which is ignored, and which warns about them.
func newTestingPopulationProvider() *testingGitProvider {
	return &testingGitProvider{
		cloneFiles:    map[string]string{"a": tF1Content, "d/b": tF2Content, "ignored": tF3Content},
		cloneWarnings: []string{"fell back to the default branch"},
		ignored:       map[string]bool{"ignored": true},
	}
}

// testingPopulation is the synchronizable content that a population with the
// provider from newTestingPopulationProvider yields, apart from the seed.
var testingPopulation = testingSubmodule(tGL0, map[string]*Entry{
	"a": tF1,
	"d": nested("b", tF2),
})

// TestTransitionPopulate tests that a submodule is populated with a clone, in
// place of nothing, an empty directory, or a file, and that the result is the
// synchronizable content of the clone, with a new seed, which is recorded
// before the clone is moved into place.
func TestTransitionPopulate(t *testing.T) {
	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// content is the content of the root.
		content map[string]string
		// old is the content that's expected at the path.
		old *Entry
		// verifyOld verifies that the old content is in place.
		verifyOld func(t *testing.T, root string)
	}{
		{
			description: "nothing",
			verifyOld: func(t *testing.T, root string) {
				testingVerifyMissing(t, root, "sub")
			},
		},
		{
			description: "empty directory",
			content:     map[string]string{"sub/": ""},
			old:         tD0,
			verifyOld: func(t *testing.T, root string) {
				if contents, err := os.ReadDir(filepath.Join(root, "sub")); err != nil {
					t.Error("unable to read old directory:", err)
				} else if len(contents) != 0 {
					t.Error("old directory not empty")
				}
			},
		},
		{
			description: "file",
			content:     map[string]string{"sub": tF1Content},
			old:         tF1,
			verifyOld: func(t *testing.T, root string) {
				testingVerifyFile(t, root, "sub", tF1Content)
			},
		},
	}

	// Process test cases, tracking the seeds that populations yield.
	seeds := make(map[string]bool)
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the root and the provider, which verifies that the old
			// content is still in place when the population is recorded.
			root, cache := testingGitTransitionRoot(t, test.content)
			provider := newTestingPopulationProvider()
			provider.hooks = map[string]func(string){
				"RecordSeed": func(directory string) {
					test.verifyOld(t, root)
					if _, err := os.Stat(filepath.Join(directory, GitLinkName, "HEAD")); err != nil {
						t.Error("population recorded without clone:", err)
					}
				},
			}

			// Perform the population.
			results, problems := testingTransitionGitMode(t, context.Background(), root, cache, provider, nil,
				&Change{Path: "sub", Old: test.old, New: testingPopulationTarget},
			)

			// Verify the result, which is the clone's synchronizable content,
			// with a new seed, rather than the target's content.
			result := results[0]
			if !result.Equal(testingPopulation, true) {
				t.Errorf("incorrect result: %v != %v", result, testingPopulation)
			} else if err := result.EnsureValid(true); err != nil {
				t.Error("result invalid:", err)
			}
			link := result.gitLink()
			if link == nil {
				t.Fatal("result isn't a submodule")
			} else if seed, err := hex.DecodeString(link.Seed); err != nil || len(seed) != gitLinkSeedLength {
				t.Errorf("invalid seed: %q", link.Seed)
			} else if link.Url != tGitURL {
				t.Errorf("incorrect URL: %q", link.Url)
			} else if seeds[link.Seed] {
				t.Errorf("seed reused: %s", link.Seed)
			}
			seeds[link.Seed] = true

			// Verify that the population was recorded with the same content.
			if !proto.Equal(provider.recorded, result) {
				t.Errorf("recorded content differs from result: %v != %v", provider.recorded, result)
			}

			// Verify the provider calls.
			if methods := provider.methods(); !slices.Equal(methods, []string{"Clone", "ScanClone", "RecordSeed"}) {
				t.Errorf("incorrect provider calls: %v", methods)
			}
			for _, call := range provider.calls {
				testingVerifyTemporaryDirectory(t, call, root)
				if call.method == "ScanClone" && call.path != "sub" {
					t.Errorf("clone scanned with incorrect path: %q", call.path)
				}
			}

			// Verify that the only problem is the clone's warning.
			testingVerifyProblems(t, problems, []*Problem{
				{Path: "sub", Error: "fell back to the default branch", Warning: true},
			})

			// Verify the clone on disk, which includes ignored content.
			testingVerifyFile(t, root, "sub/a", tF1Content)
			testingVerifyFile(t, root, "sub/d/b", tF2Content)
			testingVerifyFile(t, root, "sub/ignored", tF3Content)
			testingVerifyFile(t, root, "sub/.git/HEAD", "ref: refs/heads/main\n")
			testingVerifyMissing(t, root, "sub/c")
		})
	}
}

// TestTransitionPopulateProblematicContent tests that problematic content in a
// clone isn't part of the result of a population, but is reported as problems
// at its full paths, and that unsynchronizable content isn't either.
func TestTransitionPopulateProblematicContent(t *testing.T) {
	root, cache := testingGitTransitionRoot(t, map[string]string{"directory/": ""})
	provider := &testingGitProvider{scanned: &Entry{Contents: map[string]*Entry{
		"a":         tF1,
		"bad":       tP1,
		"untracked": tU,
		"d":         {Contents: map[string]*Entry{"worse": tP2, "ok": tF2, "phantom": tPD1}},
	}}}
	results, problems := testingTransitionGitMode(t, context.Background(), root, cache, provider, nil,
		&Change{Path: "directory/sub", New: testingPopulationTarget},
	)
	expected := testingSubmodule(tGL0, map[string]*Entry{
		"a": tF1,
		"d": nested("ok", tF2),
	})
	if !results[0].Equal(expected, true) {
		t.Errorf("incorrect result: %v != %v", results[0], expected)
	} else if err := results[0].EnsureValid(true); err != nil {
		t.Error("result invalid:", err)
	} else if results[0].gitLink().Seed == "" {
		t.Error("result without seed")
	}
	if !proto.Equal(provider.recorded, results[0]) {
		t.Errorf("recorded content differs from result: %v != %v", provider.recorded, results[0])
	}
	testingVerifyProblems(t, problems, []*Problem{
		{Path: "directory/sub/bad", Error: tP1.Problem},
		{Path: "directory/sub/d/worse", Error: tP2.Problem},
	})
	if calls := provider.callsTo("ScanClone"); len(calls) != 1 || calls[0].path != "directory/sub" {
		t.Errorf("incorrect clone scans: %v", calls)
	} else {
		testingVerifyTemporaryDirectory(t, calls[0], filepath.Join(root, "directory"))
	}
}

// TestTransitionPopulateFailures tests that a population that fails leaves the
// old content in place, yields it, reports the failure, and removes the
// temporary directory, including any partial clone.
func TestTransitionPopulateFailures(t *testing.T) {
	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// content is the content of the root.
		content map[string]string
		// retweak modifies the root after it's scanned.
		retweak func(root string) error
		// old is the content that's expected at the path.
		old *Entry
		// configure configures the provider.
		configure func(*testingGitProvider)
		// expectedResult is the expected result.
		expectedResult *Entry
		// expectedProblems are the expected problems.
		expectedProblems []*Problem
		// expectedCalls are the names of the expected provider calls.
		expectedCalls []string
		// verify verifies the root's content.
		verify func(t *testing.T, root string)
	}{
		{
			description: "clone failure without old content",
			configure: func(p *testingGitProvider) {
				p.errors = map[string]error{"Clone": errors.New("clone failed")}
			},
			expectedProblems: []*Problem{{Path: "sub", Error: "clone failed"}},
			expectedCalls:    []string{"Clone"},
			verify: func(t *testing.T, root string) {
				testingVerifyMissing(t, root, "sub")
			},
		},
		{
			description: "clone failure with old content",
			content:     map[string]string{"sub/": ""},
			old:         tD0,
			configure: func(p *testingGitProvider) {
				p.errors = map[string]error{"Clone": errors.New("clone failed")}
			},
			expectedResult:   tD0,
			expectedProblems: []*Problem{{Path: "sub", Error: "clone failed"}},
			expectedCalls:    []string{"Clone"},
			verify: func(t *testing.T, root string) {
				testingVerifyMissing(t, root, "sub/.git")
			},
		},
		{
			description: "scan failure",
			content:     map[string]string{"sub": tF1Content},
			old:         tF1,
			configure: func(p *testingGitProvider) {
				p.errors = map[string]error{"ScanClone": errors.New("scan failed")}
			},
			expectedResult:   tF1,
			expectedProblems: []*Problem{{Path: "sub", Error: "unable to scan clone: scan failed"}},
			expectedCalls:    []string{"Clone", "ScanClone"},
			verify: func(t *testing.T, root string) {
				testingVerifyFile(t, root, "sub", tF1Content)
			},
		},
		{
			description: "clone that isn't a directory",
			content:     map[string]string{"sub/": ""},
			old:         tD0,
			configure: func(p *testingGitProvider) {
				p.scanned = tF1
			},
			expectedResult:   tD0,
			expectedProblems: []*Problem{{Path: "sub", Error: "clone isn't a directory"}},
			expectedCalls:    []string{"Clone", "ScanClone"},
		},
		{
			description: "record failure",
			content:     map[string]string{"sub/": ""},
			old:         tD0,
			configure: func(p *testingGitProvider) {
				p.errors = map[string]error{"RecordSeed": errors.New("record failed")}
			},
			expectedResult:   tD0,
			expectedProblems: []*Problem{{Path: "sub", Error: "unable to record population: record failed"}},
			expectedCalls:    []string{"Clone", "ScanClone", "RecordSeed"},
			verify: func(t *testing.T, root string) {
				testingVerifyMissing(t, root, "sub/.git")
			},
		},
		{
			description:      "unexpected content",
			content:          map[string]string{"sub/": ""},
			expectedProblems: []*Problem{{Path: "sub", Error: "unable to create submodule: unknown content exists at path"}},
			verify: func(t *testing.T, root string) {
				testingVerifyMissing(t, root, "sub/.git")
			},
		},
		{
			description: "unknown content in old directory",
			content:     map[string]string{"sub/": ""},
			retweak: func(root string) error {
				return os.WriteFile(filepath.Join(root, "sub", "stray"), []byte("stray"), 0600)
			},
			old:              tD0,
			expectedResult:   tD0,
			expectedProblems: []*Problem{{Path: "sub/stray", Error: "unknown content encountered on disk"}},
			expectedCalls:    []string{"Clone", "ScanClone", "RecordSeed"},
			verify: func(t *testing.T, root string) {
				testingVerifyFile(t, root, "sub/stray", "stray")
				testingVerifyMissing(t, root, "sub/.git")
			},
		},
		{
			description: "modified old file",
			content:     map[string]string{"sub": tF1Content},
			retweak: func(root string) error {
				return os.WriteFile(filepath.Join(root, "sub"), []byte("modified"), 0600)
			},
			old:              tF1,
			expectedResult:   tF1,
			expectedProblems: []*Problem{{Path: "sub", Error: "unable to remove file: unable to validate existing file: modification detected"}},
			expectedCalls:    []string{"Clone", "ScanClone", "RecordSeed"},
			verify: func(t *testing.T, root string) {
				testingVerifyFile(t, root, "sub", "modified")
			},
		},
		{
			description:      "old content containing a submodule",
			content:          map[string]string{"sub/.git/HEAD": "head"},
			old:              testingSubmodule(tGL1, nil),
			expectedResult:   testingSubmodule(tGL1, nil),
			expectedProblems: []*Problem{{Path: "sub/.git", Error: errGitLinkRemovalRefused.Error()}},
			expectedCalls:    []string{"Clone", "ScanClone", "RecordSeed"},
			verify: func(t *testing.T, root string) {
				testingVerifyFile(t, root, "sub/.git/HEAD", "head")
			},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the root and the provider.
			root, cache := testingGitTransitionRoot(t, test.content)
			if test.retweak != nil {
				if err := test.retweak(root); err != nil {
					t.Fatal("unable to modify root:", err)
				}
			}
			provider := newTestingPopulationProvider()
			if test.configure != nil {
				test.configure(provider)
			}

			// Perform the population and verify the results.
			results, problems := testingTransitionGitMode(t, context.Background(), root, cache, provider, nil,
				&Change{Path: "sub", Old: test.old, New: testingPopulationTarget},
			)
			if !results[0].Equal(test.expectedResult, true) {
				t.Errorf("incorrect result: %v != %v", results[0], test.expectedResult)
			}
			testingVerifyProblems(t, problems, test.expectedProblems)
			if methods := provider.methods(); !slices.Equal(methods, test.expectedCalls) {
				t.Errorf("incorrect provider calls: %v != %v", methods, test.expectedCalls)
			}
			testingVerifyMissing(t, root, "sub/partial")
			if test.verify != nil {
				test.verify(t, root)
			}
		})
	}
}

// TestTransitionPopulateUnavailable tests that populations that can't be
// performed at all are refused without touching anything.
func TestTransitionPopulateUnavailable(t *testing.T) {
	// Without a provider, nothing can be populated, either in place of other
	// content or as part of a new directory.
	root, cache := testingGitTransitionRoot(t, map[string]string{"sub/": ""})
	storage := t.TempDir()
	provider := &testingProvider{storage: storage, contentMap: tD1ContentMap, hasher: newTestingHasher()}
	results, problems, _ := Transition(
		context.Background(),
		root,
		[]*Change{
			{Path: "sub", Old: tD0, New: testingPopulationTarget},
			{Path: "directory", New: &Entry{Contents: map[string]*Entry{"sub": testingPopulationTarget}}},
		},
		cache,
		newTestingHasher(),
		SymbolicLinkMode_SymbolicLinkModePortable,
		0600,
		0700,
		nil,
		false,
		provider,
	)
	if !results[0].Equal(tD0, true) {
		t.Errorf("incorrect result for replacement without provider: %v", results[0])
	} else if !results[1].Equal(tD0, true) {
		t.Errorf("incorrect result for creation without provider: %v", results[1])
	}
	testingVerifyProblems(t, problems, []*Problem{
		{Path: "sub", Error: "submodule population is unavailable"},
		{Path: "directory/sub", Error: "submodule population is unavailable"},
	})

	// The root can't be a submodule.
	gitProvider := newTestingPopulationProvider()
	results, problems = testingTransitionGitMode(t, context.Background(), root, cache, gitProvider, nil,
		&Change{Path: "", Old: &Entry{Contents: map[string]*Entry{"sub": tD0}}, New: testingPopulationTarget},
	)
	if !results[0].Equal(&Entry{Contents: map[string]*Entry{"sub": tD0}}, true) {
		t.Errorf("incorrect result for root population: %v", results[0])
	}
	testingVerifyProblems(t, problems, []*Problem{{Path: "", Error: "synchronization root can't be a submodule"}})
	if methods := gitProvider.methods(); len(methods) != 0 {
		t.Errorf("provider called for root population: %v", methods)
	}
}

// TestTransitionPopulateCancellation tests that a population that's cancelled
// leaves the old content in place and reports the cancellation.
func TestTransitionPopulateCancellation(t *testing.T) {
	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// method is the provider method that cancels the transition, or empty if
		// the transition is cancelled before it starts.
		method string
		// fail indicates whether or not the method fails because of the
		// cancellation.
		fail bool
		// expectedCalls are the names of the expected provider calls.
		expectedCalls []string
	}{
		{"before transition", "", false, nil},
		{"during clone", "Clone", true, []string{"Clone"}},
		{"during clone scan", "ScanClone", true, []string{"Clone", "ScanClone"}},
		{"after recording population", "RecordSeed", false, []string{"Clone", "ScanClone", "RecordSeed"}},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the root, the context, and the provider.
			root, cache := testingGitTransitionRoot(t, map[string]string{"sub/": ""})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := newTestingPopulationProvider()
			if test.method == "" {
				cancel()
			} else {
				provider.hooks = map[string]func(string){test.method: func(string) { cancel() }}
				if test.fail {
					provider.errors = map[string]error{test.method: context.Canceled}
				}
			}

			// Perform the population and verify the results.
			results, problems := testingTransitionGitMode(t, ctx, root, cache, provider, nil,
				&Change{Path: "sub", Old: tD0, New: testingPopulationTarget},
			)
			if !results[0].Equal(tD0, true) {
				t.Errorf("incorrect result: %v", results[0])
			}
			testingVerifyProblems(t, problems, []*Problem{{Path: "sub", Error: errTransitionCancelled.Error()}})
			if methods := provider.methods(); !slices.Equal(methods, test.expectedCalls) {
				t.Errorf("incorrect provider calls: %v != %v", methods, test.expectedCalls)
			}
			testingVerifyMissing(t, root, "sub/.git")
		})
	}
}

// TestTransitionCreateDirectoryWithSubmodule tests that a submodule within a
// directory that's being created is populated, and that the directory's other
// contents are created regardless of whether or not that succeeds.
func TestTransitionCreateDirectoryWithSubmodule(t *testing.T) {
	// Define the directory to create and the files that it requires.
	directory := &Entry{Contents: map[string]*Entry{
		"file":  tF1,
		"plain": tD1,
		"sub":   testingPopulationTarget,
	}}
	contentMap := testingContentMap{
		"directory/file":       []byte(tF1Content),
		"directory/plain/file": []byte(tF1Content),
	}

	// Verify that the submodule is populated.
	root, cache := testingGitTransitionRoot(t, nil)
	provider := newTestingPopulationProvider()
	results, problems := testingTransitionGitMode(t, context.Background(), root, cache, provider, contentMap,
		&Change{Path: "directory", New: directory},
	)
	expected := &Entry{Contents: map[string]*Entry{
		"file":  tF1,
		"plain": tD1,
		"sub":   testingPopulation,
	}}
	if !results[0].Equal(expected, true) {
		t.Errorf("incorrect result: %v != %v", results[0], expected)
	} else if results[0].Contents["sub"].gitLink().Seed == "" {
		t.Error("populated submodule without seed")
	}
	testingVerifyProblems(t, problems, []*Problem{
		{Path: "directory/sub", Error: "fell back to the default branch", Warning: true},
	})
	if calls := provider.callsTo("Clone"); len(calls) != 1 {
		t.Errorf("incorrect clones: %v", calls)
	} else {
		testingVerifyTemporaryDirectory(t, calls[0], filepath.Join(root, "directory"))
	}
	testingVerifyFile(t, root, "directory/sub/a", tF1Content)
	testingVerifyFile(t, root, "directory/sub/.git/HEAD", "ref: refs/heads/main\n")
	testingVerifyMissing(t, root, "directory/sub/c")

	// Verify that the rest of the directory is created if the population fails.
	root, cache = testingGitTransitionRoot(t, nil)
	provider = newTestingPopulationProvider()
	provider.errors = map[string]error{"Clone": errors.New("clone failed")}
	results, problems = testingTransitionGitMode(t, context.Background(), root, cache, provider, contentMap,
		&Change{Path: "directory", New: directory},
	)
	expected = &Entry{Contents: map[string]*Entry{"file": tF1, "plain": tD1}}
	if !results[0].Equal(expected, true) {
		t.Errorf("incorrect result after failed population: %v != %v", results[0], expected)
	}
	testingVerifyProblems(t, problems, []*Problem{{Path: "directory/sub", Error: "clone failed"}})
	testingVerifyMissing(t, root, "directory/sub")
	testingVerifyFile(t, root, "directory/plain/file", tF1Content)
}

// TestTransitionAttach tests that a submodule's Git entry is attached to an
// existing directory without touching its other contents.
func TestTransitionAttach(t *testing.T) {
	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// content is the content of the root.
		content map[string]string
		// path is the path of the Git entry to attach.
		path string
		// configure configures the provider.
		configure func(*testingGitProvider, context.CancelFunc)
		// expectedResult is the expected result.
		expectedResult *Entry
		// expectedProblems are the expected problems.
		expectedProblems []*Problem
		// expectedCalls are the names of the expected provider calls.
		expectedCalls []string
		// attached indicates whether or not the Git entry should be in place
		// afterward.
		attached bool
	}{
		{
			description:    "success",
			content:        map[string]string{"sub/a": tF1Content},
			path:           "sub/.git",
			expectedResult: tGL0,
			expectedCalls:  []string{"PrepareAttach", "Refresh"},
			attached:       true,
		},
		{
			description: "fallback",
			content:     map[string]string{"sub/a": tF1Content},
			path:        "sub/.git",
			configure: func(p *testingGitProvider, _ context.CancelFunc) {
				p.attachWarnings = []string{"fell back to the default branch"}
			},
			expectedResult:   tGL0,
			expectedProblems: []*Problem{{Path: "sub/.git", Error: "fell back to the default branch", Warning: true}},
			expectedCalls:    []string{"PrepareAttach", "Refresh"},
			attached:         true,
		},
		{
			description: "refresh failure",
			content:     map[string]string{"sub/a": tF1Content},
			path:        "sub/.git",
			configure: func(p *testingGitProvider, _ context.CancelFunc) {
				p.errors = map[string]error{"Refresh": errors.New("refresh failed")}
			},
			expectedResult: tGL0,
			expectedCalls:  []string{"PrepareAttach", "Refresh"},
			attached:       true,
		},
		{
			description: "preparation failure",
			content:     map[string]string{"sub/a": tF1Content},
			path:        "sub/.git",
			configure: func(p *testingGitProvider, _ context.CancelFunc) {
				p.errors = map[string]error{"PrepareAttach": errors.New("preparation failed")}
			},
			expectedProblems: []*Problem{{Path: "sub/.git", Error: "preparation failed"}},
			expectedCalls:    []string{"PrepareAttach"},
		},
		{
			description: "cancellation",
			content:     map[string]string{"sub/a": tF1Content},
			path:        "sub/.git",
			configure: func(p *testingGitProvider, cancel context.CancelFunc) {
				p.hooks = map[string]func(string){"PrepareAttach": func(string) { cancel() }}
				p.errors = map[string]error{"PrepareAttach": context.Canceled}
			},
			expectedProblems: []*Problem{{Path: "sub/.git", Error: errTransitionCancelled.Error()}},
			expectedCalls:    []string{"PrepareAttach"},
		},
		{
			description:      "existing Git entry",
			content:          map[string]string{"sub/a": tF1Content, "sub/.git": "gitdir: elsewhere"},
			path:             "sub/.git",
			expectedProblems: []*Problem{{Path: "sub/.git", Error: "unable to attach repository: unknown content exists at path"}},
		},
		{
			description:      "path other than a Git entry",
			content:          map[string]string{"sub/a": tF1Content},
			path:             "sub/a.git",
			expectedProblems: []*Problem{{Path: "sub/a.git", Error: "submodule Git entry requested at invalid path"}},
		},
		{
			description:      "root's Git entry",
			content:          map[string]string{"sub/a": tF1Content},
			path:             ".git",
			expectedProblems: []*Problem{{Path: ".git", Error: "submodule Git entry requested at invalid path"}},
		},
		{
			description:      "missing directory",
			path:             "missing/.git",
			expectedProblems: []*Problem{{Path: "missing/.git", Error: "*"}},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the root, the context, and the provider.
			root, cache := testingGitTransitionRoot(t, test.content)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &testingGitProvider{}
			if test.configure != nil {
				test.configure(provider, cancel)
			}

			// Perform the attachment and verify the results. The result doesn't
			// carry the target's seed, which describes how the other endpoint
			// came to have it.
			results, problems := testingTransitionGitMode(t, ctx, root, cache, provider, nil,
				&Change{Path: test.path, New: testingSeeded(tGL0, "alpha")},
			)
			if !results[0].Equal(test.expectedResult, true) {
				t.Errorf("incorrect result: %v != %v", results[0], test.expectedResult)
			} else if results[0] != nil && (results[0].Seed != "" || results[0].Url != tGitURL) {
				t.Errorf("incorrect GitLink entry fields: %v", results[0])
			}
			testingVerifyProblems(t, problems, test.expectedProblems)
			if methods := provider.methods(); !slices.Equal(methods, test.expectedCalls) {
				t.Errorf("incorrect provider calls: %v != %v", methods, test.expectedCalls)
			}
			for _, call := range provider.calls {
				if call.method == "PrepareAttach" {
					testingVerifyTemporaryDirectory(t, call, filepath.Join(root, "sub"))
				} else if call.method == "Refresh" && call.directory != filepath.Join(root, "sub") {
					t.Errorf("refresh in incorrect directory: %s", call.directory)
				}
			}

			// Verify the content on disk. The working tree is never touched.
			if test.attached {
				testingVerifyFile(t, root, "sub/.git/HEAD", "ref: refs/heads/main\n")
			} else if test.content["sub/.git"] == "" {
				testingVerifyMissing(t, root, "sub/.git")
			}
			if test.content["sub/a"] != "" {
				testingVerifyFile(t, root, "sub/a", tF1Content)
			}
		})
	}
}

// TestTransitionFollow tests that a submodule's Git entry follows another
// commit, and that a failure to do so is only a warning.
func TestTransitionFollow(t *testing.T) {
	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// content is the content of the root.
		content map[string]string
		// path is the path of the Git entry.
		path string
		// configure configures the provider.
		configure func(*testingGitProvider, context.CancelFunc)
		// expectedResult is the expected result.
		expectedResult *Entry
		// expectedProblems are the expected problems.
		expectedProblems []*Problem
		// expectedCalls are the names of the expected provider calls.
		expectedCalls []string
	}{
		{
			description:    "success",
			content:        map[string]string{"sub/.git/HEAD": "head", "sub/a": tF1Content},
			path:           "sub/.git",
			expectedResult: tGL1,
			expectedCalls:  []string{"Follow"},
		},
		{
			description: "failure",
			content:     map[string]string{"sub/.git/HEAD": "head", "sub/a": tF1Content},
			path:        "sub/.git",
			configure: func(p *testingGitProvider, _ context.CancelFunc) {
				p.errors = map[string]error{"Follow": errors.New("commit not available yet")}
			},
			expectedResult:   tGL0,
			expectedProblems: []*Problem{{Path: "sub/.git", Error: "commit not available yet", Warning: true}},
			expectedCalls:    []string{"Follow"},
		},
		{
			description: "cancellation",
			content:     map[string]string{"sub/.git/HEAD": "head", "sub/a": tF1Content},
			path:        "sub/.git",
			configure: func(p *testingGitProvider, cancel context.CancelFunc) {
				p.hooks = map[string]func(string){"Follow": func(string) { cancel() }}
				p.errors = map[string]error{"Follow": context.Canceled}
			},
			expectedResult:   tGL0,
			expectedProblems: []*Problem{{Path: "sub/.git", Error: errTransitionCancelled.Error()}},
			expectedCalls:    []string{"Follow"},
		},
		{
			description:      "missing Git entry",
			content:          map[string]string{"sub/a": tF1Content},
			path:             "sub/.git",
			expectedResult:   tGL0,
			expectedProblems: []*Problem{{Path: "sub/.git", Error: "*"}},
		},
		{
			description:      "path other than a Git entry",
			content:          map[string]string{"sub/a": tF1Content},
			path:             "sub/a",
			expectedResult:   tGL0,
			expectedProblems: []*Problem{{Path: "sub/a", Error: "submodule Git entry requested at invalid path"}},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the root, the context, and the provider.
			root, cache := testingGitTransitionRoot(t, test.content)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &testingGitProvider{}
			if test.configure != nil {
				test.configure(provider, cancel)
			}

			// Perform the transition and verify the results.
			results, problems := testingTransitionGitMode(t, ctx, root, cache, provider, nil,
				&Change{Path: test.path, Old: tGL0, New: tGL1},
			)
			if !results[0].Equal(test.expectedResult, true) {
				t.Errorf("incorrect result: %v != %v", results[0], test.expectedResult)
			}
			testingVerifyProblems(t, problems, test.expectedProblems)
			if methods := provider.methods(); !slices.Equal(methods, test.expectedCalls) {
				t.Errorf("incorrect provider calls: %v != %v", methods, test.expectedCalls)
			}
			for _, call := range provider.calls {
				if call.directory != filepath.Join(root, "sub") {
					t.Errorf("follow in incorrect directory: %s", call.directory)
				}
			}
			testingVerifyFile(t, root, "sub/a", tF1Content)
		})
	}
}

// TestTransitionSubmoduleRemoval tests that a submodule's Git entry is never
// removed, whether by itself or as part of a directory, and that the provider
// isn't involved in that. The removal of a directory containing a submodule is
// refused as a whole, so that the submodule's working tree stays intact.
func TestTransitionSubmoduleRemoval(t *testing.T) {
	// Define the content on disk.
	content := map[string]string{
		"sub/.git/HEAD":              "head",
		"sub/a":                      tF1Content,
		"directory/nested/.git/HEAD": "head",
		"directory/nested/a":         tF1Content,
	}
	refused := errGitLinkRemovalRefused.Error()

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// transition is the transition.
		transition *Change
		// expectedResult is the expected result.
		expectedResult *Entry
		// expectedProblems are the expected problems.
		expectedProblems []*Problem
	}{
		{
			description:      "removal of Git entry",
			transition:       &Change{Path: "sub/.git", Old: tGL0},
			expectedResult:   tGL0,
			expectedProblems: []*Problem{{Path: "sub/.git", Error: refused}},
		},
		{
			description:      "replacement of Git entry",
			transition:       &Change{Path: "sub/.git", Old: tGL0, New: tD0},
			expectedResult:   tGL0,
			expectedProblems: []*Problem{{Path: "sub/.git", Error: refused}},
		},
		{
			description:      "removal of submodule",
			transition:       &Change{Path: "sub", Old: testingSubmodule(tGL0, map[string]*Entry{"a": tF1})},
			expectedResult:   testingSubmodule(tGL0, map[string]*Entry{"a": tF1}),
			expectedProblems: []*Problem{{Path: "sub/.git", Error: refused}},
		},
		{
			description:      "replacement of submodule",
			transition:       &Change{Path: "sub", Old: testingSubmodule(tGL0, map[string]*Entry{"a": tF1}), New: tF1},
			expectedResult:   testingSubmodule(tGL0, map[string]*Entry{"a": tF1}),
			expectedProblems: []*Problem{{Path: "sub/.git", Error: refused}},
		},
		{
			description: "removal of directory containing submodule",
			transition: &Change{Path: "directory", Old: nested("nested", testingSubmodule(tGL0, map[string]*Entry{
				"a": tF1,
			}))},
			expectedResult: nested("nested", testingSubmodule(tGL0, map[string]*Entry{
				"a": tF1,
			})),
			expectedProblems: []*Problem{{Path: "directory/nested/.git", Error: refused}},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			root, cache := testingGitTransitionRoot(t, content)
			provider := &testingGitProvider{}
			results, problems := testingTransitionGitMode(t, context.Background(), root, cache, provider, nil,
				test.transition,
			)
			if !results[0].Equal(test.expectedResult, true) {
				t.Errorf("incorrect result: %v != %v", results[0], test.expectedResult)
			}
			testingVerifyProblems(t, problems, test.expectedProblems)
			if methods := provider.methods(); len(methods) != 0 {
				t.Errorf("provider called: %v", methods)
			}
			testingVerifyFile(t, root, "sub/.git/HEAD", "head")
			testingVerifyFile(t, root, "sub/a", tF1Content)
			testingVerifyFile(t, root, "directory/nested/.git/HEAD", "head")
			testingVerifyFile(t, root, "directory/nested/a", tF1Content)
		})
	}
}
