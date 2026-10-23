package local

import (
	"context"
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
		endpoint, err := NewEndpoint(nil, root, "sync_entry_count", synchronization.Version_Version1, configuration, alpha)
		if err != nil {
			t.Fatal("unable to create endpoint:", err)
		}
		t.Cleanup(func() { endpoint.Shutdown() })
		return endpoint
	}
	return create(sourceRoot, true, 0), create(targetRoot, false, maximumEntryCount)
}

// propagate propagates the content of the source endpoint's root to the target
// endpoint in the same sequence as the controller: it scans both endpoints,
// stages on the target, supplies from the source, and transitions the target.
// Changes are computed for the top-level names whose content differs, which
// stands in for reconciliation with the (flat) hierarchies used in tests. It
// returns any problems reported by the target's transition, or the error
// returned by the target's staging, in which case no transition is performed
// (as in the controller). Any other failure is fatal to the test.
func propagate(t *testing.T, source, target synchronization.Endpoint) ([]*core.Problem, error) {
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
	_, problems, stagerMissingFiles, err := target.Transition(ctx, changes)
	if err != nil {
		t.Fatal("unable to transition target:", err)
	} else if stagerMissingFiles {
		t.Fatal("staged files missing during transition")
	}
	return problems, nil
}

func TestEndpointMaximumEntryCount(t *testing.T) {
	// Each test case limits the target to two entries and propagates the
	// source's content to it. Content updates and replacements at the limit
	// must succeed, while changes that would exceed the limit must leave the
	// target's content intact.
	testCases := []struct {
		// description is a description of the test case.
		description string
		// source is the source's content, as a map from file name to contents.
		source map[string]string
		// target is the target's initial content.
		target map[string]string
		// expectStageError indicates whether or not staging is expected to
		// fail, which it should only do if the staged files alone would exceed
		// the limit.
		expectStageError bool
		// expectProblem indicates whether or not the transition is expected to
		// report that it would exceed the limit.
		expectProblem bool
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
			description:   "file addition over limit",
			source:        map[string]string{"a": "original", "b": "added"},
			target:        map[string]string{"a": "original"},
			expectProblem: true,
			expected:      map[string]string{"a": "original"},
		},
		{
			description: "file removal and addition at limit",
			source:      map[string]string{"b": "added"},
			target:      map[string]string{"a": "original"},
			expected:    map[string]string{"b": "added"},
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
			createRoot := func(files map[string]string) string {
				t.Helper()
				root := t.TempDir()
				for name, contents := range files {
					if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
						t.Fatal("unable to create file:", err)
					}
				}
				return root
			}
			sourceRoot := createRoot(testCase.source)
			targetRoot := createRoot(testCase.target)
			source, target := newEntryCountEndpoints(t, sourceRoot, targetRoot, 2)

			// Propagate the source's content and check the outcome.
			problems, err := propagate(t, source, target)
			if testCase.expectStageError {
				if err == nil {
					t.Error("staging succeeded unexpectedly")
				} else if !strings.Contains(err.Error(), "entry count") {
					t.Error("staging failed for an unexpected reason:", err)
				}
			} else if err != nil {
				t.Fatal("unable to stage files:", err)
			} else if testCase.expectProblem {
				if len(problems) != 1 || !strings.Contains(problems[0].Error, "entry count") {
					t.Errorf("unexpected transition problems: %v", problems)
				}
			} else if len(problems) != 0 {
				t.Errorf("unexpected transition problems: %v", problems)
			}

			// Verify the target's content on disk.
			entries, err := os.ReadDir(targetRoot)
			if err != nil {
				t.Fatal("unable to read target root:", err)
			}
			content := make(map[string]string, len(entries))
			for _, entry := range entries {
				contents, err := os.ReadFile(filepath.Join(targetRoot, entry.Name()))
				if err != nil {
					t.Fatal("unable to read target file:", err)
				}
				content[entry.Name()] = string(contents)
			}
			if !maps.Equal(content, testCase.expected) {
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
