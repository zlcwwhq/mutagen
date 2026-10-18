package cli

import (
	"context"
	"crypto/sha1"
	"fmt"
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
	} else if result, err := describe(w, "file", false); err != nil || result.Regions != 0 || result.Base != w.file(baseName) {
		t.Fatalf("clean merge not described: %+v: %v", result, err)
	} else if _, _, err := resolution(context.Background(), w, &options{}); err != nil {
		t.Fatal("clean merge not applicable:", err)
	}
	w = newTestWorkspaceWithBase(t, &workspace{Path: "file", Token: "token", directory: t.TempDir()}, "local\n", "remote\n", "base\n")
	if result, err := describe(w, "file", false); err != nil || result.Regions != 1 {
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
	conflict := &remote.ConflictRoot{Path: "file", Files: true, BaseDigest: fmt.Sprintf("%x", sha1.Sum([]byte("base\n"))), BaseAlgorithm: "sha1"}
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
	} else if paths := sessionWorkspaces(&remote.Target{Session: "session"}); !paths["file"] {
		t.Fatal("workspace not listed")
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
