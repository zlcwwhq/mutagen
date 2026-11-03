package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// testingSubmoduleScanContent is the content of a root for scans in Git mode,
// which has a submodule at sub, with a submodule nested within it at
// sub/nested, and a Git entry of its own.
var testingSubmoduleScanContent = map[string]string{
	".git/HEAD":            "head",
	"a":                    tF1Content,
	"other/z":              tF1Content,
	"sub/.git/HEAD":        "head",
	"sub/x":                tF1Content,
	"sub/d/y":              tF2Content,
	"sub/nested/.git/HEAD": "head",
	"sub/nested/w":         tF2Content,
}

// testingScanGitMode performs a scan in Git mode.
func testingScanGitMode(
	ctx context.Context,
	root string,
	baseline *Snapshot,
	cache *Cache,
	ignorer ignore.Ignorer,
	ignoreCache ignore.IgnoreCache,
	gitProvider GitProvider,
) (*Snapshot, *Cache, ignore.IgnoreCache, error) {
	return ScanGitMode(
		ctx,
		root,
		baseline, nil,
		newTestingHasher(), cache,
		ignorer, ignoreCache,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
		gitProvider,
	)
}

// testingProbedPaths returns the paths that a provider has probed, sorted.
func testingProbedPaths(t *testing.T, root string, provider *testingGitProvider) []string {
	t.Helper()
	var paths []string
	for _, call := range provider.callsTo("Probe") {
		if call.directory != filepath.Join(root, filepath.FromSlash(call.path)) {
			t.Errorf("probe of %q in incorrect directory: %s", call.path, call.directory)
		}
		paths = append(paths, call.path)
	}
	slices.Sort(paths)
	return paths
}

// TestScanGitModeSubmodules tests how ScanGitMode represents the directories
// that have Git entries, depending on what the provider reports about them.
func TestScanGitModeSubmodules(t *testing.T) {
	// Define the entries that are the same in each case.
	other := nested("z", tF1)
	nestedSubmodule := testingSubmodule(tGL1, map[string]*Entry{"w": tF2})
	nestedPlain := &Entry{Contents: map[string]*Entry{GitLinkName: tU, "w": tF2}}

	// Define test cases.
	tests := []struct {
		// description is a human readable description of the test case.
		description string
		// configure configures the provider.
		configure func(*testingGitProvider)
		// ignorer is the ignorer to use, in addition to Git entries.
		ignorer *testingGitIgnorer
		// expectedSub is the expected entry for the sub directory.
		expectedSub *Entry
		// expectedProbes are the paths that are expected to be probed.
		expectedProbes []string
		// expectedDirectories is the expected directory count.
		expectedDirectories uint64
		// expectedFiles is the expected file count.
		expectedFiles uint64
	}{
		{
			description: "submodules",
			configure: func(p *testingGitProvider) {
				p.links = map[string]*Entry{"sub": tGL0, "sub/nested": tGL1}
			},
			expectedSub: testingSubmodule(tGL0, map[string]*Entry{
				"x":      tF1,
				"d":      nested("y", tF2),
				"nested": nestedSubmodule,
			}),
			expectedProbes:      []string{"sub", "sub/nested"},
			expectedDirectories: 5,
			expectedFiles:       5,
		},
		{
			description: "repositories that aren't submodules",
			expectedSub: &Entry{Contents: map[string]*Entry{
				GitLinkName: tU,
				"x":         tF1,
				"d":         nested("y", tF2),
				"nested":    nestedPlain,
			}},
			expectedProbes:      []string{"sub", "sub/nested"},
			expectedDirectories: 5,
			expectedFiles:       5,
		},
		{
			description: "probe failure",
			configure: func(p *testingGitProvider) {
				p.errors = map[string]error{"Probe": errors.New("unable to examine repository")}
			},
			expectedSub:         &Entry{Kind: EntryKind_Problematic, Problem: "unable to examine repository"},
			expectedProbes:      []string{"sub"},
			expectedDirectories: 2,
			expectedFiles:       2,
		},
		{
			description: "ignore masked submodule",
			configure: func(p *testingGitProvider) {
				p.links = map[string]*Entry{"sub": tGL0, "sub/nested": tGL1}
			},
			ignorer:             &testingGitIgnorer{masked: map[string]bool{"sub": true}},
			expectedSub:         tU,
			expectedDirectories: 2,
			expectedFiles:       2,
		},
		{
			description: "ignored submodule",
			configure: func(p *testingGitProvider) {
				p.links = map[string]*Entry{"sub": tGL0, "sub/nested": tGL1}
			},
			ignorer:             &testingGitIgnorer{ignored: map[string]bool{"sub": true}},
			expectedSub:         tU,
			expectedDirectories: 2,
			expectedFiles:       2,
		},
		{
			description: "ignore masked submodule within submodule",
			configure: func(p *testingGitProvider) {
				p.links = map[string]*Entry{"sub": tGL0, "sub/nested": tGL1}
			},
			ignorer: &testingGitIgnorer{masked: map[string]bool{"sub/nested": true}},
			expectedSub: testingSubmodule(tGL0, map[string]*Entry{
				"x":      tF1,
				"d":      nested("y", tF2),
				"nested": tU,
			}),
			expectedProbes:      []string{"sub"},
			expectedDirectories: 4,
			expectedFiles:       4,
		},
		{
			description: "probe failure within submodule",
			configure: func(p *testingGitProvider) {
				p.links = map[string]*Entry{"sub": tGL0}
				p.probeErrors = map[string]error{"sub/nested": errors.New("unable to examine nested repository")}
			},
			expectedSub: testingSubmodule(tGL0, map[string]*Entry{
				"x":      tF1,
				"d":      nested("y", tF2),
				"nested": {Kind: EntryKind_Problematic, Problem: "unable to examine nested repository"},
			}),
			expectedProbes:      []string{"sub", "sub/nested"},
			expectedDirectories: 4,
			expectedFiles:       4,
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the root and the provider.
			root := t.TempDir()
			testingWriteContent(t, root, testingSubmoduleScanContent)
			provider := &testingGitProvider{}
			if test.configure != nil {
				test.configure(provider)
			}
			ignorer := test.ignorer
			if ignorer == nil {
				ignorer = &testingGitIgnorer{}
			}

			// Perform the scan.
			snapshot, _, _, err := testingScanGitMode(context.Background(), root, nil, nil, ignorer, nil, provider)
			if err != nil {
				t.Fatal("unable to scan:", err)
			}

			// Verify the result. The root is never probed, so its Git entry is
			// untracked, like any other ignored content.
			expected := &Entry{Contents: map[string]*Entry{
				GitLinkName: tU,
				"a":         tF1,
				"other":     other,
				"sub":       test.expectedSub,
			}}
			if !snapshot.Content.Equal(expected, true) {
				t.Errorf("incorrect content: %v != %v", snapshot.Content, expected)
			} else if err := snapshot.EnsureValid(); err != nil {
				t.Error("snapshot invalid:", err)
			}
			if probes := testingProbedPaths(t, root, provider); !slices.Equal(probes, test.expectedProbes) {
				t.Errorf("incorrect probes: %v != %v", probes, test.expectedProbes)
			}

			// GitLink entries don't count towards the statistics.
			if snapshot.Directories != test.expectedDirectories {
				t.Errorf("incorrect directory count: %d != %d", snapshot.Directories, test.expectedDirectories)
			}
			if snapshot.Files != test.expectedFiles {
				t.Errorf("incorrect file count: %d != %d", snapshot.Files, test.expectedFiles)
			}
		})
	}
}

// TestScanGitModeWithoutProvider tests that Scan, and ScanGitMode without a
// provider, treat directories with Git entries as ordinary directories.
func TestScanGitModeWithoutProvider(t *testing.T) {
	root := t.TempDir()
	testingWriteContent(t, root, testingSubmoduleScanContent)
	expected := &Entry{Contents: map[string]*Entry{
		GitLinkName: tU,
		"a":         tF1,
		"other":     nested("z", tF1),
		"sub": {Contents: map[string]*Entry{
			GitLinkName: tU,
			"x":         tF1,
			"d":         nested("y", tF2),
			"nested":    {Contents: map[string]*Entry{GitLinkName: tU, "w": tF2}},
		}},
	}}
	snapshot, _, _, err := testingScanGitMode(context.Background(), root, nil, nil, &testingGitIgnorer{}, nil, nil)
	if err != nil {
		t.Fatal("unable to scan:", err)
	} else if !snapshot.Content.Equal(expected, true) {
		t.Errorf("incorrect content: %v != %v", snapshot.Content, expected)
	}
	snapshot, _, _, err = Scan(
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
		t.Fatal("unable to scan:", err)
	} else if !snapshot.Content.Equal(expected, true) {
		t.Errorf("incorrect content: %v != %v", snapshot.Content, expected)
	}
}

// TestScanGitModeCancellation tests that a probe that fails because the scan
// has been cancelled cancels the scan, rather than being reported as a
// problem with the directory.
func TestScanGitModeCancellation(t *testing.T) {
	root := t.TempDir()
	testingWriteContent(t, root, testingSubmoduleScanContent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &testingGitProvider{
		hooks:  map[string]func(string){"Probe": func(string) { cancel() }},
		errors: map[string]error{"Probe": context.Canceled},
	}
	if _, _, _, err := testingScanGitMode(ctx, root, nil, nil, &testingGitIgnorer{}, nil, provider); !errors.Is(err, ErrScanCancelled) {
		t.Errorf("incorrect error for cancelled scan: %v", err)
	}
}

// TestScanGitModeAccelerated tests that an accelerated scan in Git mode always
// examines the submodules in its baseline again, since their commits can change
// without any filesystem events in their directories, while still reusing the
// baseline for the rest of their contents.
func TestScanGitModeAccelerated(t *testing.T) {
	// Perform a scan to use as the baseline.
	root := t.TempDir()
	testingWriteContent(t, root, testingSubmoduleScanContent)
	provider := &testingGitProvider{links: map[string]*Entry{"sub": tGL0, "sub/nested": tGL1}}
	ignorer := &testingGitIgnorer{}
	baseline, cache, ignoreCache, err := testingScanGitMode(context.Background(), root, nil, nil, ignorer, nil, provider)
	if err != nil {
		t.Fatal("unable to perform baseline scan:", err)
	}

	// Move the submodule's commit, without any change on disk, and perform an
	// accelerated scan without re-check paths.
	provider.lock.Lock()
	provider.links["sub"] = tGL2
	provider.calls = nil
	provider.lock.Unlock()
	snapshot, newCache, _, err := testingScanGitMode(context.Background(), root, baseline, cache, ignorer, ignoreCache, provider)
	if err != nil {
		t.Fatal("unable to perform accelerated scan:", err)
	}

	// Verify that the new commit was seen and that both submodules were
	// probed again.
	if link := snapshot.Content.Contents["sub"].Contents[GitLinkName]; link != tGL2 {
		t.Errorf("new commit not seen: %v", link)
	}
	if probes := testingProbedPaths(t, root, provider); !slices.Equal(probes, []string{"sub", "sub/nested"}) {
		t.Errorf("incorrect probes: %v", probes)
	}
	expected, err := Apply(baseline.Content, []*Change{{Path: "sub/.git", New: tGL2}})
	if err != nil {
		t.Fatal("unable to compute expected content:", err)
	} else if !snapshot.Content.Equal(expected, true) {
		t.Errorf("incorrect content: %v != %v", snapshot.Content, expected)
	}

	// Verify that the directories that don't contain submodules were reused,
	// along with the cache entries for their files.
	if snapshot.Content.Contents["other"] != baseline.Content.Contents["other"] {
		t.Error("directory outside of submodule not reused")
	}
	if snapshot.Content.Contents["sub"].Contents["d"] != baseline.Content.Contents["sub"].Contents["d"] {
		t.Error("directory within submodule not reused")
	}
	if !newCache.Equal(cache) {
		t.Error("cache changed")
	}

	// Verify that a baseline without submodules is reused directly if there
	// are no re-check paths, as is a baseline with submodules if there's no
	// provider to examine them.
	plain := &Snapshot{
		Content:                tD1,
		PreservesExecutability: baseline.PreservesExecutability,
		DecomposesUnicode:      baseline.DecomposesUnicode,
	}
	if result, _, _, err := testingScanGitMode(context.Background(), root, plain, cache, ignorer, ignoreCache, provider); err != nil {
		t.Error("unable to perform accelerated scan without submodules:", err)
	} else if result != plain {
		t.Error("baseline without submodules not reused")
	}
	if result, _, _, err := testingScanGitMode(context.Background(), root, baseline, cache, ignorer, ignoreCache, nil); err != nil {
		t.Error("unable to perform accelerated scan without provider:", err)
	} else if result != baseline {
		t.Error("baseline not reused without provider")
	}

	// Verify that a submodule whose repository disappears is noticed, too.
	if err := os.RemoveAll(filepath.Join(root, "sub", "nested", GitLinkName)); err != nil {
		t.Fatal("unable to remove nested repository:", err)
	}
	snapshot, _, _, err = testingScanGitMode(context.Background(), root, snapshot, newCache, ignorer, ignoreCache, provider)
	if err != nil {
		t.Fatal("unable to perform accelerated scan:", err)
	}
	expected, err = Apply(expected, []*Change{{Path: "sub/nested/.git"}})
	if err != nil {
		t.Fatal("unable to compute expected content:", err)
	} else if !snapshot.Content.Equal(expected, true) {
		t.Errorf("incorrect content after repository removal: %v != %v", snapshot.Content, expected)
	}
}

// TestScanGitModeCanonicalGitEntry tests that, if a directory has a Git entry
// with the canonical name and another with different casing, which is only
// possible on a case-sensitive filesystem, then the canonical one is the
// submodule's Git entry, while the other is ignored like any other Git entry.
func TestScanGitModeCanonicalGitEntry(t *testing.T) {
	// Create the content, skipping the test if the filesystem doesn't allow
	// both entries to exist.
	root := t.TempDir()
	testingWriteContent(t, root, map[string]string{"sub/.git/HEAD": "head", "sub/x": tF1Content})
	if _, err := os.Lstat(filepath.Join(root, "sub", ".GIT")); err == nil {
		t.Skip("filesystem is case-insensitive")
	}
	testingWriteContent(t, root, map[string]string{"sub/.GIT/HEAD": "head"})

	// Perform the scan and verify the result.
	provider := &testingGitProvider{links: map[string]*Entry{"sub": tGL0}}
	snapshot, _, _, err := testingScanGitMode(context.Background(), root, nil, nil, &testingGitIgnorer{}, nil, provider)
	if err != nil {
		t.Fatal("unable to scan:", err)
	}
	expected := nested("sub", testingSubmodule(tGL0, map[string]*Entry{".GIT": tU, "x": tF1}))
	if !snapshot.Content.Equal(expected, true) {
		t.Errorf("incorrect content: %v != %v", snapshot.Content, expected)
	}
}

// TestScanGitModeDifferentlyCasedGitEntry tests that a directory whose only Git
// entry has a name other than the canonical one is an ordinary directory if the
// probe fails, as it does on a case-sensitive filesystem, where Git doesn't
// treat the entry as a repository, while a failing probe of a canonical Git
// entry makes the directory problematic.
func TestScanGitModeDifferentlyCasedGitEntry(t *testing.T) {
	root := t.TempDir()
	testingWriteContent(t, root, map[string]string{
		"upper/.GIT/HEAD": "head", "upper/x": tF1Content,
		"lower/.git/HEAD": "head", "lower/x": tF1Content,
	})
	if _, err := os.Lstat(filepath.Join(root, "upper", ".git")); err == nil {
		t.Skip("filesystem is case-insensitive")
	}
	provider := &testingGitProvider{probeErrors: map[string]error{
		"upper": errors.New("not a repository"),
		"lower": errors.New("not a repository"),
	}}
	snapshot, _, _, err := testingScanGitMode(context.Background(), root, nil, nil, &testingGitIgnorer{}, nil, provider)
	if err != nil {
		t.Fatal("unable to scan:", err)
	}
	upper := snapshot.Content.Contents["upper"]
	expectedUpper := &Entry{Kind: EntryKind_Directory, Contents: map[string]*Entry{".GIT": tU, "x": tF1}}
	if !upper.Equal(expectedUpper, true) {
		t.Errorf("incorrect content for differently cased Git entry: %v != %v", upper, expectedUpper)
	}
	if lower := snapshot.Content.Contents["lower"]; lower == nil || lower.Kind != EntryKind_Problematic {
		t.Errorf("directory with failing canonical Git entry not problematic: %v", lower)
	}
}

// TestScanGitModeSymbolicLinkGitEntry tests that a directory whose Git entry is
// a symbolic link is problematic, without being probed, whether the link leads
// to a Git directory or not, and whatever the case of its name, since the
// ignorer doesn't treat such a directory as a repository's top level, whereas
// Git does.
func TestScanGitModeSymbolicLinkGitEntry(t *testing.T) {
	// Create the content, skipping the test if symbolic links aren't supported.
	root := t.TempDir()
	testingWriteContent(t, root, map[string]string{
		"store/.git/HEAD": "head",
		"store/x":         tF1Content,
		"link/x":          tF1Content,
		"dangling/x":      tF1Content,
		"upper/x":         tF1Content,
	})
	links := map[string]string{
		"link/.git":     filepath.Join("..", "store", ".git"),
		"dangling/.git": "nowhere",
		"upper/.GIT":    filepath.Join("..", "store", ".git"),
	}
	for link, target := range links {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Skip("unable to create symbolic link:", err)
		}
	}

	// Perform the scan with a provider that would report each directory as a
	// submodule.
	provider := &testingGitProvider{links: map[string]*Entry{"store": tGL0, "link": tGL0, "dangling": tGL0, "upper": tGL0}}
	snapshot, _, _, err := testingScanGitMode(context.Background(), root, nil, nil, &testingGitIgnorer{}, nil, provider)
	if err != nil {
		t.Fatal("unable to scan:", err)
	}

	// Verify the result.
	for _, directory := range []string{"link", "dangling", "upper"} {
		if entry := snapshot.Content.Contents[directory]; entry == nil || entry.Kind != EntryKind_Problematic {
			t.Errorf("directory with symbolic link Git entry not problematic: %s: %v", directory, entry)
		} else if entry.Problem != gitLinkSymbolicLinkProblem {
			t.Errorf("incorrect problem for %s: %q", directory, entry.Problem)
		}
	}
	if store := snapshot.Content.Contents["store"]; store == nil || store.gitLink() == nil {
		t.Errorf("repository not a submodule: %v", store)
	}
	if probes := testingProbedPaths(t, root, provider); !slices.Equal(probes, []string{"store"}) {
		t.Errorf("incorrect probes: %v", probes)
	}
}
