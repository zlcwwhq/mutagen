package core

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	dockerignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/docker"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

// testingGitProbe is the result of a probe by testingGitProvider.
type testingGitProbe struct {
	// commit is the commit.
	commit []byte
	// url is the URL.
	url string
	// err is the error.
	err error
}

// testingGitProvider is a core.GitProvider for tests that operates on plain
// directories, without involving Git. Paths in its fields are relative to root.
type testingGitProvider struct {
	// root is the synchronization root.
	root string
	// probes are the results of probes by path. Paths that aren't listed result
	// in the test's failure.
	probes map[string]testingGitProbe
	// heads are the commits reported by Head by path.
	heads map[string][]byte
	// cloneErr is the error returned by Clone.
	cloneErr error
	// updateErr is the error returned by Update.
	updateErr error
	// nestedErr is the error returned by UpdateNested.
	nestedErr error
	// onClone is an optional callback invoked by Clone once the clone has been
	// populated, before cloneErr is returned, with the clone's path.
	onClone func(directory string)
	// probed are the paths that have been probed, in order.
	probed []string
	// cloned are the absolute paths in which clones have been performed.
	cloned []string
	// updated are the paths that have been updated.
	updated []string
	// nested are the paths and clone paths whose nested submodules were
	// updated.
	nested []string
	// t is the test.
	t *testing.T
}

// relative computes the path of a directory relative to the root.
func (p *testingGitProvider) relative(directory string) string {
	relative, err := filepath.Rel(p.root, directory)
	if err != nil {
		p.t.Error("unable to compute relative path:", err)
	}
	return filepath.ToSlash(relative)
}

// Probe implements GitProvider.Probe.
func (p *testingGitProvider) Probe(ctx context.Context, directory string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	path := p.relative(directory)
	p.probed = append(p.probed, path)
	result, ok := p.probes[path]
	if !ok {
		p.t.Error("unexpected probe of", path)
	}
	return result.commit, result.url, result.err
}

// Head implements GitProvider.Head.
func (p *testingGitProvider) Head(_ context.Context, directory string) ([]byte, error) {
	head, ok := p.heads[p.relative(directory)]
	if !ok {
		return nil, errors.New("no head")
	}
	return head, nil
}

// Clone implements GitProvider.Clone.
func (p *testingGitProvider) Clone(_ context.Context, directory, _ string, _ []byte) error {
	p.cloned = append(p.cloned, directory)
	if err := os.Mkdir(filepath.Join(directory, ".git"), 0700); err != nil {
		p.t.Error("unable to create clone:", err)
	} else if err := os.WriteFile(filepath.Join(directory, "cloned"), []byte("cloned"), 0600); err != nil {
		p.t.Error("unable to populate clone:", err)
	}
	if p.onClone != nil {
		p.onClone(directory)
	}
	return p.cloneErr
}

// Update implements GitProvider.Update.
func (p *testingGitProvider) Update(_ context.Context, directory, _ string, _ []byte) error {
	p.updated = append(p.updated, p.relative(directory))
	return p.updateErr
}

// UpdateNested implements GitProvider.UpdateNested.
func (p *testingGitProvider) UpdateNested(_ context.Context, directory, _ string) error {
	p.nested = append(p.nested, directory)
	return p.nestedErr
}

// testingGitScan performs a scan in Git mode, using the same rules as sessions
// unless ignorer is specified, using no ignore cache.
func testingGitScan(
	t *testing.T,
	ctx context.Context,
	root string,
	baseline *Snapshot,
	recheckPaths map[string]bool,
	cache *Cache,
	ignorer ignore.Ignorer,
	provider GitProvider,
) (*Snapshot, *Cache, error) {
	t.Helper()
	if ignorer == nil {
		ignorer = gitignore.NewIgnorer(root, nil)
	}
	snapshot, cache, _, err := ScanGitMode(
		ctx,
		root,
		baseline, recheckPaths,
		newTestingHasher(), cache,
		ignorer, nil,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
		provider,
	)
	return snapshot, cache, err
}

// testingGitTree creates a root containing the specified files, which have
// slash-separated paths relative to the root, and directories, which are the
// paths ending with a slash.
func testingGitTree(t *testing.T, paths ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		target := filepath.Join(root, filepath.FromSlash(path))
		if strings.HasSuffix(path, "/") {
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(target, []byte(tF1Content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestScanGitModeDetection tests how directories with entries named .git are
// represented, and that nothing else is examined.
func TestScanGitModeDetection(t *testing.T) {
	commit := []byte("first commit")
	root := testingGitTree(t,
		".git/config",
		"plain/file",
		"plain/deep/file",
		"submodule/.git/config",
		"submodule/file",
		"plain/deep/gitfile/.git",
		"plain/deep/gitfile/file",
		"uppercase/.GIT/config",
		"sourceless/.git/config",
		"sourceless/file",
		"invalid/.git",
		"invalid/file",
		"file-named.git",
		"not.git/file",
	)
	provider := &testingGitProvider{root: root, t: t, probes: map[string]testingGitProbe{
		"submodule":          {commit: commit, url: "https://example.org/submodule"},
		"plain/deep/gitfile": {commit: commit, url: "https://example.org/gitfile"},
		"uppercase":          {commit: commit, url: "https://example.org/uppercase"},
		"sourceless":         {},
		"invalid":            {err: errors.New("remove the directory")},
	}}
	snapshot, cache, err := testingGitScan(t, context.Background(), root, nil, nil, nil, nil, provider)
	if err != nil {
		t.Fatal("scan failed:", err)
	}
	if err := snapshot.EnsureValid(); err != nil {
		t.Error("invalid snapshot:", err)
	}

	// Verify which directories were probed. The root must not be, because its
	// repository is what's being synchronized.
	probed := slices.Sorted(slices.Values(provider.probed))
	expectedProbes := slices.Sorted(maps.Keys(provider.probes))
	if !slices.Equal(probed, expectedProbes) {
		t.Errorf("incorrect probes: %v != %v", probed, expectedProbes)
	}

	// Verify the entries.
	contents := snapshot.Content.Contents
	if contents[".git"].Kind != EntryKind_Untracked {
		t.Error("root's .git isn't untracked:", contents[".git"])
	}
	for _, submodule := range []struct {
		entry *Entry
		url   string
	}{
		{contents["submodule"], "https://example.org/submodule"},
		{contents["plain"].Contents["deep"].Contents["gitfile"], "https://example.org/gitfile"},
		{contents["uppercase"], "https://example.org/uppercase"},
	} {
		if submodule.entry.Kind != EntryKind_GitLink || !bytes.Equal(submodule.entry.Digest, commit) || submodule.entry.Url != submodule.url {
			t.Errorf("incorrect submodule entry: %v", submodule.entry)
		} else if submodule.entry.Contents != nil {
			t.Error("submodule entry has contents:", submodule.entry)
		}
	}

	// A repository without an origin or commits is an ordinary directory, whose
	// .git is excluded by the ignorer.
	if sourceless := contents["sourceless"]; sourceless.Kind != EntryKind_Directory {
		t.Error("sourceless repository isn't a directory:", sourceless)
	} else if sourceless.Contents[".git"].Kind != EntryKind_Untracked {
		t.Error("sourceless repository's .git isn't untracked:", sourceless.Contents[".git"])
	} else if sourceless.Contents["file"].Kind != EntryKind_File {
		t.Error("sourceless repository's content isn't scanned:", sourceless.Contents["file"])
	}

	// Any other failure is a problem that carries the provider's message, and
	// it doesn't tell us anything about the directory's contents.
	if invalid := contents["invalid"]; invalid.Kind != EntryKind_Problematic || invalid.Problem != "remove the directory" {
		t.Error("incorrect entry for invalid repository:", invalid)
	}

	// Verify that what doesn't have an entry named .git is scanned as usual,
	// even if it resembles that name.
	if contents["file-named.git"].Kind != EntryKind_File || contents["not.git"].Kind != EntryKind_Directory {
		t.Error("incorrect entries for similar names:", contents["file-named.git"], contents["not.git"])
	}

	// Verify that submodules don't count towards statistics, and neither do
	// their contents. We have directories for the root, plain, plain/deep,
	// sourceless, and not.git, with files in plain, plain/deep, sourceless, the
	// root, and not.git.
	if snapshot.Directories != 5 {
		t.Error("incorrect directory count:", snapshot.Directories)
	}
	if snapshot.Files != 5 {
		t.Error("incorrect file count:", snapshot.Files)
	}
	if snapshot.SymbolicLinks != 0 {
		t.Error("incorrect symbolic link count:", snapshot.SymbolicLinks)
	}
	if _, ok := cache.Entries["submodule/file"]; ok {
		t.Error("submodule content in cache")
	}
}

// TestScanWithoutGitProvider tests that a scan that isn't in Git mode treats
// directories containing entries named .git like any other directory.
func TestScanWithoutGitProvider(t *testing.T) {
	root := testingGitTree(t, "submodule/.git/config", "submodule/file")
	ignorer, err := dockerignore.NewIgnorer(nil)
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatal("scan failed:", err)
	}
	submodule := snapshot.Content.Contents["submodule"]
	if submodule.Kind != EntryKind_Directory || submodule.Contents[".git"].Kind != EntryKind_Directory {
		t.Error("directory with .git entry treated specially:", submodule)
	}
}

// TestScanGitModeIgnoreMask tests that a directory containing a .git entry
// that's only being traversed because of an ignore mask isn't probed, and that
// it can't reify its phantom parents by being a submodule.
func TestScanGitModeIgnoreMask(t *testing.T) {
	root := testingGitTree(t,
		"ignored/sub/.git/config",
		"ignored/sub/keep/file",
		"ignored/other/file",
	)

	// Create an ignorer that ignores everything, but that has to traverse
	// directories in order to look for what's unignored.
	ignorer, err := dockerignore.NewIgnorer([]string{"ignored", "!ignored/sub/keep"})
	if err != nil {
		t.Fatal(err)
	}

	// Perform the scan with a provider that fails tests that use it.
	provider := &testingGitProvider{root: root, t: t}
	snapshot, _, err := testingGitScan(t, context.Background(), root, nil, nil, nil, ignorer, provider)
	if err != nil {
		t.Fatal("scan failed:", err)
	}
	if len(provider.probed) > 0 {
		t.Error("masked directory probed:", provider.probed)
	}

	// Verify that it's traversal was as expected and that the directory
	// containing the .git entry is untracked.
	ignored := snapshot.Content.Contents["ignored"]
	if ignored == nil || ignored.Kind != EntryKind_PhantomDirectory {
		t.Fatal("incorrect entry for ignored directory:", ignored)
	} else if sub := ignored.Contents["sub"]; sub == nil || sub.Kind != EntryKind_Untracked {
		t.Error("incorrect entry for masked directory:", sub)
	}

	// Verify that this is due to the mask, because the same directory is
	// otherwise probed, unlike the one that remains masked.
	unmasked, err := dockerignore.NewIgnorer(nil)
	if err != nil {
		t.Fatal(err)
	}
	provider.probes = map[string]testingGitProbe{"ignored/sub": {commit: []byte("commit"), url: "https://example.org"}}
	snapshot, _, err = testingGitScan(t, context.Background(), root, nil, nil, nil, unmasked, provider)
	if err != nil {
		t.Fatal("scan failed:", err)
	}
	if !slices.Equal(provider.probed, []string{"ignored/sub"}) {
		t.Error("unmasked directory not probed:", provider.probed)
	}
	if sub := snapshot.Content.Contents["ignored"].Contents["sub"]; sub.Kind != EntryKind_GitLink {
		t.Error("incorrect entry for unmasked directory:", sub)
	}
}

// TestScanGitModeBaseline tests that submodules are always examined again in
// the presence of a baseline, even if nothing suggests that they've changed,
// because a submodule's HEAD can change without any filesystem events at its
// path.
func TestScanGitModeBaseline(t *testing.T) {
	first, second := []byte("first commit"), []byte("second commit")
	root := testingGitTree(t,
		"file",
		"unrelated/deep/file",
		"parent/sibling/file",
		"parent/sub/.git/config",
		"parent/sub/file",
	)
	provider := &testingGitProvider{root: root, t: t, probes: map[string]testingGitProbe{
		"parent/sub": {commit: first, url: "https://example.org/sub"},
	}}

	// Perform a baseline scan.
	ctx := context.Background()
	baseline, cache, err := testingGitScan(t, ctx, root, nil, nil, nil, nil, provider)
	if err != nil {
		t.Fatal("baseline scan failed:", err)
	}
	if entry := baseline.Content.Contents["parent"].Contents["sub"]; !bytes.Equal(entry.Digest, first) {
		t.Fatal("incorrect baseline:", entry)
	}

	// Change the commit, without providing any re-check paths, and verify that
	// the scan notices it, without having modified what it was provided.
	provider.probes["parent/sub"] = testingGitProbe{commit: second, url: "https://example.org/sub"}
	provider.probed = nil
	baselineCopy := baseline.Content.Copy(EntryCopyBehaviorDeep)
	recheckPaths := map[string]bool{}
	snapshot, newCache, err := testingGitScan(t, ctx, root, baseline, recheckPaths, cache, nil, provider)
	if err != nil {
		t.Fatal("accelerated scan failed:", err)
	}
	if snapshot == baseline {
		t.Fatal("baseline was reused despite containing a submodule")
	}
	if !slices.Equal(provider.probed, []string{"parent/sub"}) {
		t.Error("incorrect probes:", provider.probed)
	}
	if entry := snapshot.Content.Contents["parent"].Contents["sub"]; entry.Kind != EntryKind_GitLink || !bytes.Equal(entry.Digest, second) {
		t.Error("stale submodule entry:", entry)
	}
	if !baseline.Content.Equal(baselineCopy, true) {
		t.Error("baseline was modified")
	}
	if len(recheckPaths) != 0 {
		t.Error("caller's re-check paths were modified:", recheckPaths)
	}

	// Verify that this didn't cost anything for unrelated directories, which
	// should be reused along with their cache entries, and that statistics are
	// correct.
	if snapshot.Content.Contents["unrelated"] != baseline.Content.Contents["unrelated"] {
		t.Error("unrelated directory wasn't reused")
	}
	if snapshot.Content.Contents["parent"].Contents["sibling"] != baseline.Content.Contents["parent"].Contents["sibling"] {
		t.Error("submodule's sibling wasn't reused")
	}
	if _, ok := newCache.Entries["unrelated/deep/file"]; !ok {
		t.Error("cache entry not propagated for reused directory")
	}
	if snapshot.Files != baseline.Files || snapshot.Directories != baseline.Directories {
		t.Errorf("statistics changed: %d/%d != %d/%d", snapshot.Files, snapshot.Directories, baseline.Files, baseline.Directories)
	}

	// Verify that a submodule that's gone is noticed, and that the same applies
	// to a submodule with re-check paths inside of it, which are merged rather
	// than taking the place of those for submodules.
	if err := os.RemoveAll(filepath.Join(root, "parent", "sibling")); err != nil {
		t.Fatal(err)
	}
	provider.probed = nil
	recheckPaths = map[string]bool{"parent/sibling": true}
	snapshot, _, err = testingGitScan(t, ctx, root, baseline, recheckPaths, cache, nil, provider)
	if err != nil {
		t.Fatal("accelerated scan with re-check path failed:", err)
	}
	if _, ok := snapshot.Content.Contents["parent"].Contents["sibling"]; ok {
		t.Error("re-check path ignored")
	}
	if !slices.Equal(provider.probed, []string{"parent/sub"}) {
		t.Error("incorrect probes with re-check path:", provider.probed)
	}
	if !maps.Equal(recheckPaths, map[string]bool{"parent/sibling": true}) {
		t.Error("caller's re-check paths were modified:", recheckPaths)
	}

	// Verify that a directory that has become a submodule, and a submodule that
	// has become a directory, are noticed once they're dirty, without any panic
	// about their baselines being of the wrong kind.
	provider.probes["parent/sub"] = testingGitProbe{}
	provider.probed = nil
	snapshot, _, err = testingGitScan(t, ctx, root, baseline, map[string]bool{}, cache, nil, provider)
	if err != nil {
		t.Fatal("accelerated scan after falling through failed:", err)
	}
	if entry := snapshot.Content.Contents["parent"].Contents["sub"]; entry.Kind != EntryKind_Directory {
		t.Error("submodule baseline retained for directory:", entry)
	}

	// A baseline without submodules is reused as usual, which is what makes
	// this cost nothing for sessions in Git mode that don't use them.
	plain := testingGitTree(t, "plain/file")
	plainProvider := &testingGitProvider{root: plain, t: t}
	baseline, cache, err = testingGitScan(t, ctx, plain, nil, nil, nil, nil, plainProvider)
	if err != nil {
		t.Fatal("scan without submodules failed:", err)
	}
	snapshot, _, err = testingGitScan(t, ctx, plain, baseline, nil, cache, nil, plainProvider)
	if err != nil {
		t.Fatal("accelerated scan without submodules failed:", err)
	} else if snapshot != baseline {
		t.Error("baseline without submodules wasn't reused")
	}
}

// TestScanGitModeBaselineWithoutProvider tests that a plain scan doesn't
// perform any of the additional work for submodules, even if its baseline has
// them.
func TestScanGitModeBaselineWithoutProvider(t *testing.T) {
	root := testingGitTree(t, "file")
	ignorer, err := dockerignore.NewIgnorer(nil)
	if err != nil {
		t.Fatal(err)
	}
	scan := func(baseline *Snapshot, cache *Cache) (*Snapshot, *Cache) {
		t.Helper()
		snapshot, newCache, _, err := Scan(
			context.Background(),
			root,
			baseline, nil,
			newTestingHasher(), cache,
			ignorer, nil,
			behavior.ProbeMode_ProbeModeProbe,
			SymbolicLinkMode_SymbolicLinkModePortable,
			PermissionsMode_PermissionsModePortable,
		)
		if err != nil {
			t.Fatal("scan failed:", err)
		}
		return snapshot, newCache
	}
	baseline, cache := scan(nil, nil)
	withSubmodule := &Snapshot{
		Content: &Entry{Contents: map[string]*Entry{"file": baseline.Content.Contents["file"], "submodule": tG1}},
	}
	withSubmodule.PreservesExecutability = baseline.PreservesExecutability
	withSubmodule.DecomposesUnicode = baseline.DecomposesUnicode
	if snapshot, _ := scan(withSubmodule, cache); snapshot != withSubmodule {
		t.Error("baseline wasn't reused")
	}
}

// TestScanGitModeCancellation tests that cancellation during a probe isn't
// mistaken for something being wrong with the repository.
func TestScanGitModeCancellation(t *testing.T) {
	root := testingGitTree(t, "submodule/.git/config")
	provider := &testingGitProvider{root: root, t: t}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := testingGitScan(t, ctx, root, nil, nil, nil, nil, provider); !errors.Is(err, ErrScanCancelled) {
		t.Error("incorrect error for cancelled scan:", err)
	}
}

// TestEntryGitLinkPaths tests Entry.GitLinkPaths.
func TestEntryGitLinkPaths(t *testing.T) {
	if paths := (*Entry)(nil).GitLinkPaths(); len(paths) != 0 {
		t.Error("nil entry has submodules:", paths)
	}
	if paths := tD1.GitLinkPaths(); len(paths) != 0 {
		t.Error("directory without submodules has them:", paths)
	}
	if paths := tG1.GitLinkPaths(); !slices.Equal(paths, []string{""}) {
		t.Error("incorrect paths for submodule:", paths)
	}
	root := &Entry{Contents: map[string]*Entry{
		"second": tG2,
		"first":  tDG,
		"file":   tF1,
	}}
	if paths := root.GitLinkPaths(); !slices.Equal(paths, []string{"first/submodule", "second"}) {
		t.Error("incorrect paths for hierarchy:", paths)
	}
}

// testingGitTransition performs a transition in Git mode after a scan that uses
// the provider's probes, and it returns the results and problems, verifying
// that nothing temporary is left behind anywhere, which is a requirement in all
// cases.
func testingGitTransition(
	t *testing.T,
	ctx context.Context,
	root string,
	transitions []*Change,
	provider *testingGitProvider,
	contentMap testingContentMap,
) ([]*Entry, []*Problem) {
	t.Helper()

	// Perform a scan to extract filesystem behavior and a cache.
	ignorer := gitignore.NewIgnorer(root, nil)
	snapshot, cache, _, err := ScanGitMode(
		context.Background(),
		root,
		nil, nil,
		newTestingHasher(), nil,
		ignorer, nil,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
		provider,
	)
	if err != nil {
		t.Fatal("unable to perform scan of baseline:", err)
	}

	// Perform the transitions.
	results, problems, missingFiles := TransitionGitMode(
		ctx,
		root,
		transitions,
		cache,
		newTestingHasher(),
		SymbolicLinkMode_SymbolicLinkModePortable,
		0600,
		0755,
		nil,
		snapshot.DecomposesUnicode,
		&testingProvider{
			storage:    t.TempDir(),
			contentMap: contentMap,
			hasher:     newTestingHasher(),
		},
		provider,
	)
	if missingFiles {
		t.Error("unexpectedly missing staged files")
	}
	for r, result := range results {
		if err := result.EnsureValid(true); err != nil {
			t.Errorf("result %d is invalid: %v", r, err)
		}
	}

	// Verify that there aren't any temporary directories left.
	err = filepath.WalkDir(filepath.Dir(root), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		} else if strings.HasPrefix(entry.Name(), filesystem.TemporaryNamePrefix) {
			t.Error("temporary content left behind:", path)
		}
		return nil
	})
	if err != nil {
		t.Error("unable to walk root:", err)
	}
	return results, problems
}

// testingExpectPaths verifies which paths exist within a root.
func testingExpectPaths(t *testing.T, root string, present, absent []string) {
	t.Helper()
	for _, path := range present {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Errorf("expected content is missing (%s): %v", path, err)
		}
	}
	for _, path := range absent {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("unexpected content is present (%s): %v", path, err)
		}
	}
}

// TestTransitionGitModeClone tests transitions from content that isn't a
// submodule to a submodule, which replaces that content if and only if the
// clone succeeds, and which never leaves a temporary directory behind.
func TestTransitionGitModeClone(t *testing.T) {
	cloneFailure := errors.New("clone failed")

	// Define a directory that contains a submodule at a lower level along with
	// what remains of it after an attempt to remove it.
	nested := &Entry{Contents: map[string]*Entry{"file": tF1, "nested": tDG}}
	nestedReduced := &Entry{Contents: map[string]*Entry{"nested": tDGO}}

	// Define the test cases. Each operates on the same on-disk content.
	tests := []struct {
		description      string
		transitions      []*Change
		cloneErr         error
		nestedErr        error
		expectedResults  []*Entry
		expectedProblems []*Problem
		expectedClones   int
		present          []string
		absent           []string
	}{
		{
			description:     "creation",
			transitions:     []*Change{{Path: "created", New: tG1}},
			expectedResults: []*Entry{tG1},
			expectedClones:  1,
			present:         []string{"created/.git", "created/cloned"},
		},
		{
			description:      "failed creation",
			transitions:      []*Change{{Path: "created", New: tG1}},
			cloneErr:         cloneFailure,
			expectedResults:  []*Entry{nil},
			expectedProblems: []*Problem{{Path: "created", Error: "clone failed"}},
			expectedClones:   1,
			absent:           []string{"created"},
		},
		{
			description:      "creation with content in the way",
			transitions:      []*Change{{Path: "replaced", New: tG1}},
			expectedResults:  []*Entry{nil},
			expectedProblems: []*Problem{{Path: "replaced", Error: "unable to create submodule: unknown content exists at path"}},
			present:          []string{"replaced"},
		},
		{
			description:      "creation below a missing parent",
			transitions:      []*Change{{Path: "missing/created", New: tG1}},
			expectedResults:  []*Entry{nil},
			expectedProblems: []*Problem{{Path: "missing/created", Error: "*"}},
			absent:           []string{"missing"},
		},
		{
			description:     "file replacement",
			transitions:     []*Change{{Path: "replaced", Old: tF1, New: tG1}},
			expectedResults: []*Entry{tG1},
			expectedClones:  1,
			present:         []string{"replaced/.git", "replaced/cloned"},
		},
		{
			description:      "failed file replacement",
			transitions:      []*Change{{Path: "replaced", Old: tF1, New: tG1}},
			cloneErr:         cloneFailure,
			expectedResults:  []*Entry{tF1},
			expectedProblems: []*Problem{{Path: "replaced", Error: "clone failed"}},
			expectedClones:   1,
			present:          []string{"replaced"},
		},
		{
			description:     "empty directory replacement",
			transitions:     []*Change{{Path: "empty", Old: tD0, New: tG1}},
			expectedResults: []*Entry{tG1},
			expectedClones:  1,
			present:         []string{"empty/.git", "empty/cloned"},
		},
		{
			description:      "failed empty directory replacement",
			transitions:      []*Change{{Path: "empty", Old: tD0, New: tG1}},
			cloneErr:         cloneFailure,
			expectedResults:  []*Entry{tD0},
			expectedProblems: []*Problem{{Path: "empty", Error: "clone failed"}},
			expectedClones:   1,
			present:          []string{"empty"},
			absent:           []string{"empty/cloned"},
		},
		{
			description:     "directory replacement",
			transitions:     []*Change{{Path: "plain", Old: tD1, New: tG1}},
			expectedResults: []*Entry{tG1},
			expectedClones:  1,
			present:         []string{"plain/.git", "plain/cloned"},
			absent:          []string{"plain/file"},
		},
		{
			description:      "failed directory replacement",
			transitions:      []*Change{{Path: "plain", Old: tD1, New: tG1}},
			cloneErr:         cloneFailure,
			expectedResults:  []*Entry{tD1},
			expectedProblems: []*Problem{{Path: "plain", Error: "clone failed"}},
			expectedClones:   1,
			present:          []string{"plain/file"},
			absent:           []string{"plain/.git", "plain/cloned"},
		},
		{
			description:      "replacement of directory that has changed",
			transitions:      []*Change{{Path: "plain", Old: tD1, New: tG1}},
			expectedClones:   1,
			expectedResults:  []*Entry{tD0},
			expectedProblems: []*Problem{{Path: "plain/unknown", Error: "unknown content encountered on disk"}},
			present:          []string{"plain/unknown"},
			absent:           []string{"plain/file", "plain/cloned", "plain/.git"},
		},
		{
			description:      "replacement of directory containing submodule",
			transitions:      []*Change{{Path: "directory", Old: tDG, New: tG2}},
			expectedClones:   1,
			expectedResults:  []*Entry{tDGO},
			expectedProblems: []*Problem{{Path: "directory/submodule", Error: "submodule isn't removed automatically; remove it manually"}},
			present:          []string{"directory/submodule/file"},
			absent:           []string{"directory/file", "directory/cloned"},
		},
		{
			description:      "replacement of directory containing nested submodule",
			transitions:      []*Change{{Path: "parent", Old: nested, New: tG2}},
			expectedClones:   1,
			expectedResults:  []*Entry{nestedReduced},
			expectedProblems: []*Problem{{Path: "parent/nested/submodule", Error: "submodule isn't removed automatically; remove it manually"}},
			present:          []string{"parent/nested/submodule/file"},
			absent:           []string{"parent/file", "parent/nested/file", "parent/cloned"},
		},
		{
			description:      "failed replacement of directory containing submodule",
			transitions:      []*Change{{Path: "directory", Old: tDG, New: tG2}},
			cloneErr:         cloneFailure,
			expectedClones:   1,
			expectedResults:  []*Entry{tDG},
			expectedProblems: []*Problem{{Path: "directory", Error: "clone failed"}},
			present:          []string{"directory/file", "directory/submodule/file"},
		},
		{
			description:      "nested submodule failure",
			transitions:      []*Change{{Path: "created", New: tG1}},
			nestedErr:        errors.New("nested failed"),
			expectedClones:   1,
			expectedResults:  []*Entry{tG1},
			expectedProblems: []*Problem{{Path: "created", Error: "unable to update nested submodules: nested failed"}},
			present:          []string{"created/cloned"},
		},
		{
			description:      "nested submodule failure when replacing content",
			transitions:      []*Change{{Path: "plain", Old: tD1, New: tG1}},
			nestedErr:        errors.New("nested failed"),
			expectedClones:   1,
			expectedResults:  []*Entry{tG1},
			expectedProblems: []*Problem{{Path: "plain", Error: "unable to update nested submodules: nested failed"}},
			present:          []string{"plain/cloned"},
			absent:           []string{"plain/file"},
		},
		{
			description:     "directory creation containing submodule",
			transitions:     []*Change{{Path: "created", New: tDG}},
			expectedResults: []*Entry{tDG},
			expectedClones:  1,
			present:         []string{"created/file", "created/submodule/.git", "created/submodule/cloned"},
		},
		{
			description:      "failed directory creation containing submodule",
			transitions:      []*Change{{Path: "created", New: tDG}},
			cloneErr:         cloneFailure,
			expectedClones:   1,
			expectedResults:  []*Entry{tD1},
			expectedProblems: []*Problem{{Path: "created/submodule", Error: "clone failed"}},
			present:          []string{"created/file"},
			absent:           []string{"created/submodule"},
		},
		{
			description:      "root",
			transitions:      []*Change{{Path: "", Old: tD1, New: tG1}},
			expectedResults:  []*Entry{tD1},
			expectedProblems: []*Problem{{Path: "", Error: "synchronization root can't be a submodule"}},
			present:          []string{"plain/file"},
		},
	}

	// Process test cases.
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			// Create the on-disk content.
			root := testingGitTree(t,
				"replaced",
				"empty/",
				"plain/file",
				"directory/file",
				"directory/submodule/file",
				"parent/file",
				"parent/nested/file",
				"parent/nested/submodule/file",
			)

			// Create the provider. Scans don't see submodules here, because the
			// directories that stand in for them don't contain .git entries.
			provider := &testingGitProvider{
				root:      root,
				t:         t,
				cloneErr:  test.cloneErr,
				nestedErr: test.nestedErr,
			}

			// Verify that each clone is performed in a temporary directory
			// that's a sibling of the target, under the name that identifies
			// these directories.
			var target string
			if len(test.transitions) > 0 {
				target = test.transitions[0].Path
			}
			if target == "created" && test.transitions[0].New.Kind == EntryKind_Directory {
				target = "created/submodule"
			}
			provider.onClone = func(directory string) {
				expectedParent := filepath.Dir(filepath.Join(root, filepath.FromSlash(target)))
				if filepath.Dir(directory) != expectedParent {
					t.Errorf("clone isn't next to its target: %s", directory)
				}
				if !strings.HasPrefix(filepath.Base(directory), filesystem.TemporaryNamePrefix) {
					t.Errorf("clone doesn't have a temporary name: %s", directory)
				}
			}

			// Copy the old entries so that we can verify that they aren't
			// modified.
			originals := make([]*Entry, len(test.transitions))
			for i, transition := range test.transitions {
				originals[i] = transition.Old.Copy(EntryCopyBehaviorDeep)
			}

			// Perform a modification that the scan doesn't know about, for the
			// test case that's about that, which has to happen after the scan.
			contentMap := testingContentMap{"created/file": []byte(tF1Content)}
			if test.description == "replacement of directory that has changed" {
				onClone := provider.onClone
				provider.onClone = func(directory string) {
					onClone(directory)
					if err := os.WriteFile(filepath.Join(root, "plain", "unknown"), []byte("unknown"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}

			// Perform the transitions.
			results, problems := testingGitTransition(t, context.Background(), root, test.transitions, provider, contentMap)

			// Check results and problems.
			if len(results) != len(test.expectedResults) {
				t.Fatalf("incorrect result count: %d != %d", len(results), len(test.expectedResults))
			}
			for r, result := range results {
				if !result.Equal(test.expectedResults[r], true) {
					t.Errorf("result %d does not match expected: %v != %v", r, result, test.expectedResults[r])
				}
			}
			if !testingProblemListsEqual(problems, test.expectedProblems) {
				t.Errorf("problems do not match expected: %v != %v", problems, test.expectedProblems)
			}

			// Check the number of clones.
			if len(provider.cloned) != test.expectedClones {
				t.Errorf("incorrect number of clones: %d != %d", len(provider.cloned), test.expectedClones)
			}

			// Nested submodules are only updated following a successful clone,
			// and in the clone, which is to say before the content is replaced.
			if test.expectedClones == 1 && test.cloneErr == nil {
				if !slices.Equal(provider.nested, provider.cloned) {
					t.Errorf("nested submodules not updated in clone: %v != %v", provider.nested, provider.cloned)
				}
			} else if len(provider.nested) != 0 {
				t.Error("nested submodules updated despite not having a clone:", provider.nested)
			}

			// Verify that the old entries weren't modified.
			for i, transition := range test.transitions {
				if !transition.Old.Equal(originals[i], true) {
					t.Errorf("old entry %d was modified", i)
				}
			}

			// Check the on-disk content.
			testingExpectPaths(t, root, test.present, test.absent)
		})
	}
}

// TestTransitionGitModeClonePermissions tests that a clone's top-level
// directory gets the permissions that are configured for directories.
func TestTransitionGitModeClonePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions aren't supported")
	}
	root := testingGitTree(t, "file")
	provider := &testingGitProvider{root: root, t: t}
	results, problems := testingGitTransition(t, context.Background(), root, []*Change{{Path: "created", New: tG1}}, provider, nil)
	if len(problems) > 0 || !results[0].Equal(tG1, true) {
		t.Fatal("transition failed:", results, problems)
	}
	info, err := os.Lstat(filepath.Join(root, "created"))
	if err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0755 {
		t.Errorf("incorrect permissions: %v", info.Mode().Perm())
	}
}

// TestTransitionGitModeCloneSymbolicLink tests that a submodule isn't created
// beyond a symbolic link, the target of which isn't part of the root.
func TestTransitionGitModeCloneSymbolicLink(t *testing.T) {
	root := testingGitTree(t, "file")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symbolic links aren't available:", err)
	}
	provider := &testingGitProvider{root: root, t: t}
	results, problems := testingGitTransition(t, context.Background(), root, []*Change{{Path: "link/created", New: tG1}}, provider, nil)
	if results[0] != nil {
		t.Error("submodule reported as created:", results[0])
	}
	if !testingProblemListsEqual(problems, []*Problem{{Path: "link/created", Error: "*"}}) {
		t.Error("incorrect problems:", problems)
	}
	if len(provider.cloned) != 0 {
		t.Error("clone attempted beyond symbolic link:", provider.cloned)
	}
	if entries, err := os.ReadDir(outside); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Error("content created beyond symbolic link:", entries)
	}
}

// TestTransitionGitModeCloneCancellation tests that cancellation during a clone
// removes the temporary directory and leaves everything in place, whichever
// part of the operation it arrives during.
func TestTransitionGitModeCloneCancellation(t *testing.T) {
	for _, test := range []struct {
		description string
		cloneErr    error
		nestedErr   error
	}{
		{"during clone", errors.New("killed"), nil},
		{"during nested update", nil, errors.New("killed")},
		{"after nested update", nil, nil},
	} {
		t.Run(test.description, func(t *testing.T) {
			root := testingGitTree(t, "plain/file")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &testingGitProvider{
				root: root, t: t,
				cloneErr: test.cloneErr, nestedErr: test.nestedErr,
				onClone: func(string) { cancel() },
			}
			results, problems := testingGitTransition(t, ctx, root, []*Change{{Path: "plain", Old: tD1, New: tG1}}, provider, nil)
			if !results[0].Equal(tD1, true) {
				t.Error("incorrect result:", results[0])
			}
			if !testingProblemListsEqual(problems, []*Problem{{Path: "plain", Error: errTransitionCancelled.Error()}}) {
				t.Error("incorrect problems:", problems)
			}
			testingExpectPaths(t, root, []string{"plain/file"}, []string{"plain/cloned"})
		})
	}

	// Verify that nothing is started once the transition has been cancelled.
	t.Run("before clone", func(t *testing.T) {
		root := testingGitTree(t, "plain/file")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		provider := &testingGitProvider{root: root, t: t}
		results, problems := testingGitTransition(t, ctx, root, []*Change{{Path: "plain", Old: tD1, New: tG1}}, provider, nil)
		if !results[0].Equal(tD1, true) || len(problems) != 1 || len(provider.cloned) != 0 {
			t.Error("transition not cancelled:", results, problems, provider.cloned)
		}
	})
}

// TestTransitionGitModeUpdate tests transitions between the commits of a
// submodule that's in place, which must never replace or remove it.
func TestTransitionGitModeUpdate(t *testing.T) {
	updateFailure := errors.New("commit isn't available")
	tests := []struct {
		description      string
		head             []byte
		updateErr        error
		nestedErr        error
		expectedResult   *Entry
		expectedProblems []*Problem
		expectUpdate     bool
	}{
		{
			description:    "update",
			head:           tG1.Digest,
			expectedResult: tG2,
			expectUpdate:   true,
		},
		{
			description:    "already at target",
			head:           tG2.Digest,
			expectedResult: tG2,
		},
		{
			description:      "changed since scan",
			head:             tG3.Digest,
			expectedResult:   tG1,
			expectedProblems: []*Problem{{Path: "submodule", Error: "submodule's commit changed since the scan"}},
		},
		{
			description:      "update failure",
			head:             tG1.Digest,
			updateErr:        updateFailure,
			expectedResult:   tG1,
			expectedProblems: []*Problem{{Path: "submodule", Error: "commit isn't available"}},
			expectUpdate:     true,
		},
		{
			description:      "no commit",
			expectedResult:   tG1,
			expectedProblems: []*Problem{{Path: "submodule", Error: "unable to determine submodule's commit: no head"}},
		},
		{
			description:      "nested submodule failure",
			head:             tG1.Digest,
			nestedErr:        errors.New("nested failed"),
			expectedResult:   tG2,
			expectedProblems: []*Problem{{Path: "submodule", Error: "unable to update nested submodules: nested failed"}},
			expectUpdate:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			root := testingGitTree(t, "submodule/file")
			provider := &testingGitProvider{root: root, t: t, updateErr: test.updateErr, nestedErr: test.nestedErr, heads: map[string][]byte{}}
			if test.head != nil {
				provider.heads["submodule"] = test.head
			}
			results, problems := testingGitTransition(t, context.Background(), root, []*Change{{Path: "submodule", Old: tG1, New: tG2}}, provider, nil)
			if !results[0].Equal(test.expectedResult, true) {
				t.Errorf("incorrect result: %v != %v", results[0], test.expectedResult)
			}
			if !testingProblemListsEqual(problems, test.expectedProblems) {
				t.Errorf("incorrect problems: %v != %v", problems, test.expectedProblems)
			}
			if updated := len(provider.updated) > 0; updated != test.expectUpdate {
				t.Errorf("incorrect update behavior: %t != %t", updated, test.expectUpdate)
			}
			if len(provider.cloned) != 0 {
				t.Error("existing submodule cloned:", provider.cloned)
			}
			if updatedNested := len(provider.nested) > 0; updatedNested != (test.expectUpdate && test.updateErr == nil) {
				t.Errorf("incorrect nested update behavior: %v", provider.nested)
			}
			testingExpectPaths(t, root, []string{"submodule/file"}, nil)
		})
	}

	// A submodule's directory that's gone or that's become something else can't
	// be checked out. Without verifying this, Git would follow a symbolic link.
	t.Run("missing", func(t *testing.T) {
		root := testingGitTree(t, "file")
		provider := &testingGitProvider{root: root, t: t, heads: map[string][]byte{"submodule": tG1.Digest}}
		results, problems := testingGitTransition(t, context.Background(), root, []*Change{{Path: "submodule", Old: tG1, New: tG2}}, provider, nil)
		if !results[0].Equal(tG1, true) || len(problems) != 1 || len(provider.updated) != 0 {
			t.Error("transition for missing submodule succeeded:", results, problems, provider.updated)
		}
	})
	t.Run("symbolic link", func(t *testing.T) {
		root := testingGitTree(t, "file")
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "submodule")); err != nil {
			t.Skip("symbolic links aren't available:", err)
		}
		provider := &testingGitProvider{root: root, t: t, heads: map[string][]byte{"submodule": tG1.Digest}}
		results, problems := testingGitTransition(t, context.Background(), root, []*Change{{Path: "submodule", Old: tG1, New: tG2}}, provider, nil)
		if !results[0].Equal(tG1, true) || len(problems) != 1 || len(provider.updated) != 0 {
			t.Error("transition through symbolic link succeeded:", results, problems, provider.updated)
		}
	})
}

// TestTransitionGitModeRemovalRefused tests that nothing about having a
// provider changes the fact that submodules aren't removed or replaced by other
// content.
func TestTransitionGitModeRemovalRefused(t *testing.T) {
	root := testingGitTree(t, "submodule/file")
	provider := &testingGitProvider{root: root, t: t}
	results, problems := testingGitTransition(t, context.Background(), root, []*Change{
		{Path: "submodule", Old: tG1},
	}, provider, nil)
	if !results[0].Equal(tG1, true) || len(problems) != 1 || problems[0].Error != errGitLinkRemovalRefused.Error() {
		t.Error("submodule removal not refused:", results, problems)
	}
	testingExpectPaths(t, root, []string{"submodule/file"}, nil)
}
