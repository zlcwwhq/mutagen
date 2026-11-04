package local

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TODO: Implement tests for the remaining endpoint behavior.

// scopeRecords lists the scope registration records (see remote.RegisterScope)
// in the specified Mutagen data directory.
func scopeRecords(t *testing.T, dataDirectory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDirectory, "remote-scopes"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var result []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
			result = append(result, name)
		}
	}
	return result
}

func TestEndpointScopeRegistration(t *testing.T) {
	// Use a short data directory, since registrations create sockets in it,
	// whose paths have tight length limits.
	dataDirectory, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDirectory) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", dataDirectory)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "results"), 0700); err != nil {
		t.Fatal(err)
	}
	synchronizing := func(path string) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sessions, err := remote.Synchronizing(ctx, filepath.Join(root, filepath.FromSlash(path)), false)
		if err != nil {
			t.Fatal("unable to determine synchronizing sessions:", err)
		}
		return sessions
	}

	// Endpoints register their roots and effective ignores, so that remote
	// --pull knows the content that they synchronize, even though the session
	// doesn't run remote commands.
	configuration := &synchronization.Configuration{
		WatchMode: synchronization.WatchMode_WatchModeNoWatch,
		Ignores:   []string{"results"},
	}
	endpoint, err := NewEndpoint(nil, root, "sync_scope", synchronization.Version_Version1, configuration, true)
	if err != nil {
		t.Fatal("unable to create endpoint:", err)
	}
	if records := scopeRecords(t, dataDirectory); len(records) != 1 {
		t.Errorf("incorrect scope records after creation: %v", records)
	} else if sessions := synchronizing("data"); !slices.Equal(sessions, []string{"sync_scope"}) {
		t.Errorf("incorrect sessions synchronizing content in the root: %v", sessions)
	} else if sessions := synchronizing("results/data"); len(sessions) != 0 {
		t.Errorf("ignored content counted as synchronized: %v", sessions)
	}

	// Shutdown removes the registration.
	if err := endpoint.Shutdown(); err != nil {
		t.Fatal("unable to shut down endpoint:", err)
	} else if records := scopeRecords(t, dataDirectory); len(records) != 0 {
		t.Errorf("scope records remain after shutdown: %v", records)
	}
}

// entryCountSession is the session identifier for endpoints created by
// newEntryCountEndpoints.
const entryCountSession = "sync_entry_count"

// newEntryCountEndpoints creates a source endpoint and a target endpoint for
// the specified roots, with the target limited to the specified maximum entry
// count, using a new Mutagen data directory. The endpoints are shut down when
// the test completes.
func newEntryCountEndpoints(t *testing.T, sourceRoot, targetRoot string, maximumEntryCount uint64) (synchronization.Endpoint, synchronization.Endpoint) {
	t.Helper()

	// Use a short data directory, since registrations create sockets in it,
	// whose paths have tight length limits. We create a new one for each test
	// so that caches and staging roots don't carry over between tests.
	dataDirectory, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDirectory) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", dataDirectory)

	// Create the endpoints. A zero maximum entry count leaves the source at the
	// default limit.
	create := func(root string, alpha bool, maximumEntryCount uint64) synchronization.Endpoint {
		t.Helper()
		configuration := &synchronization.Configuration{
			WatchMode:         synchronization.WatchMode_WatchModeNoWatch,
			MaximumEntryCount: maximumEntryCount,
		}
		endpoint, err := NewEndpoint(nil, root, entryCountSession, synchronization.Version_Version1, configuration, alpha)
		if err != nil {
			t.Fatal("unable to create endpoint:", err)
		}
		t.Cleanup(func() { endpoint.Shutdown() })
		return endpoint
	}
	return create(sourceRoot, true, 0), create(targetRoot, false, maximumEntryCount)
}

// writeFiles writes the specified files, as a map from file name to contents,
// to the specified root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatal("unable to write file:", err)
		}
	}
}

// readFiles reads the files in the specified root, returning them as a map from
// file name to contents.
func readFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal("unable to read root:", err)
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		contents, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal("unable to read file:", err)
		}
		files[entry.Name()] = string(contents)
	}
	return files
}

// verifyTargetStagingEmpty verifies that the staging root of the target
// endpoint created by newEntryCountEndpoints doesn't exist or is empty, i.e.
// that no staged files remain after a transition.
func verifyTargetStagingEmpty(t *testing.T) {
	t.Helper()
	stagingRoot, err := pathForMutagenStagingRoot(entryCountSession, false)
	if err != nil {
		t.Fatal("unable to compute staging root:", err)
	}
	entries, err := os.ReadDir(stagingRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal("unable to read staging root:", err)
	} else if len(entries) > 0 {
		t.Errorf("staging root not empty after transition (%d entries)", len(entries))
	}
}

// propagateChanges propagates the content of the source endpoint's root to the
// target endpoint in the same sequence as the controller: it scans both
// endpoints, stages on the target, supplies from the source, and transitions
// the target. Changes are computed for the top-level names whose content
// differs, which stands in for reconciliation with the (flat) hierarchies used
// in tests. If beforeTransition is non-nil, it's invoked immediately before the
// target's transition, which allows tests to simulate concurrent modifications.
// It returns the changes, the transition's results, and any problems reported
// by the transition, or the error returned by the target's staging, in which
// case no transition is performed (as in the controller). Any other failure is
// fatal to the test.
func propagateChanges(t *testing.T, source, target synchronization.Endpoint, beforeTransition func()) ([]*core.Change, []*core.Entry, []*core.Problem, error) {
	t.Helper()
	ctx := context.Background()

	// Scan both endpoints.
	sourceSnapshot, err, _ := source.Scan(ctx, nil, nil, true)
	if err != nil {
		t.Fatal("unable to scan source:", err)
	}
	targetSnapshot, err, _ := target.Scan(ctx, nil, nil, true)
	if err != nil {
		t.Fatal("unable to scan target:", err)
	}

	// Compute the changes that bring the target's content in line with the
	// source's content.
	sourceContents := sourceSnapshot.Content.GetContents()
	targetContents := targetSnapshot.Content.GetContents()
	names := make(map[string]bool, len(sourceContents)+len(targetContents))
	for name := range sourceContents {
		names[name] = true
	}
	for name := range targetContents {
		names[name] = true
	}
	var changes []*core.Change
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if oldEntry, newEntry := targetContents[name], sourceContents[name]; !oldEntry.Equal(newEntry, true) {
			changes = append(changes, &core.Change{Path: name, Old: oldEntry, New: newEntry})
		}
	}

	// Stage files on the target, supplying them from the source.
	if paths, digests := core.TransitionDependencies(changes); len(paths) > 0 {
		filteredPaths, signatures, receiver, err := target.Stage(paths, digests)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(filteredPaths) > 0 {
			if err := source.Supply(filteredPaths, signatures, receiver); err != nil {
				t.Fatal("unable to supply files:", err)
			}
		}
	}

	// Transition the target.
	if beforeTransition != nil {
		beforeTransition()
	}
	results, problems, stagerMissingFiles, err := target.Transition(ctx, changes)
	if err != nil {
		t.Fatal("unable to transition target:", err)
	} else if stagerMissingFiles {
		t.Fatal("staged files missing during transition")
	} else if len(results) != len(changes) {
		t.Fatalf("transition result count (%d) doesn't match change count (%d)", len(results), len(changes))
	}
	return changes, results, problems, nil
}

// propagate performs propagateChanges, verifies that the transition's results
// correspond to the changes in order, and returns any problems reported by the
// transition, or the error returned by the target's staging.
func propagate(t *testing.T, source, target synchronization.Endpoint, beforeTransition func()) ([]*core.Problem, error) {
	t.Helper()
	changes, results, problems, err := propagateChanges(t, source, target, beforeTransition)
	if err != nil {
		return nil, err
	}

	// Verify that the results line up with the changes, i.e. that each result
	// is either the old or new content at the change's path, as appropriate.
	// Paths with problems should retain their old content, while all others
	// should have their new content.
	problemPaths := make(map[string]bool, len(problems))
	for _, problem := range problems {
		problemPaths[problem.Path] = true
	}
	for c, change := range changes {
		expected := change.New
		if problemPaths[change.Path] {
			expected = change.Old
		}
		if !results[c].Equal(expected, true) {
			t.Errorf("incorrect transition result at %q", change.Path)
		}
	}
	return problems, nil
}

func TestEndpointMaximumEntryCount(t *testing.T) {
	// Each test case limits the target to two entries and propagates the
	// source's content to it. Content updates and replacements at the limit
	// must succeed, while changes that would exceed the limit must be refused
	// individually and leave the target's content at their paths intact.
	testCases := []struct {
		// description is a description of the test case.
		description string
		// source is the source's content, as a map from file name to contents.
		source map[string]string
		// target is the target's initial content.
		target map[string]string
		// modify is content written to the target after it's scanned and
		// before it's transitioned, which simulates concurrent modifications.
		modify map[string]string
		// expectStageError indicates whether or not staging is expected to
		// fail, which it should only do if the staged files alone would exceed
		// the limit.
		expectStageError bool
		// expectedProblems maps the paths at which the transition is expected
		// to report problems to a substring of the expected problem.
		expectedProblems map[string]string
		// expected is the target's expected content after propagation.
		expected map[string]string
	}{
		{
			description: "file update at limit",
			source:      map[string]string{"a": "updated"},
			target:      map[string]string{"a": "original"},
			expected:    map[string]string{"a": "updated"},
		},
		{
			description: "file addition at limit",
			source:      map[string]string{"a": "added"},
			target:      map[string]string{},
			expected:    map[string]string{"a": "added"},
		},
		{
			description:      "file addition over limit",
			source:           map[string]string{"a": "original", "b": "added"},
			target:           map[string]string{"a": "original"},
			expectedProblems: map[string]string{"b": "entry count"},
			expected:         map[string]string{"a": "original"},
		},
		{
			description:      "file additions partially over limit",
			source:           map[string]string{"a": "first", "b": "second"},
			target:           map[string]string{},
			expectedProblems: map[string]string{"b": "entry count"},
			expected:         map[string]string{"a": "first"},
		},
		{
			description:      "file update and addition over limit",
			source:           map[string]string{"a": "updated", "b": "added"},
			target:           map[string]string{"a": "original"},
			expectedProblems: map[string]string{"b": "entry count"},
			expected:         map[string]string{"a": "updated"},
		},
		{
			description: "file removal and addition at limit",
			source:      map[string]string{"b": "added"},
			target:      map[string]string{"a": "original"},
			expected:    map[string]string{"b": "added"},
		},
		{
			description:      "file removal and additions over limit",
			source:           map[string]string{"b": "first", "c": "second"},
			target:           map[string]string{"a": "original"},
			expectedProblems: map[string]string{"c": "entry count"},
			expected:         map[string]string{"b": "first"},
		},
		{
			description: "refused file removal and addition at limit",
			source:      map[string]string{"b": "added"},
			target:      map[string]string{"a": "original"},
			modify:      map[string]string{"a": "modified after scan"},
			expectedProblems: map[string]string{
				"a": "modification detected",
				"b": "entry count",
			},
			expected: map[string]string{"a": "modified after scan"},
		},
		{
			description:      "staging more files than limit",
			source:           map[string]string{"a": "first", "b": "second", "c": "third"},
			target:           map[string]string{},
			expectStageError: true,
			expected:         map[string]string{},
		},
	}

	// Process test cases.
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			// Create the roots and the endpoints.
			sourceRoot, targetRoot := t.TempDir(), t.TempDir()
			writeFiles(t, sourceRoot, testCase.source)
			writeFiles(t, targetRoot, testCase.target)
			source, target := newEntryCountEndpoints(t, sourceRoot, targetRoot, 2)

			// Propagate the source's content, applying any modifications to the
			// target before its transition, and check the outcome.
			var beforeTransition func()
			if testCase.modify != nil {
				beforeTransition = func() { writeFiles(t, targetRoot, testCase.modify) }
			}
			problems, err := propagate(t, source, target, beforeTransition)
			if testCase.expectStageError {
				if err == nil {
					t.Error("staging succeeded unexpectedly")
				} else if !strings.Contains(err.Error(), "entry count") {
					t.Error("staging failed for an unexpected reason:", err)
				}
			} else if err != nil {
				t.Fatal("unable to stage files:", err)
			} else {
				reported := make(map[string]string, len(problems))
				for _, problem := range problems {
					reported[problem.Path] = problem.Error
				}
				valid := len(reported) == len(problems) &&
					len(reported) == len(testCase.expectedProblems)
				for path, expected := range testCase.expectedProblems {
					valid = valid && strings.Contains(reported[path], expected)
				}
				if !valid {
					t.Errorf("unexpected transition problems: %v", problems)
				}
			}

			// Verify that no staged files remain, including those for changes
			// that were refused.
			verifyTargetStagingEmpty(t)

			// Verify the target's content on disk.
			if content := readFiles(t, targetRoot); !maps.Equal(content, testCase.expected) {
				t.Errorf("incorrect target content: %v != %v", content, testCase.expected)
			}

			// Verify that the target scans successfully with the expected entry
			// count (the root and its files).
			snapshot, err, _ := target.Scan(context.Background(), nil, nil, true)
			if err != nil {
				t.Fatal("unable to rescan target:", err)
			} else if count, expected := snapshot.Content.Count(), uint64(1+len(testCase.expected)); count != expected {
				t.Errorf("incorrect target entry count: %d != %d", count, expected)
			}
		})
	}
}

func TestEndpointMaximumEntryCountRepeatedRefusal(t *testing.T) {
	// Create a source and a target with the same file, limiting the target to
	// two entries, so that adding a second file to the source is refused.
	sourceRoot, targetRoot := t.TempDir(), t.TempDir()
	writeFiles(t, sourceRoot, map[string]string{"a": "original"})
	writeFiles(t, targetRoot, map[string]string{"a": "original"})
	source, target := newEntryCountEndpoints(t, sourceRoot, targetRoot, 2)

	// Repeatedly change the second file on the source and propagate it. Each
	// version is staged on the target before being refused, and none of them
	// should remain in staging afterward.
	//
	// The versions have the same size and are written in place, so they keep
	// the same file ID, and with coarse timestamps (e.g. on Linux without
	// multigrain timestamps), consecutive writes can receive the same
	// modification time. Scans rehash such recently modified files rather than
	// reusing their racy digests (see core.CacheEntry.Racy), but this test is
	// about entry count refusals rather than racy digests (which core tests
	// cover), so we give each version a distinct modification time in the
	// past, independently of timing. We use whole-second steps, which
	// filesystems that store coarser timestamps still keep distinct.
	modificationTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 3; i++ {
		version := fmt.Sprintf("version %d", i)
		writeFiles(t, sourceRoot, map[string]string{"b": version})
		modificationTime = modificationTime.Add(time.Second)
		if err := os.Chtimes(filepath.Join(sourceRoot, "b"), modificationTime, modificationTime); err != nil {
			t.Fatal("unable to set modification time:", err)
		}
		problems, err := propagate(t, source, target, nil)
		if err != nil {
			t.Fatal("unable to stage files:", err)
		} else if len(problems) != 1 || problems[0].Path != "b" || !strings.Contains(problems[0].Error, "entry count") {
			t.Fatalf("unexpected transition problems for %s: %v", version, problems)
		}
		verifyTargetStagingEmpty(t)
	}

	// Remove the first file from the source and verify that the replacement
	// of the first file with the latest version of the second, which had been
	// refused, is staged again and applied.
	if err := os.Remove(filepath.Join(sourceRoot, "a")); err != nil {
		t.Fatal("unable to remove file:", err)
	}
	if problems, err := propagate(t, source, target, nil); err != nil {
		t.Fatal("unable to stage files:", err)
	} else if len(problems) != 0 {
		t.Fatalf("unexpected transition problems: %v", problems)
	}
	verifyTargetStagingEmpty(t)
	expected := map[string]string{"b": "version 2"}
	if content := readFiles(t, targetRoot); !maps.Equal(content, expected) {
		t.Errorf("incorrect target content: %v != %v", content, expected)
	}
}

// gitModeSession is the session identifier for endpoints created by
// newGitModeEndpoint and newConfiguredGitModeEndpoint.
const gitModeSession = "sync_git_mode"

// newGitModeEndpoint creates an endpoint in Git mode, using a new Mutagen data
// directory, by applying options to its configuration. The endpoint is shut
// down when the test completes.
func newGitModeEndpoint(t *testing.T, root string, watchMode synchronization.WatchMode, ignores ...string) *endpoint {
	t.Helper()
	return newConfiguredGitModeEndpoint(t, root, &synchronization.Configuration{
		WatchMode:            watchMode,
		WatchPollingInterval: 1,
		Ignores:              ignores,
	})
}

// newConfiguredGitModeEndpoint creates an endpoint with the specified
// configuration, with Git mode enabled, using a new Mutagen data directory. The
// endpoint is shut down when the test completes.
func newConfiguredGitModeEndpoint(t *testing.T, root string, configuration *synchronization.Configuration) *endpoint {
	t.Helper()

	// Use a short data directory, since registrations create sockets in it,
	// whose paths have tight length limits.
	dataDirectory, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDirectory) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", dataDirectory)

	// Create the endpoint.
	configuration.GitMode = synchronization.GitMode_GitModeEnabled
	result, err := NewEndpoint(nil, root, gitModeSession, synchronization.Version_Version1, configuration, true)
	if err != nil {
		t.Fatal("unable to create endpoint:", err)
	}
	t.Cleanup(func() { result.Shutdown() })
	return result.(*endpoint)
}

// writeTree writes files with the specified relative paths and contents,
// creating parent directories as necessary. Each file is created outside of the
// root and renamed into place, so that a concurrent scan can't see it partially
// written.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal("unable to create parent directory:", err)
		}
		temporary, err := os.CreateTemp(filepath.Dir(root), "write-")
		if err != nil {
			t.Fatal("unable to create temporary file:", err)
		}
		_, err = temporary.WriteString(contents)
		temporary.Close()
		if err != nil {
			t.Fatal("unable to write temporary file:", err)
		} else if err := os.Rename(temporary.Name(), path); err != nil {
			t.Fatal("unable to move file into place:", err)
		}
	}
}

// entryAt returns the entry at the specified path in a snapshot, or nil if
// there isn't one.
func entryAt(snapshot *core.Snapshot, path string) *core.Entry {
	entry := snapshot.Content
	for _, component := range strings.Split(path, "/") {
		if entry == nil {
			return nil
		}
		entry = entry.Contents[component]
	}
	return entry
}

// verifyKinds verifies the kinds of the entries at the specified paths.
func verifyKinds(t *testing.T, snapshot *core.Snapshot, expected map[string]core.EntryKind) {
	t.Helper()
	for path, kind := range expected {
		if entry := entryAt(snapshot, path); entry == nil {
			t.Errorf("no entry at %q", path)
		} else if entry.Kind != kind {
			t.Errorf("incorrect kind at %q: %s != %s", path, entry.Kind, kind)
		}
	}
}

// scanEndpoint performs a scan that must succeed.
func scanEndpoint(t *testing.T, e *endpoint, full bool) *core.Snapshot {
	t.Helper()
	snapshot, err, _ := e.Scan(context.Background(), nil, nil, full)
	if err != nil {
		t.Fatal("scan failed:", err)
	}
	return snapshot
}

// TestEndpointGitModePollRuleChange tests that poll-based watching detects
// changes to ignore rules, which requires that rule files be read again and
// that decisions not be cached, even though nothing else on disk has changed.
func TestEndpointGitModePollRuleChange(t *testing.T) {
	// Create content and an endpoint, then wait for its baseline scan so that
	// it treats what happens next as modifications.
	root := filepath.Join(t.TempDir(), "root")
	writeTree(t, root, map[string]string{
		"kept":           "kept",
		"data/file":      "data",
		"deep/sub/x.log": "log",
	})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeForcePoll)
	deadline := time.Now().Add(30 * time.Second)
	for {
		e.lockScanLock(context.Background())
		accelerate := e.accelerate
		e.unlockScanLock()
		if accelerate {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("timed out waiting for baseline scan")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// waitForModification waits for the watching Goroutine to signal changes.
	waitForModification := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.Poll(ctx); err != nil {
			t.Fatal("polling failed:", err)
		} else if ctx.Err() != nil {
			t.Fatal("timed out waiting for modifications to be detected")
		}
	}

	// Verify the initial state, using the snapshot from the watching Goroutine.
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		"kept":           core.EntryKind_File,
		"data":           core.EntryKind_Directory,
		"data/file":      core.EntryKind_File,
		"deep/sub/x.log": core.EntryKind_File,
	})

	// Add rules.
	writeTree(t, root, map[string]string{".gitignore": "data/\n*.log\n"})
	waitForModification()
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		"kept":           core.EntryKind_File,
		".gitignore":     core.EntryKind_File,
		"data":           core.EntryKind_Untracked,
		"deep/sub":       core.EntryKind_Directory,
		"deep/sub/x.log": core.EntryKind_Untracked,
	})

	// Change the rules.
	writeTree(t, root, map[string]string{".gitignore": "data/\n"})
	waitForModification()
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		"data":           core.EntryKind_Untracked,
		"deep/sub/x.log": core.EntryKind_File,
	})

	// Remove the rules.
	if err := os.Remove(filepath.Join(root, ".gitignore")); err != nil {
		t.Fatal("unable to remove rules:", err)
	}
	waitForModification()
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		"data/file":      core.EntryKind_File,
		"deep/sub/x.log": core.EntryKind_File,
	})

	// Add rules using the repository's exclude file, which isn't synchronized,
	// and thus doesn't change the snapshot except for what it ignores.
	writeTree(t, root, map[string]string{".git/info/exclude": "kept\n"})
	waitForModification()
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		".git": core.EntryKind_Untracked,
		"kept": core.EntryKind_Untracked,
	})

	// Decisions must never have been cached.
	e.lockScanLock(context.Background())
	ignoreCache := e.ignoreCache
	e.unlockScanLock()
	if ignoreCache != nil {
		t.Error("ignore cache stored in Git mode")
	}
}

// TestEndpointGitModeFullScanRuleChange tests that full scans, which are what
// flushes use, detect changes to ignore rules.
func TestEndpointGitModeFullScanRuleChange(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	writeTree(t, root, map[string]string{"kept": "kept", "data/file": "data"})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	verifyKinds(t, scanEndpoint(t, e, true), map[string]core.EntryKind{"data/file": core.EntryKind_File})

	writeTree(t, root, map[string]string{".gitignore": "data/\n"})
	verifyKinds(t, scanEndpoint(t, e, true), map[string]core.EntryKind{"data": core.EntryKind_Untracked})

	writeTree(t, root, map[string]string{".gitignore": "kept\n"})
	verifyKinds(t, scanEndpoint(t, e, true), map[string]core.EntryKind{
		"data/file": core.EntryKind_File,
		"kept":      core.EntryKind_Untracked,
	})
}

// TestEndpointGitModeAcceleratedRuleChange tests that scans accelerated by
// recursive watching discard the baseline, and with it all of the decisions
// that it reflects, if a rule file is among the re-check paths. This test
// simulates recursive watching, which isn't available on all platforms.
func TestEndpointGitModeAcceleratedRuleChange(t *testing.T) {
	// Create an endpoint with a baseline for accelerated scanning.
	root := filepath.Join(t.TempDir(), "root")
	writeTree(t, root, map[string]string{
		"other/a":        "a",
		"deep/sub/x.log": "log",
		"deep/sub/y":     "y",
	})
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	scanEndpoint(t, e, true)
	e.watchMode = reifiedWatchModeRecursive
	e.accelerate = true

	// Verify that an unrelated re-check path doesn't cause directories away
	// from it to be scanned, so a change to rules wouldn't be seen by a regular
	// accelerated scan. This means that the remainder of the test is meaningful
	// and also that propagation from the baseline tolerates the lack of an
	// ignore cache.
	writeTree(t, root, map[string]string{".gitignore": "*.log\n"})
	e.recheckPaths = map[string]bool{"other/a": true}
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		".gitignore":     core.EntryKind_File,
		"deep/sub/x.log": core.EntryKind_File,
	})
	if len(e.recheckPaths) != 0 {
		t.Error("re-check paths not reset by accelerated scan")
	}

	// Verify the same if there aren't any re-check paths.
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{"deep/sub/x.log": core.EntryKind_File})

	// Verify that a root rule file forces a complete re-evaluation, and that
	// re-check paths are reset afterward.
	e.recheckPaths = map[string]bool{".gitignore": true}
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		"deep/sub/x.log": core.EntryKind_Untracked,
		"deep/sub/y":     core.EntryKind_File,
	})
	if len(e.recheckPaths) != 0 {
		t.Error("re-check paths not reset by full scan")
	}

	// Verify the same for a nested rule file, which is mixed with other paths.
	writeTree(t, root, map[string]string{"deep/.gitignore": "y\n"})
	e.recheckPaths = map[string]bool{"deep/.gitignore": true, "other/a": true}
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{"deep/sub/y": core.EntryKind_Untracked})
	if len(e.recheckPaths) != 0 {
		t.Error("re-check paths not reset by full scan")
	}

	// Verify the same for the repository's exclude file.
	writeTree(t, root, map[string]string{".git/info/exclude": "a\n"})
	e.recheckPaths = map[string]bool{".git/info/exclude": true}
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{"other/a": core.EntryKind_Untracked})

	// Verify that all of this occurred without a stored ignore cache.
	if e.ignoreCache != nil {
		t.Error("ignore cache stored in Git mode")
	}
}

// newLinkedWorktree creates a repository with a single commit in a new
// directory within a base directory, and a linked worktree of it at the
// specified path, whose exclude file is thus the repository's.
func newLinkedWorktree(t *testing.T, base, name, worktree string) string {
	t.Helper()
	repository := newGitWithOrigin(t, base, name, "")
	mustGit(t, repository, "worktree", "add", "--quiet", worktree)
	return repository
}

// TestEndpointGitModeAcceleratedOutsideExclude tests that scans accelerated by
// recursive watching discard the baseline if the exclude file of a repository
// whose Git directory is outside of the root changes, which the watcher can't
// report. That's the case for a root that's a linked worktree, and for a nested
// repository that's a linked worktree, which may only be found by an
// accelerated scan. Otherwise, a file that the exclude file now excludes would
// remain in snapshots, and its deletion on the other endpoint would remove it.
// This test simulates recursive watching, which isn't available on all
// platforms.
func TestEndpointGitModeAcceleratedOutsideExclude(t *testing.T) {
	// Create a root that's a linked worktree of a repository elsewhere.
	isolateGit(t)
	base := realTemporaryDirectory(t)
	root := filepath.Join(base, "root")
	repository := newLinkedWorktree(t, base, "repository", root)
	writeTree(t, root, map[string]string{"local": "local", "other/a": "a"})

	// Create an endpoint with a baseline for accelerated scanning.
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	verifyKinds(t, scanEndpoint(t, e, true), map[string]core.EntryKind{"local": core.EntryKind_File})
	e.watchMode = reifiedWatchModeRecursive
	e.accelerate = true
	e.recheckPaths = make(map[string]bool)

	// Verify that an accelerated scan still reuses the baseline if the exclude
	// file hasn't changed, so a change to rules within the root that isn't
	// among the re-check paths isn't seen.
	writeTree(t, root, map[string]string{".gitignore": "a\n"})
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{"other/a": core.EntryKind_File})

	// Verify that a change to the exclude file forces a full scan.
	writeTree(t, repository, map[string]string{".git/info/exclude": "local\n"})
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{
		"local":   core.EntryKind_Untracked,
		"other/a": core.EntryKind_Untracked,
	})

	// Create a nested repository that's a linked worktree of another
	// repository, which an accelerated scan finds because of a re-check path.
	nested := newLinkedWorktree(t, base, "nested", filepath.Join(root, "sub"))
	e.recheckPaths = map[string]bool{"sub": true}
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{"sub/file": core.EntryKind_File})

	// Verify that a change to its exclude file forces a full scan as well.
	writeTree(t, nested, map[string]string{".git/info/exclude": "file\n"})
	verifyKinds(t, scanEndpoint(t, e, false), map[string]core.EntryKind{"sub/file": core.EntryKind_Untracked})
}

// TestEndpointNoGitModeRetainsIgnoreCache tests that the ignore cache continues
// to be used without Git mode, in which rule files aren't read.
func TestEndpointNoGitModeRetainsIgnoreCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	writeTree(t, root, map[string]string{"data/file": "data"})
	dataDirectory, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDirectory) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", dataDirectory)
	configuration := &synchronization.Configuration{WatchMode: synchronization.WatchMode_WatchModeNoWatch}
	result, err := NewEndpoint(nil, root, "sync_no_git_mode", synchronization.Version_Version1, configuration, true)
	if err != nil {
		t.Fatal("unable to create endpoint:", err)
	}
	t.Cleanup(func() { result.Shutdown() })
	e := result.(*endpoint)

	// Rule files are ordinary content, and nothing is attached to the snapshot.
	writeTree(t, root, map[string]string{".gitignore": "data/\n"})
	snapshot := scanEndpoint(t, e, true)
	verifyKinds(t, snapshot, map[string]core.EntryKind{
		".gitignore": core.EntryKind_File,
		"data/file":  core.EntryKind_File,
	})
	if e.ignoreCache == nil {
		t.Error("ignore cache not stored")
	}
	if snapshot != e.snapshot {
		t.Error("snapshot copied without Git mode")
	}
}
