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

// propagate propagates the content of the source endpoint's root to the target
// endpoint in the same sequence as the controller: it scans both endpoints,
// stages on the target, supplies from the source, and transitions the target.
// Changes are computed for the top-level names whose content differs, which
// stands in for reconciliation with the (flat) hierarchies used in tests. If
// beforeTransition is non-nil, it's invoked immediately before the target's
// transition, which allows tests to simulate concurrent modifications. It
// verifies that the transition's results correspond to the changes in order,
// and it returns any problems reported by the transition, or the error returned
// by the target's staging, in which case no transition is performed (as in the
// controller). Any other failure is fatal to the test.
func propagate(t *testing.T, source, target synchronization.Endpoint, beforeTransition func()) ([]*core.Problem, error) {
	t.Helper()
	ctx := context.Background()

	// Scan both endpoints.
	sourceSnapshot, err, _ := source.Scan(ctx, nil, true)
	if err != nil {
		t.Fatal("unable to scan source:", err)
	}
	targetSnapshot, err, _ := target.Scan(ctx, nil, true)
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
			return nil, err
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
			snapshot, err, _ := target.Scan(context.Background(), nil, true)
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
