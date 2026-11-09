package local

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// scanWithSubmodules performs a full scan that must succeed, with the specified
// GitLink entries as those of the session's ancestor, and verifies that the
// resulting snapshot is valid.
func scanWithSubmodules(t *testing.T, e *endpoint, submodules map[string]*core.Entry) *core.Snapshot {
	t.Helper()
	snapshot, err, _ := e.Scan(context.Background(), nil, submodules, true)
	if err != nil {
		t.Fatal("scan failed:", err)
	} else if err := snapshot.EnsureValid(); err != nil {
		t.Fatal("invalid snapshot:", err)
	}
	return snapshot
}

// TestEndpointGitModeSubmoduleStatuses tests that a scan describes the working
// trees of the submodules that have a commit checked out other than the one
// that the ancestor has for them (or for which it has none), by listing the
// files that Git doesn't consider to be the commit's, and that it doesn't
// examine the others.
func TestEndpointGitModeSubmoduleStatuses(t *testing.T) {
	// Create a submodule with files in various states: modified, staged,
	// deleted, assumed to be unchanged while modified, outside of the sparse
	// checkout while present, staged and then undone in the working tree, not
	// tracked, and unchanged.
	upstream := newGitUpstream(t, "upstream")
	committed := map[string]string{}
	for _, name := range []string{"modified", "staged", "deleted", "assumed", "skipped", "undone", "unchanged", "directory/unchanged"} {
		committed[name] = "committed"
	}
	upstream.commitFiles("files", committed)
	upstream.push()
	clone := newSubmoduleClone(t, upstream)
	writeTree(t, clone.directory, map[string]string{
		"modified":          "changed",
		"staged":            "changed",
		"assumed":           "changed",
		"skipped":           "changed",
		"undone":            "changed",
		"untracked":         "new",
		"directory/created": "new",
	})
	mustGit(t, clone.directory, "add", "--", "staged", "undone")
	mustGit(t, clone.directory, "update-index", "--assume-unchanged", "assumed")
	mustGit(t, clone.directory, "update-index", "--skip-worktree", "skipped")
	writeTree(t, clone.directory, map[string]string{"undone": "committed"})
	if err := os.Remove(filepath.Join(clone.directory, "deleted")); err != nil {
		t.Fatal(err)
	}

	// Within it, create a nested submodule with a modified file, whose content
	// isn't part of the submodule's, and a nested repository that isn't a
	// submodule, whose content is.
	nested := newGitWithOrigin(t, clone.directory, "nested", upstream.url)
	writeTree(t, nested, map[string]string{"file": "changed"})
	newGitWithOrigin(t, clone.directory, "plain", "")

	// Verify that both submodules are examined if the ancestor doesn't have
	// them.
	e := newGitModeEndpoint(t, clone.root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanWithSubmodules(t, e, nil)
	link, nestedLink := gitLinkAt(t, snapshot, "sub"), gitLinkAt(t, snapshot, "sub/nested")
	expected := map[string][]string{
		"sub":        {"assumed", "directory/created", "modified", "plain/file", "skipped", "staged", "undone", "untracked"},
		"sub/nested": {"file"},
	}
	verifyStatuses := func(snapshot *core.Snapshot, paths ...string) {
		t.Helper()
		if len(snapshot.SubmoduleStatuses) != len(paths) {
			t.Errorf("incorrect statuses: %v", snapshot.SubmoduleStatuses)
		}
		for _, path := range paths {
			if status := snapshot.SubmoduleStatuses[path]; status == nil {
				t.Errorf("no status for %s", path)
			} else if !slices.Equal(status.Changed, expected[path]) {
				t.Errorf("incorrect changes for %s: %v != %v", path, status.Changed, expected[path])
			} else if commit := hex.EncodeToString(status.Commit); commit != hex.EncodeToString(gitLinkAt(t, snapshot, path).Digest) {
				t.Errorf("incorrect commit for %s: %s", path, commit)
			}
		}
	}
	verifyStatuses(snapshot, "sub", "sub/nested")

	// Verify that a submodule isn't examined if the ancestor has its commit,
	// and that it is if the ancestor has another commit.
	other := &core.Entry{Kind: core.EntryKind_GitLink, Digest: []byte{1}, Url: upstream.url}
	verifyStatuses(scanWithSubmodules(t, e, map[string]*core.Entry{"sub": link, "sub/nested": nestedLink}))
	verifyStatuses(scanWithSubmodules(t, e, map[string]*core.Entry{"sub": other, "sub/nested": nestedLink}), "sub")
	verifyStatuses(scanWithSubmodules(t, e, map[string]*core.Entry{"sub": link, "sub/nested": other}), "sub/nested")

	// Verify that a status isn't used if the commit that's checked out isn't
	// the one that was scanned.
	content := snapshot.Content.Copy(core.EntryCopyBehaviorDeepPreservingLeaves)
	content.Contents["sub"].Contents[core.GitLinkName] = other
	if _, err := e.gitProvider.submoduleStatus(context.Background(), "sub", content.Contents["sub"]); err == nil || !strings.Contains(err.Error(), "commit changed") {
		t.Error("status for another commit not refused:", err)
	}
}

// TestEndpointGitModeSubmoduleStatusesLineEndings tests that a file whose line
// endings Git converts on checkout is the commit's, as Git considers it to be.
func TestEndpointGitModeSubmoduleStatusesLineEndings(t *testing.T) {
	upstream := newGitUpstream(t, "upstream")
	upstream.commitFiles("text", map[string]string{"text": "line\n"})
	upstream.push()
	root := newGitRoot(t, nil)
	directory := filepath.Join(root, "sub")
	mustGit(t, root, "clone", "--quiet", "--config", "core.autocrlf=true", "--", upstream.url, directory)
	if contents, err := os.ReadFile(filepath.Join(directory, "text")); err != nil {
		t.Fatal(err)
	} else if string(contents) != "line\r\n" {
		t.Fatalf("line endings not converted: %q", contents)
	}
	e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
	snapshot := scanWithSubmodules(t, e, nil)
	if status := snapshot.SubmoduleStatuses["sub"]; status == nil {
		t.Error("submodule not examined")
	} else if len(status.Changed) != 0 {
		t.Errorf("converted files considered changed: %v", status.Changed)
	}
}
