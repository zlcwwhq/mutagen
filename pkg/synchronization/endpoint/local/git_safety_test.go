package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestEndpointGitModeUnreadableRules verifies that read failures hide the
// affected subtree from reconciliation, while unrelated directories still scan.
func TestEndpointGitModeUnreadableRules(t *testing.T) {
	isolateGit(t)
	for _, rules := range []string{".gitignore", "nested/.gitignore", ".git/info/exclude"} {
		t.Run(rules, func(t *testing.T) {
			root := newGitRoot(t, map[string]string{"secret.env": "root secret", "nested/secret.env": "nested secret", "public": "public"})
			initializeRepository(t, root)
			writeTree(t, root, map[string]string{rules: "secret.env\n"})
			rulePath := filepath.Join(root, filepath.FromSlash(rules))
			if err := os.Chmod(rulePath, 0); err != nil {
				t.Skip(err)
			}
			t.Cleanup(func() { os.Chmod(rulePath, 0600) })
			if file, err := os.Open(rulePath); err == nil {
				file.Close()
				t.Skip("process can read permission-denied files")
			}
			e := newGitModeEndpoint(t, root, synchronization.WatchMode_WatchModeNoWatch)
			snapshot := scanEndpoint(t, e, true)
			blocked := ""
			if rules == "nested/.gitignore" {
				blocked = "nested"
			}
			entry := snapshot.Content
			if blocked != "" {
				entry = entryAt(snapshot, blocked)
			}
			if entry == nil || entry.Kind != core.EntryKind_Problematic {
				t.Fatalf("subtree was not blocked: %v", entry)
			}
			if blocked != "" && entryAt(snapshot, "public").Kind != core.EntryKind_File {
				t.Error("unaffected file not scanned")
			}
			// Reconciliation cannot send secrets from the blocked subtree.
			empty := &core.Entry{Kind: core.EntryKind_Directory}
			_, _, changes, _ := core.ReconcileGitMode(nil, snapshot.Content, empty, core.SynchronizationMode_SynchronizationModeTwoWaySafe)
			for _, change := range changes {
				if change.Path == blocked || change.New.GetContents()["secret.env"] != nil {
					t.Fatal("blocked subtree entered changes:", change)
				}
			}
			if err := os.Chmod(rulePath, 0600); err != nil {
				t.Fatal(err)
			}
			recovered := scanEndpoint(t, e, true).Content
			if blocked != "" {
				recovered = recovered.Contents[blocked]
			}
			if recovered.Kind != core.EntryKind_Directory {
				t.Error("scan did not recover after permissions were restored")
			}
		})
	}
}
