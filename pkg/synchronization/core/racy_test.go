package core

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

// scanRacyTestRoot scans the specified root using the specified cache.
func scanRacyTestRoot(t *testing.T, root string, cache *Cache) (*Snapshot, *Cache) {
	t.Helper()
	ignorer, err := mutagenignore.NewIgnorer(nil)
	if err != nil {
		t.Fatal("unable to create ignorer:", err)
	}
	snapshot, newCache, _, err := Scan(
		context.Background(),
		root,
		nil, nil,
		newTestingHasher(), cache,
		ignorer, nil,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
	)
	if err != nil {
		t.Fatal("unable to perform scan:", err)
	}
	return snapshot, newCache
}

// rewriteWithinTimestampGranule rewrites the specified file in place with
// content of the same size and restores its modification time, which simulates
// a modification within the same timestamp granule as the previous one. The
// file then has the same type, modification time, size, and file ID as before.
func rewriteWithinTimestampGranule(t *testing.T, path, content string) {
	t.Helper()
	metadata, err := os.Stat(path)
	if err != nil {
		t.Fatal("unable to query file metadata:", err)
	} else if metadata.Size() != int64(len(content)) {
		t.Fatal("rewrite would change file size")
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal("unable to rewrite file:", err)
	}
	modificationTime := metadata.ModTime()
	if err := os.Chtimes(path, modificationTime, modificationTime); err != nil {
		t.Fatal("unable to restore modification time:", err)
	}
}

// TestScanRacyDigest tests that scans don't reuse digests computed so soon
// after a file's modification that a later modification may have left its
// metadata unchanged, while they do reuse the digests of older files.
func TestScanRacyDigest(t *testing.T) {
	// Create a root with a recently modified file and scan it.
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("first!"), 0600); err != nil {
		t.Fatal("unable to create file:", err)
	}
	_, cache := scanRacyTestRoot(t, root, nil)
	if entry := cache.Entries["file"]; entry == nil || !entry.Racy {
		t.Fatal("digest of recently modified file not marked as racy")
	}

	// Modify the file without changing its metadata, as a modification in the
	// same timestamp granule would, and verify that a rescan detects it.
	rewriteWithinTimestampGranule(t, path, "second")
	snapshot, cache := scanRacyTestRoot(t, root, cache)
	if digest := snapshot.Content.Contents["file"].GetDigest(); !bytes.Equal(digest, testingDigest("second")) {
		t.Error("rescan reused racy digest")
	}

	// Backdate the file beyond the racy window and verify that its digest is
	// no longer marked as racy and is then reused.
	backdated := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, backdated, backdated); err != nil {
		t.Fatal("unable to backdate file:", err)
	}
	_, cache = scanRacyTestRoot(t, root, cache)
	if entry := cache.Entries["file"]; entry == nil || entry.Racy {
		t.Fatal("digest of backdated file marked as racy")
	}
	ignorer, err := mutagenignore.NewIgnorer(nil)
	if err != nil {
		t.Fatal("unable to create ignorer:", err)
	}
	_, _, _, err = Scan(
		context.Background(),
		root,
		nil, nil,
		&testHashingDetector{newTestingHasher(), func() {
			t.Error("digest of backdated file not reused")
		}}, cache,
		ignorer, nil,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
	)
	if err != nil {
		t.Fatal("unable to perform scan:", err)
	}
}

// TestTransitionRacyFile tests that transitions verify the contents of files
// with racy digests before removing them, refusing to remove a file that was
// modified without a change to its metadata.
func TestTransitionRacyFile(t *testing.T) {
	for _, modify := range []bool{false, true} {
		// Create a root with a recently modified file and scan it.
		root := t.TempDir()
		path := filepath.Join(root, "file")
		if err := os.WriteFile(path, []byte("first!"), 0600); err != nil {
			t.Fatal("unable to create file:", err)
		}
		snapshot, cache := scanRacyTestRoot(t, root, nil)
		if entry := cache.Entries["file"]; entry == nil || !entry.Racy {
			t.Fatal("digest of recently modified file not marked as racy")
		}
		old := snapshot.Content.Contents["file"]

		// Optionally modify the file without changing its metadata.
		if modify {
			rewriteWithinTimestampGranule(t, path, "second")
		}

		// Attempt to remove the file.
		provider := &testingProvider{storage: t.TempDir(), hasher: newTestingHasher()}
		results, problems, _ := Transition(
			context.Background(),
			root,
			[]*Change{{Path: "file", Old: old}},
			cache,
			newTestingHasher(),
			SymbolicLinkMode_SymbolicLinkModePortable,
			0600,
			0700,
			nil,
			snapshot.DecomposesUnicode,
			provider,
		)
		if len(results) != 1 {
			t.Fatalf("unexpected number of results: %d", len(results))
		}

		// Verify the outcome.
		_, statErr := os.Stat(path)
		if modify {
			if len(problems) != 1 || !strings.Contains(problems[0].Error, "modification detected") {
				t.Error("modification of racy file not detected:", problems)
			}
			if contents, err := os.ReadFile(path); err != nil || string(contents) != "second" {
				t.Error("modified racy file not preserved:", string(contents), err)
			}
		} else {
			if len(problems) != 0 {
				t.Error("unmodified racy file not removed:", problems)
			}
			if !os.IsNotExist(statErr) {
				t.Error("unmodified racy file still exists:", statErr)
			}
		}
	}
}
