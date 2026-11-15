package integration

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// newGitRootSession creates a session in Git mode whose alpha root is a
// repository with a single commit, on a branch named main, of the files in
// options.alpha (or of a single file if there are none). It returns the
// session and the commit.
func newGitRootSession(t *testing.T, options gitModeOptions) (*gitModeSession, string) {
	t.Helper()
	isolateGitMode(t)
	if options.alpha == nil {
		options.alpha = map[string]string{"file": "first"}
	}
	var first string
	prepare := options.prepare
	options.prepare = func(session *gitModeSession) {
		gitModeRun(t, session.alpha, "init", "--quiet", "--initial-branch=main")
		gitModeRun(t, session.alpha, "add", "--all")
		gitModeRun(t, session.alpha, "commit", "--quiet", "--no-gpg-sign", "-m", "first")
		first = gitModeHead(t, session.alpha)
		if prepare != nil {
			prepare(session)
		}
	}
	return newGitModeSession(t, options), first
}

// gitModeBranch returns the branch that a repository has checked out, or an
// empty string if its HEAD is detached.
func gitModeBranch(t *testing.T, directory string) string {
	t.Helper()
	branch, err := gitModeTry(directory, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return branch
}

// expectRepository verifies that the repository at a root has a commit
// checked out on a branch, and that Git considers its working tree and index
// unchanged from that commit.
func (s *gitModeSession) expectRepository(root, commit, branch string) {
	s.t.Helper()
	if head, err := gitModeTry(root, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		s.t.Fatalf("no repository at %s: %v", root, err)
	} else if head != commit {
		s.t.Errorf("incorrect commit at %s: %s instead of %s", root, head, commit)
	}
	if current := gitModeBranch(s.t, root); current != branch {
		s.t.Errorf("incorrect branch at %s: %q instead of %q", root, current, branch)
	}
	if status := gitModeRun(s.t, root, "status", "--porcelain"); status != "" {
		s.t.Errorf("repository at %s differs from its commit:\n%s", root, status)
	}
}

// TestGitModeRootRepository tests that an endpoint without a repository at its
// root gets the other endpoint's, with its history, branch, and origin, and
// that commits and branch switches on either endpoint are followed by the
// other, without being pushed anywhere.
func TestGitModeRootRepository(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			session, first := newGitRootSession(t, gitModeOptions{
				remoteBeta: remote,
				prepare: func(session *gitModeSession) {
					gitModeRun(t, session.alpha, "remote", "add", "origin", "https://user:secret@example.org/project.git")
				},
			})

			// Beta gets a repository with alpha's commit on the same branch,
			// and alpha's origin without its credentials.
			session.flush()
			session.expectRepository(session.beta, first, "main")
			if url := gitModeRun(t, session.beta, "config", "remote.origin.url"); url != "https://example.org/project.git" {
				t.Errorf("incorrect origin URL on beta: %q", url)
			}

			// A commit on alpha is followed by beta.
			second := gitModeCommit(t, session.alpha, "file", "second")
			session.flush()
			session.expect(session.beta, map[string]string{"file": "second"})
			session.expectRepository(session.beta, second, "main")

			// A commit on beta is followed by alpha, which didn't have it.
			third := gitModeCommit(t, session.beta, "other", "third")
			session.flush()
			session.expect(session.alpha, map[string]string{"other": "third"})
			session.expectRepository(session.alpha, third, "main")
			if log := gitModeRun(t, session.alpha, "log", "--format=%s"); log != "commit other\ncommit file\nfirst" {
				t.Errorf("incorrect history on alpha:\n%s", log)
			}

			// A new branch on beta is followed by alpha.
			gitModeRun(t, session.beta, "switch", "--quiet", "--create", "topic")
			session.flush()
			session.expectRepository(session.alpha, third, "topic")
			if main := gitModeRun(t, session.alpha, "rev-parse", "main"); main != third {
				t.Errorf("main moved on alpha: %s", main)
			}

			// There's nothing left to do.
			session.verifySettled()
		})
	}
}

// TestGitModeRootRepositoryDivergence tests that commits made on both
// endpoints before either follows the other are a conflict, during which files
// are still synchronized, and that resetting one endpoint to the other's
// commit and committing again resolves it.
func TestGitModeRootRepositoryDivergence(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			session, _ := newGitRootSession(t, gitModeOptions{remoteBeta: remote})
			session.flush()

			// Both endpoints commit.
			alpha := gitModeCommit(t, session.alpha, "a", "a")
			beta := gitModeCommit(t, session.beta, "b", "b")
			state := session.flushAllowingProblems()
			conflict := state.RepositoryConflict
			if conflict == nil {
				t.Fatal("divergence not reported as a conflict")
			} else if hex.EncodeToString(conflict.Alpha.Commit) != alpha || hex.EncodeToString(conflict.Beta.Commit) != beta {
				t.Errorf("incorrect commits in conflict: %x, %x", conflict.Alpha.Commit, conflict.Beta.Commit)
			}

			// Files are still synchronized, but neither HEAD moves.
			session.expect(session.alpha, map[string]string{"a": "a", "b": "b"})
			session.expect(session.beta, map[string]string{"a": "a", "b": "b"})
			if head := gitModeHead(t, session.alpha); head != alpha {
				t.Errorf("alpha's HEAD moved to %s", head)
			}
			if head := gitModeHead(t, session.beta); head != beta {
				t.Errorf("beta's HEAD moved to %s", head)
			}

			// Beta has alpha's commit, so it can reset to it, which keeps its
			// files, and commit again, which resolves the conflict.
			gitModeRun(t, session.beta, "reset", "--quiet", alpha)
			again := gitModeCommitFiles(t, session.beta, map[string]string{"b": "b"}, "b again")
			state = session.flushAllowingProblems()
			if state.RepositoryConflict != nil {
				t.Fatal("conflict not resolved")
			}
			session.expectRepository(session.alpha, again, "main")
			session.expectRepository(session.beta, again, "main")
			session.verifySettled()
		})
	}
}

// TestGitModeRootRepositoryStagedChanges tests that following a commit keeps
// the changes that the following endpoint has staged.
func TestGitModeRootRepositoryStagedChanges(t *testing.T) {
	session, _ := newGitRootSession(t, gitModeOptions{})
	session.flush()

	// Stage a new file on beta, and commit another one on alpha.
	session.write(session.beta, map[string]string{"staged": "staged"})
	gitModeRun(t, session.beta, "add", "staged")
	second := gitModeCommit(t, session.alpha, "file", "second")
	session.flush()

	// Beta follows and still has the file staged.
	if head := gitModeHead(t, session.beta); head != second {
		t.Fatalf("beta didn't follow: %s", head)
	}
	if staged := gitModeRun(t, session.beta, "diff", "--cached", "--name-only"); staged != "staged" {
		t.Errorf("incorrect staged changes on beta: %q", staged)
	}
}

// TestGitModeRootRepositoryPolling tests that a commit that doesn't change any
// file wakes a session that polls for changes, although the session doesn't
// scan .git.
func TestGitModeRootRepositoryPolling(t *testing.T) {
	session, _ := newGitRootSession(t, gitModeOptions{
		watch:     true,
		watchMode: synchronization.WatchMode_WatchModeForcePoll,
	})
	session.flush()

	// Commit on beta without changing any file, and wait for alpha to follow
	// without flushing.
	gitModeRun(t, session.beta, "commit", "--quiet", "--no-gpg-sign", "--allow-empty", "-m", "empty")
	commit := gitModeHead(t, session.beta)
	for deadline := time.Now().Add(30 * time.Second); !gitModeHeadIs(session.alpha, commit); {
		if time.Now().After(deadline) {
			t.Fatal("alpha didn't follow beta's commit")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestGitModeRootRepositoryNotOverwritten tests that a repository that an
// endpoint has at its root, with unrelated history, is never replaced, and
// that the disagreement is a conflict even in modes that would otherwise
// follow alpha, because the endpoints never agreed.
func TestGitModeRootRepositoryNotOverwritten(t *testing.T) {
	var unrelated string
	session, _ := newGitRootSession(t, gitModeOptions{
		mode: core.SynchronizationMode_SynchronizationModeTwoWayResolved,
		prepare: func(session *gitModeSession) {
			gitModeRun(t, session.beta, "init", "--quiet", "--initial-branch=main")
			unrelated = gitModeCommit(t, session.beta, "unrelated", "unrelated")
		},
	})
	state := session.flushAllowingProblems()
	if state.RepositoryConflict == nil {
		t.Error("unrelated histories not reported as a conflict")
	}
	if head := gitModeHead(t, session.beta); head != unrelated {
		t.Errorf("beta's repository moved to %s", head)
	}
	if _, err := os.Stat(filepath.Join(session.beta, ".git", "HEAD")); err != nil {
		t.Error("beta's repository affected:", err)
	}
}
