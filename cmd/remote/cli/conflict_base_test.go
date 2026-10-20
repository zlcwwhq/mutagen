package cli

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestThreeWayWorkspace(t *testing.T) {
	// With their common ancestor, both copies' changes are merged, and only
	// overlapping changes are marked, with all three versions.
	w := newTestWorkspaceWithBase(t, &workspace{Path: "file", Token: "token", directory: t.TempDir()},
		"1\nlocal\n3\n4\n", "1\n2\n3\nremote\n", "1\n2\n3\n4\n")
	if data, err := os.ReadFile(w.file(mergedName)); err != nil || string(data) != "1\nlocal\n3\nremote\n" {
		t.Fatalf("copies not merged: %q: %v", data, err)
	} else if result, err := describe(w, "file"); err != nil || result.Regions != 0 || result.Base != w.file(baseName) {
		t.Fatalf("clean merge not described: %+v: %v", result, err)
	} else if _, _, err := resolution(context.Background(), w, &options{}); err != nil {
		t.Fatal("clean merge not applicable:", err)
	}
	w = newTestWorkspaceWithBase(t, &workspace{Path: "file", Token: "token", directory: t.TempDir()}, "local\n", "remote\n", "base\n")
	if result, err := describe(w, "file"); err != nil || result.Regions != 1 {
		t.Fatalf("conflicting region not described: %+v: %v", result, err)
	} else if data, _ := os.ReadFile(w.file(mergedName)); !strings.Contains(string(data), markersFor("token").base+"\nbase\n") {
		t.Fatalf("base version not marked: %q", data)
	} else if _, _, err := resolution(context.Background(), w, &options{}); err == nil {
		t.Fatal("unresolved base markers accepted")
	}
}

func TestFindBase(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	local := filepath.Join(directory, "file")
	if err := os.WriteFile(local, []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	conflict := &remote.ConflictRoot{
		Path: "file", Files: true, Algorithm: "sha1", BaseDigest: fmt.Sprintf("%x", sha1.Sum([]byte("base\n"))),
		AlphaDigest: fmt.Sprintf("%x", sha1.Sum([]byte("local\n"))), BetaDigest: fmt.Sprintf("%x", sha1.Sum([]byte("remote\n"))),
	}
	candidate := filepath.Join(t.TempDir(), "candidate")
	find := func(explicit string) (string, string, error) {
		t.Helper()
		destination := filepath.Join(t.TempDir(), "base")
		source, err := findBase(ctx, conflict, local, explicit, destination)
		data, _ := os.ReadFile(destination)
		return source, string(data), err
	}

	// Explicit candidates must be verified.
	os.WriteFile(candidate, []byte("other\n"), 0600)
	if _, data, err := find(candidate); err == nil || !strings.Contains(err.Error(), "common ancestor") || data != "" {
		t.Fatalf("unverified base accepted: %v", err)
	}
	os.WriteFile(candidate, []byte("base\n"), 0600)
	if source, data, err := find(candidate); err != nil || source == "" || data != "base\n" {
		t.Fatalf("verified base rejected: %v", err)
	}
	if source, _, err := find(""); err != nil || source != "" {
		t.Fatalf("base found without candidates: %q: %v", source, err)
	}

	// Without the copies' digests, or with an unsupported algorithm, copies
	// can't be bound to the listing, so no ancestor is used.
	for _, unbound := range []remote.ConflictRoot{
		{Path: "file", Files: true, Algorithm: "sha1", BaseDigest: conflict.BaseDigest},
		{Path: "file", Files: true, Algorithm: "unknown", BaseDigest: conflict.BaseDigest, AlphaDigest: conflict.AlphaDigest, BetaDigest: conflict.BetaDigest},
	} {
		destination := filepath.Join(t.TempDir(), "base")
		if _, err := findBase(ctx, &unbound, local, candidate, destination); err == nil || !strings.Contains(err.Error(), "can't be verified") {
			t.Fatalf("unbound base accepted: %+v: %v", unbound, err)
		} else if source, err := findBase(ctx, &unbound, local, "", destination); err != nil || source != "" {
			t.Fatalf("unbound base found: %+v: %q: %v", unbound, source, err)
		}
	}

	// Without one, versions in Git are verified.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		command.Dir = directory
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	os.WriteFile(local, []byte("base\n"), 0600)
	git("add", "file")
	git("commit", "-q", "-m", "base")
	os.WriteFile(local, []byte("local\n"), 0600)
	if source, data, err := find(""); err != nil || !strings.Contains(source, "HEAD") || data != "base\n" {
		t.Fatalf("committed base not found: %q: %v", source, err)
	}
	os.WriteFile(local, []byte("other\n"), 0600)
	git("commit", "-q", "-a", "-m", "other")
	os.WriteFile(local, []byte("base\n"), 0600)
	git("add", "file")
	if source, data, err := find(""); err != nil || !strings.Contains(source, "index") || data != "base\n" {
		t.Fatalf("staged base not found: %q: %v", source, err)
	}
	conflict.BaseDigest = fmt.Sprintf("%x", sha1.Sum([]byte("unknown\n")))
	if source, _, err := find(""); err != nil || source != "" {
		t.Fatalf("unverified base found: %q: %v", source, err)
	}
}

func TestCheckoutBindsCopiesToListing(t *testing.T) {
	// The listed common ancestor is that of the listed copies. Copies that
	// changed after the listing (here, after they came to agree on "a\n" and
	// changed again) must not be merged with it: since the remote change
	// restores a line of the stale ancestor, the merge would take the local
	// copy without marking anything, silently reverting the remote change.
	ctx := context.Background()
	digest := func(data string) string { return fmt.Sprintf("%x", sha1.Sum([]byte(data))) }
	base := filepath.Join(t.TempDir(), "base")
	if err := os.WriteFile(base, []byte("a\nb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	listing := &remote.ConflictRoot{
		Path: "file", Files: true, Algorithm: "sha1", BaseDigest: digest("a\nb\n"),
		AlphaDigest: digest("a\nb\nalpha\n"), BetaDigest: digest("beta\na\nb\n"),
	}
	serve := func(data string) copier {
		return func(output io.Writer) (*remote.Entry, error) {
			_, err := io.WriteString(output, data)
			return &remote.Entry{Kind: remote.EntryFile, Size: int64(len(data)), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}, err
		}
	}
	for _, alpha := range []bool{true, false} {
		t.Run(fmt.Sprintf("alpha=%t", alpha), func(t *testing.T) {
			checkout := func(listing *remote.ConflictRoot, explicit, alphaCopy, betaCopy string) (*workspace, error) {
				w := &workspace{Path: "file", Alpha: alpha, Token: "token", directory: t.TempDir()}
				local, opposite := serve(alphaCopy), serve(betaCopy)
				if !alpha {
					local, opposite = opposite, local
				}
				return w, checkoutFiles(ctx, listing, filepath.Join(t.TempDir(), "file"), explicit, w, local, opposite)
			}

			// The listed copies are merged with their ancestor.
			if w, err := checkout(listing, base, "a\nb\nalpha\n", "beta\na\nb\n"); err != nil || w.BaseSource == "" {
				t.Fatalf("listed copies not merged with their ancestor: %v", err)
			} else if data, err := os.ReadFile(w.file(mergedName)); err != nil || string(data) != "beta\na\nb\nalpha\n" {
				t.Fatalf("listed copies merged incorrectly: %q: %v", data, err)
			}

			// Other copies, including the listed ones exchanged, are rejected
			// before an ancestor is used, whether found or specified.
			for _, explicit := range []string{base, ""} {
				for _, copies := range [][2]string{{"a\nx\n", "a\nb\n"}, {"a\nb\nalpha\n", "a\nb\n"}, {"beta\na\nb\n", "a\nb\nalpha\n"}} {
					if w, err := checkout(listing, explicit, copies[0], copies[1]); !errors.Is(err, errCopiesChanged) {
						t.Fatalf("changed copies %q accepted: %v", copies, err)
					} else if _, err := os.Lstat(w.file(mergedName)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("changed copies %q merged: %v", copies, err)
					}
				}
			}

			// If the algorithm isn't supported, copies can't be compared with
			// the listed ones, and they're merged without an ancestor.
			unsupported := *listing
			unsupported.Algorithm = "unknown"
			if _, err := checkout(&unsupported, base, "a\nx\n", "a\nb\n"); err == nil || !strings.Contains(err.Error(), "can't be verified") {
				t.Fatalf("specified ancestor used for unverifiable copies: %v", err)
			} else if w, err := checkout(&unsupported, "", "a\nx\n", "a\nb\n"); err != nil || w.BaseSource != "" {
				t.Fatalf("unverifiable copies not merged without an ancestor: %q: %v", w.BaseSource, err)
			} else if regions, err := countMarkers(w.file(mergedName), markersFor("token").local); err != nil || regions != 1 {
				t.Fatalf("unverifiable copies' differences not marked: %d: %v", regions, err)
			}
		})
	}
}

func TestRetireAndPrune(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	local := filepath.Join(t.TempDir(), "file")
	base, directory, err := workspaceDirectory(local)
	if err != nil {
		t.Fatal(err)
	} else if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	file := &remote.Entry{Kind: remote.EntryFile, Size: 1, Digest: strings.Repeat("0", 64)}
	w := &workspace{Session: "session", Path: "file", Local: file, Remote: file, directory: directory}
	if err := w.save(); err != nil {
		t.Fatal(err)
	} else if paths := sessionWorkspaces(&remote.Target{Session: "session"}); paths["file"] == nil || paths["file"].applied() {
		t.Fatal("workspace not listed as checked out with nothing applied")
	} else if paths := sessionWorkspaces(&remote.Target{Session: "session", Alpha: true}); len(paths) != 0 {
		t.Fatal("workspace of another endpoint listed")
	}

	// Retired workspaces are kept, but not listed, until they expire, as are
	// staging directories.
	backup, err := w.retire()
	if err != nil {
		t.Fatal(err)
	} else if _, err := loadWorkspace(local); !os.IsNotExist(err) {
		t.Fatal("retired workspace still loaded:", err)
	} else if paths := sessionWorkspaces(&remote.Target{Session: "session"}); len(paths) != 0 {
		t.Fatal("retired workspace listed")
	}
	w.directory = directory
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	} else if err := w.save(); err != nil {
		t.Fatal(err)
	} else if again, err := w.retire(); err != nil || again == backup {
		t.Fatalf("repeated retirement failed: %v", err)
	}
	staging, err := os.MkdirTemp(base, stagingPrefix)
	if err != nil {
		t.Fatal(err)
	}
	pruneWorkspaces()
	if _, err := os.Stat(filepath.Join(backup, metadataName)); err != nil {
		t.Fatal("recent backup pruned:", err)
	} else if _, err := os.Stat(staging); err != nil {
		t.Fatal("recent staging directory pruned:", err)
	}
	expired := filepath.Join(filepath.Dir(backup), fmt.Sprintf("%d-expired", time.Now().Add(-backupRetention-time.Hour).UnixNano()))
	if err := os.Mkdir(expired, 0700); err != nil {
		t.Fatal(err)
	} else if err := os.Chtimes(staging, time.Now().Add(-stagingRetention-time.Hour), time.Now().Add(-stagingRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	pruneWorkspaces()
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatal("expired backup kept:", err)
	} else if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("abandoned staging directory kept:", err)
	} else if _, err := os.Stat(backup); err != nil {
		t.Fatal("recent backup pruned:", err)
	}
}
