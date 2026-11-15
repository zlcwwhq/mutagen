package local

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

const (
	// rootOrigin is the origin of the source repositories of tests, which
	// includes credentials, which aren't passed on.
	rootOrigin = "https://user:secret@example.org/repository.git"
	// rootOriginWithoutCredentials is rootOrigin without credentials.
	rootOriginWithoutCredentials = "https://example.org/repository.git"
)

// rootBigFile returns the contents of a large file that doesn't compress well,
// so that a pack that includes it is much larger than one that only includes
// a change to it.
func rootBigFile() string {
	random := rand.New(rand.NewPCG(1, 2))
	var builder strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&builder, "%016x\n", random.Uint64())
	}
	return builder.String()
}

// rootFixture is a pair of endpoints in Git mode for testing the
// synchronization of the repositories at their roots, between a source, whose
// repository the follower's follows, and the follower. Neither watches.
type rootFixture struct {
	// t is the test.
	t *testing.T
	// base is the directory that contains the roots.
	base string
	// sourceRoot and followerRoot are the roots of the endpoints.
	sourceRoot, followerRoot string
	// source and follower are the endpoints.
	source, follower *endpoint
	// first is the hexadecimal identifier of the source's first commit, on
	// its main branch, which the follower's repository has checked out as well
	// if it was cloned from the source.
	first string
}

// newRootFixture creates a source repository with a single commit and an
// origin, and a follower whose root has the same files, which, if clone is
// true, is a clone of the source.
func newRootFixture(t *testing.T, clone bool) *rootFixture {
	t.Helper()
	isolateGit(t)
	base := realTemporaryDirectory(t)
	f := &rootFixture{
		t:            t,
		base:         base,
		sourceRoot:   filepath.Join(base, "source"),
		followerRoot: filepath.Join(base, "follower"),
	}
	initializeRepository(t, f.sourceRoot, "--initial-branch=main")
	mustGit(t, f.sourceRoot, "config", "remote.origin.url", rootOrigin)
	files := map[string]string{"file": "first", "big": rootBigFile()}
	writeTree(t, f.sourceRoot, files)
	mustGit(t, f.sourceRoot, "add", "--", "file", "big")
	mustGit(t, f.sourceRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "first")
	f.first = mustGit(t, f.sourceRoot, "rev-parse", "HEAD")
	if clone {
		mustGit(t, base, "clone", "--quiet", "--", f.sourceRoot, f.followerRoot)
	} else {
		writeTree(t, f.followerRoot, files)
	}
	f.source = newGitModeEndpoint(t, f.sourceRoot, synchronization.WatchMode_WatchModeNoWatch)
	f.follower = newGitModeEndpoint(t, f.followerRoot, synchronization.WatchMode_WatchModeNoWatch)
	return f
}

// commit creates a commit in the source repository with the specified files
// (or an empty one if there are none), and writes the files to the follower's
// root as well, as the synchronization of its content would. It returns the
// commit's identifier.
func (f *rootFixture) commit(message string, files map[string]string) string {
	f.t.Helper()
	writeTree(f.t, f.sourceRoot, files)
	writeTree(f.t, f.followerRoot, files)
	mustGit(f.t, f.sourceRoot, "add", "--all")
	mustGit(f.t, f.sourceRoot, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", message)
	return mustGit(f.t, f.sourceRoot, "rev-parse", "HEAD")
}

// rootFollow describes how the follower's repository followed the source's.
type rootFollow struct {
	// result is the result of the follower's transition.
	result *core.RepositoryResult
	// supplyProblem is the problem that the source reported, if any.
	supplyProblem *core.Problem
	// haves are the commits that the follower reported having.
	haves [][]byte
	// transferred indicates whether or not the follower requested a pack.
	transferred bool
	// packSize is the size of the pack.
	packSize int
}

// countingReceiver is a synchronization.RepositoryReceiver that measures a pack
// and passes it on to another receiver, if there is one.
type countingReceiver struct {
	// receiver is the receiver, or nil.
	receiver synchronization.RepositoryReceiver
	// size is the size of the pack so far.
	size int
}

// Receive implements synchronization.RepositoryReceiver.Receive.
func (r *countingReceiver) Receive(data []byte) error {
	r.size += len(data)
	if r.receiver != nil {
		return r.receiver.Receive(data)
	}
	return nil
}

// Finish implements synchronization.RepositoryReceiver.Finish.
func (r *countingReceiver) Finish(problem string) error {
	if r.receiver != nil {
		return r.receiver.Finish(problem)
	}
	return nil
}

// follow performs the steps by which the controller moves the follower's
// HEAD to the source's commit and branch: it scans both endpoints, stages on
// the follower, supplies from the source, calls beforeTransition (if it isn't
// nil), and transitions the follower.
func (f *rootFixture) follow(policy core.RepositoryPolicy, beforeTransition func()) *rootFollow {
	f.t.Helper()
	ctx := context.Background()

	// Scan both endpoints and create the action.
	target := scanEndpoint(f.t, f.source, true).Repository
	if target == nil || target.Unavailable != "" {
		f.t.Fatalf("source repository unusable: %v", target)
	}
	old := scanEndpoint(f.t, f.follower, true).Repository
	if old != nil && old.Unavailable != "" {
		f.t.Fatalf("follower repository unusable: %v", old)
	}
	action := &core.RepositoryAction{Old: old, Target: target, Policy: policy}

	// Stage and supply.
	result := &rootFollow{}
	haves, receiver, err := f.follower.StageRepository(ctx, action)
	if err != nil {
		f.t.Fatal("unable to stage:", err)
	}
	result.haves = haves
	if receiver != nil {
		result.transferred = true
		counter := &countingReceiver{receiver: receiver}
		result.supplyProblem, err = f.source.SupplyRepository(ctx, target.Commit, haves, counter)
		if err != nil {
			f.t.Fatal("unable to supply:", err)
		}
		result.packSize = counter.size
	}

	// Transition.
	if beforeTransition != nil {
		beforeTransition()
	}
	result.result, err = f.follower.TransitionRepository(ctx, action)
	if err != nil {
		f.t.Fatal("unable to transition:", err)
	} else if err := result.result.EnsureValid(); err != nil {
		f.t.Fatal("invalid result:", err)
	}
	verifyNoTemporaryContent(f.t, f.followerRoot)
	return result
}

// verifyRootOutcome verifies the outcome of a follow, along with its problem:
// none if no contents are specified, or otherwise one with the specified
// severity whose message has the specified contents.
func verifyRootOutcome(t *testing.T, result *core.RepositoryResult, outcome core.RepositoryOutcome, warning bool, contents ...string) {
	t.Helper()
	if result.Outcome != outcome {
		t.Errorf("incorrect outcome: %s != %s (%v)", result.Outcome, outcome, result.Problem)
	}
	if len(contents) == 0 {
		if result.Problem != nil {
			t.Error("unexpected problem:", result.Problem)
		}
		return
	} else if result.Problem == nil {
		t.Fatal("no problem reported")
	} else if result.Problem.Path != ".git" {
		t.Errorf("incorrect problem path: %q", result.Problem.Path)
	} else if result.Problem.Warning != warning {
		t.Errorf("incorrect problem severity (warning: %t): %s", result.Problem.Warning, result.Problem.Error)
	}
	for _, content := range contents {
		if !strings.Contains(result.Problem.Error, content) {
			t.Errorf("problem doesn't contain %q: %s", content, result.Problem.Error)
		}
	}
}

// verifyHead verifies the commit and branch (empty if HEAD should be
// detached) that HEAD identifies in a repository.
func verifyHead(t *testing.T, directory, commit, branch string) {
	t.Helper()
	if head := mustGit(t, directory, "rev-parse", "HEAD"); head != commit {
		t.Errorf("incorrect HEAD commit: %s != %s", head, commit)
	}
	symbolic, err := runGit(directory, "symbolic-ref", "--quiet", "HEAD")
	if branch == "" && err == nil {
		t.Errorf("HEAD isn't detached: %s", symbolic)
	} else if branch != "" && symbolic != "refs/heads/"+branch {
		t.Errorf("HEAD doesn't refer to branch %s: %q, %v", branch, symbolic, err)
	}
}

// workingTree reads the files in a root, apart from its Git directory.
func workingTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if entry.Name() == ".git" {
			return filepath.SkipDir
		} else if entry.Type().IsRegular() {
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, _ := filepath.Rel(root, path)
			files[filepath.ToSlash(relative)] = string(contents)
		}
		return nil
	})
	if err != nil {
		t.Fatal("unable to read working tree:", err)
	}
	return files
}

// verifyUntouched verifies that the working tree of a root is the same as it
// was, and that the repository at the root doesn't have ORIG_HEAD, which
// operations like reset write, but which following mustn't.
func verifyUntouched(t *testing.T, root string, before map[string]string) {
	t.Helper()
	if after := workingTree(t, root); !maps.Equal(before, after) {
		t.Error("working tree changed")
	}
	if _, err := os.Lstat(filepath.Join(root, ".git", "ORIG_HEAD")); err == nil {
		t.Error("ORIG_HEAD written")
	}
}

// TestEndpointGitModeRootScan tests the description of the repository at the
// root in snapshots.
func TestEndpointGitModeRootScan(t *testing.T) {
	f := newRootFixture(t, false)

	// The source's repository is described, without its origin's credentials,
	// while the follower doesn't have one.
	snapshot := scanEndpoint(t, f.source, true)
	if repository := snapshot.Repository; repository == nil {
		t.Fatal("repository not found")
	} else if repository.Unavailable != "" {
		t.Fatal("repository unavailable:", repository.Unavailable)
	} else if fmt.Sprintf("%x", repository.Commit) != f.first || repository.Branch != "main" {
		t.Errorf("incorrect commit or branch: %x %q", repository.Commit, repository.Branch)
	} else if repository.Url != rootOriginWithoutCredentials {
		t.Error("incorrect URL:", repository.Url)
	}
	if repository := scanEndpoint(t, f.follower, true).Repository; repository != nil {
		t.Error("repository found without .git entry:", repository)
	}

	// The scan's own snapshot doesn't have the repository, since it's the
	// baseline of later scans.
	f.source.lockScanLock(context.Background())
	baseline := f.source.snapshot
	f.source.unlockScanLock()
	if baseline.Repository != nil {
		t.Error("repository stored in the scan's snapshot")
	}

	// An operation in progress makes the repository unavailable.
	rebase := filepath.Join(f.sourceRoot, ".git", "rebase-merge")
	if err := os.Mkdir(rebase, 0700); err != nil {
		t.Fatal(err)
	}
	if repository := scanEndpoint(t, f.source, true).Repository; repository == nil || !strings.Contains(repository.Unavailable, "a rebase is in progress") {
		t.Error("repository available during rebase:", repository)
	}
	if err := os.Remove(rebase); err != nil {
		t.Fatal(err)
	}

	// So does a .git entry that isn't a valid repository.
	if err := os.Mkdir(filepath.Join(f.followerRoot, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if repository := scanEndpoint(t, f.follower, true).Repository; repository == nil || !strings.Contains(repository.Unavailable, "isn't a valid repository") {
		t.Error("invalid repository not unavailable:", repository)
	}
}

// TestEndpointGitModeRootAttach tests the attachment of the source's
// repository to a root without a .git entry.
func TestEndpointGitModeRootAttach(t *testing.T) {
	// A repository is attached with the source's commit and branch, including
	// commits that only the source has (the origin isn't real), its origin,
	// and its line ending settings, and with an index that matches the files.
	t.Run("attached", func(t *testing.T) {
		f := newRootFixture(t, false)
		mustGit(t, f.sourceRoot, "config", "core.autocrlf", "input")
		mustGit(t, f.sourceRoot, "config", "core.eol", "lf")
		second := f.commit("second", map[string]string{"file": "second", "directory/new": "new"})
		before := workingTree(t, f.followerRoot)
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		if !follow.transferred || len(follow.haves) > 0 {
			t.Errorf("incorrect transfer: %t, %x", follow.transferred, follow.haves)
		}
		verifyHead(t, f.followerRoot, second, "main")
		verifyUntouched(t, f.followerRoot, before)
		if status := mustGit(t, f.followerRoot, "status", "--porcelain"); status != "" {
			t.Errorf("working tree isn't clean: %s", status)
		}
		if parent := mustGit(t, f.followerRoot, "rev-parse", second+"^"); parent != f.first {
			t.Errorf("incorrect history: %s", parent)
		}
		if url := mustGit(t, f.followerRoot, "config", "remote.origin.url"); url != rootOriginWithoutCredentials {
			t.Error("incorrect origin:", url)
		} else if fetch := mustGit(t, f.followerRoot, "config", "remote.origin.fetch"); fetch != gitRootFetchRefspec {
			t.Error("incorrect fetch refspec:", fetch)
		}
		if autoCRLF, eol := mustGit(t, f.followerRoot, "config", "core.autocrlf"), mustGit(t, f.followerRoot, "config", "core.eol"); autoCRLF != "input" || eol != "lf" {
			t.Errorf("incorrect line ending settings: %q %q", autoCRLF, eol)
		}
		if message := mustGit(t, f.followerRoot, "reflog", "-1", "--format=%gs", "refs/heads/main"); message != gitRootAttachMessage {
			t.Errorf("incorrect reflog message: %q", message)
		}
		if repository := scanEndpoint(t, f.follower, true).Repository; !repository.Equal(scanEndpoint(t, f.source, true).Repository) {
			t.Error("repositories differ after attachment:", repository)
		}
	})

	// A detached HEAD is attached as such.
	t.Run("detached", func(t *testing.T) {
		f := newRootFixture(t, false)
		mustGit(t, f.sourceRoot, "checkout", "--quiet", "--detach")
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, f.first, "")
	})

	// A repository with SHA-256 object identifiers is attached with the same
	// object format.
	t.Run("SHA-256", func(t *testing.T) {
		f := newRootFixture(t, false)
		source := filepath.Join(f.base, "sha256")
		if err := os.MkdirAll(source, 0700); err != nil {
			t.Fatal(err)
		} else if _, err := runGit(source, "init", "--quiet", "--object-format=sha256", "--initial-branch=main"); err != nil {
			t.Skip("SHA-256 repositories aren't supported:", err)
		}
		writeTree(t, source, workingTree(t, f.followerRoot))
		mustGit(t, source, "add", "--all")
		mustGit(t, source, "commit", "--quiet", "--no-gpg-sign", "-m", "first")
		head := mustGit(t, source, "rev-parse", "HEAD")
		f.sourceRoot, f.source = source, newGitModeEndpoint(t, source, synchronization.WatchMode_WatchModeNoWatch)
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, head, "main")
		if format := mustGit(t, f.followerRoot, "rev-parse", "--show-object-format"); format != "sha256" {
			t.Error("incorrect object format:", format)
		}
	})

	// A .git entry that appears in the mean time is left alone, and so is
	// the root, apart from the temporary repository, which is removed.
	t.Run("appeared", func(t *testing.T) {
		f := newRootFixture(t, false)
		follow := f.follow(core.RepositoryPolicy_Follow, func() {
			initializeRepository(t, f.followerRoot, "--initial-branch=other")
		})
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Failed, true, "appeared")
		if branch := mustGit(t, f.followerRoot, "symbolic-ref", "HEAD"); branch != "refs/heads/other" {
			t.Error("repository replaced:", branch)
		}
	})

	// If the root doesn't exist when the commits are requested, then the
	// repository is attached in the next cycle.
	t.Run("root created", func(t *testing.T) {
		f := newRootFixture(t, false)
		if err := os.RemoveAll(f.followerRoot); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		action := &core.RepositoryAction{Target: scanEndpoint(t, f.source, true).Repository}
		if _, receiver, err := f.follower.StageRepository(ctx, action); err != nil || receiver != nil {
			t.Fatalf("staging didn't skip the transfer: %v, %v", receiver, err)
		}
		writeTree(t, f.followerRoot, map[string]string{"file": "first"})
		result, err := f.follower.TransitionRepository(ctx, action)
		if err != nil {
			t.Fatal("unable to transition:", err)
		}
		verifyRootOutcome(t, result, core.RepositoryOutcome_Failed, true, "didn't exist")
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
	})
}

// TestEndpointGitModeRootFollow tests that HEAD follows the source's commit on
// the same branch, keeping what's staged where possible.
func TestEndpointGitModeRootFollow(t *testing.T) {
	// HEAD and the branch move, while the working tree is left alone, and the
	// index matches the new commit. The source leaves out the history that the
	// follower has, and changes to it, so the pack is thin.
	t.Run("same branch", func(t *testing.T) {
		f := newRootFixture(t, true)
		second := f.commit("second", map[string]string{"big": rootBigFile() + "more\n"})
		before := workingTree(t, f.followerRoot)
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, second, "main")
		verifyUntouched(t, f.followerRoot, before)
		if status := mustGit(t, f.followerRoot, "status", "--porcelain"); status != "" {
			t.Errorf("working tree isn't clean: %s", status)
		}
		if message := mustGit(t, f.followerRoot, "reflog", "-1", "--format=%gs", "refs/heads/main"); message != gitRootFollowMessage {
			t.Errorf("incorrect reflog message: %q", message)
		}
		if len(follow.haves) != 1 || fmt.Sprintf("%x", follow.haves[0]) != f.first {
			t.Errorf("incorrect haves: %x", follow.haves)
		}

		// A full pack is much larger.
		full := &countingReceiver{}
		if problem, err := f.source.SupplyRepository(context.Background(), follow.haves[0], nil, full); err != nil || problem != nil {
			t.Fatal("unable to supply full pack:", problem, err)
		} else if follow.packSize == 0 || follow.packSize*4 > full.size {
			t.Errorf("pack isn't thin: %d bytes, full pack %d bytes", follow.packSize, full.size)
		}

		// Now that the follower has the commit, nothing is transferred.
		third := f.commit("third", nil)
		mustGit(t, f.followerRoot, "fetch", "--quiet", "origin")
		follow = f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		if follow.transferred {
			t.Error("commit transferred, even though the follower had it")
		}
		verifyHead(t, f.followerRoot, third, "main")
	})

	// Staged changes are kept, unless they conflict with the change, in which
	// case they're unstaged, with a warning.
	t.Run("staged changes", func(t *testing.T) {
		f := newRootFixture(t, true)
		writeTree(t, f.followerRoot, map[string]string{"file": "staged"})
		mustGit(t, f.followerRoot, "add", "file")
		second := f.commit("second", map[string]string{"big": "changed"})
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, second, "main")
		if staged := mustGit(t, f.followerRoot, "diff", "--cached", "--name-only"); staged != "file" {
			t.Errorf("staged changes not kept: %q", staged)
		}

		writeTree(t, f.followerRoot, map[string]string{"big": "staged"})
		mustGit(t, f.followerRoot, "add", "big")
		third := f.commit("third", map[string]string{"big": "changed again"})
		follow = f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, true, "unstaged")
		verifyHead(t, f.followerRoot, third, "main")
		if staged := mustGit(t, f.followerRoot, "diff", "--cached", "--name-only"); staged != "" {
			t.Errorf("staged changes kept: %q", staged)
		}
	})

	// HEAD follows from an unborn branch.
	t.Run("unborn", func(t *testing.T) {
		f := newRootFixture(t, false)
		initializeRepository(t, f.followerRoot, "--initial-branch=main")
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, f.first, "main")
		if status := mustGit(t, f.followerRoot, "status", "--porcelain"); status != "" {
			t.Errorf("working tree isn't clean: %s", status)
		}
	})
}

// TestEndpointGitModeRootBranches tests that HEAD follows the source to
// another branch, or detaches, without losing commits.
func TestEndpointGitModeRootBranches(t *testing.T) {
	// A branch that doesn't exist is created, and one that the target commit
	// contains is fast-forwarded, while the old branch stays where it was.
	t.Run("switch", func(t *testing.T) {
		for _, existing := range []bool{false, true} {
			f := newRootFixture(t, true)
			if existing {
				mustGit(t, f.followerRoot, "branch", "feature")
			}
			mustGit(t, f.sourceRoot, "checkout", "--quiet", "-b", "feature")
			second := f.commit("second", map[string]string{"file": "second"})
			before := workingTree(t, f.followerRoot)
			follow := f.follow(core.RepositoryPolicy_Follow, nil)
			verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
			verifyHead(t, f.followerRoot, second, "feature")
			verifyUntouched(t, f.followerRoot, before)
			if main := mustGit(t, f.followerRoot, "rev-parse", "refs/heads/main"); main != f.first {
				t.Error("old branch moved:", main)
			}
		}
	})

	// A branch that has commits that the target doesn't is left alone.
	t.Run("collision", func(t *testing.T) {
		f := newRootFixture(t, true)
		mustGit(t, f.followerRoot, "checkout", "--quiet", "-b", "feature")
		mustGit(t, f.followerRoot, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "local")
		local := mustGit(t, f.followerRoot, "rev-parse", "HEAD")
		mustGit(t, f.followerRoot, "checkout", "--quiet", "main")
		mustGit(t, f.sourceRoot, "checkout", "--quiet", "-b", "feature")
		f.commit("second", map[string]string{"file": "second"})
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Failed, false, "branch feature", "git branch -m")
		verifyHead(t, f.followerRoot, f.first, "main")
		if feature := mustGit(t, f.followerRoot, "rev-parse", "refs/heads/feature"); feature != local {
			t.Error("branch moved:", feature)
		}
	})

	// A branch that's checked out in another worktree is left alone.
	t.Run("other worktree", func(t *testing.T) {
		f := newRootFixture(t, true)
		mustGit(t, f.followerRoot, "worktree", "add", "--quiet", "-b", "feature", filepath.Join(f.base, "worktree"))
		mustGit(t, f.sourceRoot, "checkout", "--quiet", "-b", "feature")
		f.commit("second", map[string]string{"file": "second"})
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Failed, false, "branch feature", "another worktree")
		verifyHead(t, f.followerRoot, f.first, "main")
		if feature := mustGit(t, f.followerRoot, "rev-parse", "refs/heads/feature"); feature != f.first {
			t.Error("branch moved:", feature)
		}
	})

	// A detached HEAD is followed by detaching HEAD.
	t.Run("detach", func(t *testing.T) {
		f := newRootFixture(t, true)
		mustGit(t, f.sourceRoot, "checkout", "--quiet", "--detach")
		second := f.commit("second", map[string]string{"file": "second"})
		follow := f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, second, "")
		if main := mustGit(t, f.followerRoot, "rev-parse", "refs/heads/main"); main != f.first {
			t.Error("branch moved:", main)
		}
	})
}

// TestEndpointGitModeRootPolicies tests the policies with which HEAD follows.
func TestEndpointGitModeRootPolicies(t *testing.T) {
	// HEAD fast-forwards.
	t.Run("fast-forward", func(t *testing.T) {
		f := newRootFixture(t, true)
		second := f.commit("second", map[string]string{"file": "second"})
		follow := f.follow(core.RepositoryPolicy_FastForward, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, second, "main")
	})

	// HEAD doesn't move back to a commit that it contains.
	t.Run("ahead", func(t *testing.T) {
		f := newRootFixture(t, true)
		mustGit(t, f.followerRoot, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "local")
		local := mustGit(t, f.followerRoot, "rev-parse", "HEAD")
		for _, policy := range []core.RepositoryPolicy{core.RepositoryPolicy_FastForward, core.RepositoryPolicy_FastForwardOrFollow} {
			follow := f.follow(policy, nil)
			verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Ahead, false)
			if follow.transferred {
				t.Error("commit transferred, even though the follower had it")
			}
			verifyHead(t, f.followerRoot, local, "main")
		}
	})

	// HEAD only moves to a commit that diverged from its own if the policy
	// says so.
	t.Run("diverged", func(t *testing.T) {
		f := newRootFixture(t, true)
		mustGit(t, f.followerRoot, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "local")
		local := mustGit(t, f.followerRoot, "rev-parse", "HEAD")
		second := f.commit("second", map[string]string{"file": "second"})
		follow := f.follow(core.RepositoryPolicy_FastForward, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Diverged, false)
		verifyHead(t, f.followerRoot, local, "main")
		follow = f.follow(core.RepositoryPolicy_FastForwardOrFollow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, second, "main")
	})
}

// TestEndpointGitModeRootTransient tests that HEAD doesn't follow while the
// follower's repository is busy or has changed, with warnings that say so.
func TestEndpointGitModeRootTransient(t *testing.T) {
	// A commit since the scan is kept.
	t.Run("changed since scan", func(t *testing.T) {
		f := newRootFixture(t, true)
		f.commit("second", map[string]string{"file": "second"})
		var local string
		follow := f.follow(core.RepositoryPolicy_Follow, func() {
			mustGit(t, f.followerRoot, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "local")
			local = mustGit(t, f.followerRoot, "rev-parse", "HEAD")
		})
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Failed, true, "changed here since the scan")
		verifyHead(t, f.followerRoot, local, "main")
	})

	// Another command's lock on the index is respected, and HEAD follows once
	// it's gone.
	t.Run("index locked", func(t *testing.T) {
		f := newRootFixture(t, true)
		second := f.commit("second", map[string]string{"file": "second"})
		lock := filepath.Join(f.followerRoot, ".git", "index.lock")
		follow := f.follow(core.RepositoryPolicy_Follow, func() {
			writeTree(t, filepath.Join(f.followerRoot, ".git"), map[string]string{"index.lock": ""})
		})
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Failed, true, "index.lock")
		verifyHead(t, f.followerRoot, f.first, "main")
		if err := os.Remove(lock); err != nil {
			t.Fatal("lock removed:", err)
		}
		follow = f.follow(core.RepositoryPolicy_Follow, nil)
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Moved, false)
		verifyHead(t, f.followerRoot, second, "main")
	})

	// An operation that starts after the scan is respected as well.
	t.Run("operation in progress", func(t *testing.T) {
		f := newRootFixture(t, true)
		f.commit("second", map[string]string{"file": "second"})
		follow := f.follow(core.RepositoryPolicy_Follow, func() {
			if err := os.Mkdir(filepath.Join(f.followerRoot, ".git", "rebase-merge"), 0700); err != nil {
				t.Fatal(err)
			}
		})
		verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Failed, true, "a rebase is in progress")
		verifyHead(t, f.followerRoot, f.first, "main")
	})
}

// TestEndpointGitModeRootShallowSource tests that a shallow repository doesn't
// supply commits, and that the follower reports why it didn't get them.
func TestEndpointGitModeRootShallowSource(t *testing.T) {
	f := newRootFixture(t, false)
	f.commit("second", map[string]string{"file": "second"})
	shallow := filepath.Join(f.base, "shallow")
	mustGit(t, f.base, "clone", "--quiet", "--depth=1", "--", fileURL(f.sourceRoot), shallow)
	f.source = newGitModeEndpoint(t, shallow, synchronization.WatchMode_WatchModeNoWatch)
	follow := f.follow(core.RepositoryPolicy_Follow, nil)
	if follow.supplyProblem == nil || !strings.Contains(follow.supplyProblem.Error, "shallow") || follow.supplyProblem.Warning {
		t.Error("incorrect supply problem:", follow.supplyProblem)
	}
	verifyRootOutcome(t, follow.result, core.RepositoryOutcome_Failed, false, "shallow", "git fetch --unshallow")
	if _, err := os.Lstat(filepath.Join(f.followerRoot, ".git")); !os.IsNotExist(err) {
		t.Error("repository attached:", err)
	}
}

// TestEndpointGitModeRootPollHeadChange tests that poll-based watching wakes
// the session when HEAD changes at the root, even though no file changes.
func TestEndpointGitModeRootPollHeadChange(t *testing.T) {
	f := newRootFixture(t, false)
	e := newGitModeEndpoint(t, f.sourceRoot, synchronization.WatchMode_WatchModeForcePoll)

	// waitForPoll waits for the endpoint to signal changes, for up to the
	// specified duration, and returns whether or not it did.
	waitForPoll := func(timeout time.Duration) bool {
		select {
		case <-e.pollSignal.Signals():
			return true
		case <-time.After(timeout):
			return false
		}
	}

	// Wait until polling has settled, which takes a few polling intervals of
	// one second each, so that its baseline includes the repository.
	for deadline := time.Now().Add(30 * time.Second); waitForPoll(2500 * time.Millisecond); {
		if time.Now().After(deadline) {
			t.Fatal("polling didn't settle")
		}
	}

	// A commit wakes the session, as does a switch to another branch.
	mustGit(t, f.sourceRoot, "commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "empty")
	if !waitForPoll(30 * time.Second) {
		t.Fatal("commit didn't wake the session")
	}
	drainPollSignal(e)
	mustGit(t, f.sourceRoot, "checkout", "--quiet", "-b", "other")
	if !waitForPoll(30 * time.Second) {
		t.Fatal("branch switch didn't wake the session")
	}
}
