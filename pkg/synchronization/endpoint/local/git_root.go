package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

const (
	// gitRootProblemPath is the path of the problems that concern the
	// repository at the root, which is that of its Git entry.
	gitRootProblemPath = ".git"
	// gitRootTemporaryPrefix is the prefix of the name of the directory in
	// which a repository is prepared for attachment at the root, which is
	// followed by the session's identifier. The directory is in the root so
	// that its Git directory can be renamed into place, and its name hides it
	// from scans and watching.
	gitRootTemporaryPrefix = filesystem.TemporaryNamePrefix + "repository-"
	// gitRootFollowMessage is the reflog message for the changes that follow
	// the other endpoint's repository.
	gitRootFollowMessage = "mutagen: follow"
	// gitRootAttachMessage is the reflog message for the changes that set up a
	// repository for attachment.
	gitRootAttachMessage = "mutagen: attach"
	// gitRootFetchRefspec is the refspec that an attached repository's origin
	// fetches with, which is the one that a clone configures.
	gitRootFetchRefspec = "+refs/heads/*:refs/remotes/origin/*"
	// gitRootMaximumTextLength is the maximum length of the texts that are
	// included in a core.Repository, which is well within what
	// core.Repository.EnsureValid accepts.
	gitRootMaximumTextLength = 2048
	// gitIndexRenameAttempts is the number of times that renaming a new index
	// into place is attempted on Windows, where it fails while another process
	// has the index open, such as a Git command that only reads it.
	gitIndexRenameAttempts = 20
	// gitIndexRenameInterval is the interval between those attempts.
	gitIndexRenameInterval = 50 * time.Millisecond
)

// gitRootText converts text, such as an error message, into a form that can be
// included in a core.Repository or a core.Problem, which are transmitted using
// Protocol Buffers: valid UTF-8, without null bytes, and of limited length.
func gitRootText(text string) string {
	text = strings.ReplaceAll(text, "\x00", "")
	if len(text) > gitRootMaximumTextLength {
		text = text[:gitRootMaximumTextLength] + "..."
	}
	return strings.ToValidUTF8(text, "�")
}

// unavailableRootRepository returns a repository that's unavailable for the
// specified reason (see core.Repository.Unavailable).
func unavailableRootRepository(reason string) *core.Repository {
	return &core.Repository{Unavailable: gitRootText(reason)}
}

// rootRepository examines the repository at the root for a scan in Git mode
// (see core.Snapshot.Repository). If there's a repository, but it can't be
// examined, or if it's in a state in which HEAD mustn't be moved, then the
// result only says why, which makes the session leave the repositories at both
// endpoints alone for the time being.
func (e *endpoint) rootRepository(ctx context.Context) *core.Repository {
	// Examine the repository, if there is one.
	probed, err := git.ProbeRoot(ctx, e.root)
	if err != nil {
		return unavailableRootRepository(err.Error())
	} else if probed == nil {
		return nil
	}

	// Check for an operation in progress whose state moving HEAD and the index
	// would disrupt (see gitOperationInProgress). Since the session can't
	// move this repository's HEAD, it doesn't move the other endpoint's to
	// this one's commit either, which may well change before the operation is
	// over.
	operationCtx, cancel := context.WithTimeout(ctx, gitWarningTimeout)
	operation, err := gitOperationInProgress(operationCtx, e.root)
	cancel()
	if err != nil {
		return unavailableRootRepository(fmt.Sprintf("unable to examine the repository at the root: %v", err))
	} else if operation != "" {
		return unavailableRootRepository(fmt.Sprintf(
			"HEAD isn't synchronized with the other endpoint while %s here; it is again once that's over", operation,
		))
	}

	// Create the description, and make sure that it's valid, since an invalid
	// snapshot would end the session. Git's rules for branch names are
	// stricter than what core.Repository requires, except that they allow
	// names that aren't valid UTF-8, which can't be transmitted.
	repository := &core.Repository{
		Commit:   probed.Commit,
		Branch:   probed.Branch,
		Url:      probed.URL,
		AutoCRLF: probed.AutoCRLF,
		Eol:      probed.EOL,
	}
	if !utf8.ValidString(repository.Branch) {
		return unavailableRootRepository("the name of the branch that HEAD refers to at the root isn't valid UTF-8, so HEAD isn't synchronized with the other endpoint; rename the branch")
	} else if err := repository.EnsureValid(); err != nil {
		return unavailableRootRepository(fmt.Sprintf("HEAD can't be synchronized with the other endpoint: %v", err))
	}
	return repository
}

// rootHeadFingerprint returns a description of what HEAD identifies in the
// repository at the root, which changes whenever its commit or the branch that
// it refers to does, for poll-based watching, which doesn't see anything in
// the root's Git directory. It runs a single Git command if the root has a
// .git entry, and none otherwise. A failure is described as well, since it
// changes when the repository does, such as when the first commit is made in a
// new repository.
func (e *endpoint) rootHeadFingerprint(ctx context.Context) string {
	if _, err := os.Lstat(filepath.Join(e.root, ".git")); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, gitWarningTimeout)
	defer cancel()
	output, err := git.Run(ctx, e.root, "rev-parse", "HEAD", "--symbolic-full-name", "HEAD")
	if err != nil {
		return "failed: " + err.Error()
	}
	return output
}

// temporaryRepositoryPath returns the path of the directory in which a
// repository is prepared for attachment at the root. It's specific to the
// session, since the roots of sessions can coincide (such as those of a star's
// edges at its center).
func (e *endpoint) temporaryRepositoryPath() string {
	return filepath.Join(e.root, gitRootTemporaryPrefix+e.gitProvider.session)
}

// gitRootTarget describes the commit and branch that a repository identifies,
// for use in messages.
func gitRootTarget(repository *core.Repository) string {
	commit := gitShortCommit(hex.EncodeToString(repository.Commit))
	if repository.Branch == "" {
		return "commit " + commit + " (with HEAD detached)"
	}
	return "commit " + commit + " on branch " + repository.Branch
}

// gitRootResult creates the result of a repository action, with a problem that
// has the specified message, if it isn't empty.
func gitRootResult(outcome core.RepositoryOutcome, message string, warning bool) *core.RepositoryResult {
	result := &core.RepositoryResult{Outcome: outcome}
	if message != "" {
		result.Problem = &core.Problem{Path: gitRootProblemPath, Error: gitRootText(message), Warning: warning}
	}
	return result
}

// gitHasHistory determines whether or not the repository in a directory has a
// commit, specified by its hexadecimal identifier, along with everything that
// it needs (its history, and their trees and contents), apart from what the
// repository's references need, which it's assumed to have. This is what a
// fetch verifies before it updates references, and the walk only covers what
// the references don't, which is usually only a few commits. A repository that
// doesn't have something that the commit needs can't move HEAD to it, since
// Git would fail to read it. Any failure counts as not having the commit.
func gitHasHistory(ctx context.Context, directory, commit string) bool {
	_, err := git.Run(ctx, directory, "rev-list", "--objects", "--quiet", commit+"^{commit}", "--not", "--all")
	return err == nil
}

// gitIsAncestor determines whether or not one commit, specified by its
// hexadecimal identifier, is an ancestor of (or the same as) another in the
// repository in a directory.
func gitIsAncestor(ctx context.Context, directory, ancestor, descendant string) (bool, error) {
	_, err := git.Run(ctx, directory, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	} else if git.ExitedWith(err, 1) {
		return false, nil
	}
	return false, err
}

// gitCheckBranchName verifies that a branch name, which comes from the other
// endpoint, is one that Git accepts for a branch. Git expands some special
// syntax (such as @{-1}) in the name, which only yields the same name if
// there's nothing to expand.
func gitCheckBranchName(ctx context.Context, directory, branch string) error {
	output, err := git.Run(ctx, directory, "check-ref-format", "--branch", branch)
	if err != nil {
		return err
	} else if output != branch {
		return fmt.Errorf("Git interprets %q as %q", branch, output)
	}
	return nil
}

// gitBranchWorktree returns the path of a worktree of the repository in a
// directory in which a branch is checked out, or an empty string if there's
// none.
func gitBranchWorktree(ctx context.Context, directory, branch string) (string, error) {
	output, err := git.Run(ctx, directory, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	var worktree string
	for _, line := range strings.Split(output, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			worktree = path
		} else if line == "branch refs/heads/"+branch {
			return worktree, nil
		}
	}
	return "", nil
}

// attachRootRepository attaches the repository that StageRepository prepared
// to the root, which doesn't have a .git entry, for TransitionRepository.
func (e *endpoint) attachRootRepository(ctx context.Context, target *core.Repository) *core.RepositoryResult {
	// Create a function to report failures.
	failure := func(warning bool, format string, arguments ...any) *core.RepositoryResult {
		return gitRootResult(core.RepositoryOutcome_Failed, fmt.Sprintf(
			"the other endpoint's repository (at %s) wasn't attached here, because %s",
			gitRootTarget(target), fmt.Sprintf(format, arguments...),
		), warning)
	}

	// If a .git entry has appeared at the root since the scan, then it's not
	// ours to replace. The next scan finds it, and HEAD then follows instead.
	gitEntry := filepath.Join(e.root, ".git")
	if _, err := os.Lstat(gitEntry); err == nil {
		return failure(true, "a .git entry appeared at the root since the scan; the next synchronization cycle takes it into account")
	} else if !os.IsNotExist(err) {
		return failure(false, "it couldn't be determined whether the root has a .git entry: %v", err)
	}

	// Verify that the prepared repository exists and has the commit. If it
	// doesn't exist, then either preparing it failed, or the root didn't exist
	// when it would have been prepared, which only delays attachment until the
	// next cycle.
	temporary := e.temporaryRepositoryPath()
	commit := hex.EncodeToString(target.Commit)
	if _, err := os.Lstat(temporary); err != nil {
		if e.repositoryProblem != "" {
			return failure(false, "its commits aren't available here: %s", e.repositoryProblem)
		}
		return failure(true, "the root didn't exist when its commits were requested; the next synchronization cycle attaches it")
	} else if !gitHasHistory(ctx, temporary, commit) {
		if e.repositoryProblem != "" {
			return failure(false, "its commits aren't available here: %s", e.repositoryProblem)
		}
		return failure(false, "its commits didn't arrive in full")
	}

	// Configure the origin and the line ending settings, as a clone on the
	// other endpoint would be (see cloneOptions). The values are validated on
	// the other endpoint, but we check them again before handing them to Git.
	var settings [][2]string
	if target.Url != "" {
		if url, err := git.UsableURL(target.Url); err == nil {
			settings = append(settings,
				[2]string{"remote.origin.url", url},
				[2]string{"remote.origin.fetch", gitRootFetchRefspec},
			)
		}
	}
	if git.UsableSetting(target.AutoCRLF) {
		settings = append(settings, [2]string{"core.autocrlf", target.AutoCRLF})
	}
	if git.UsableSetting(target.Eol) {
		settings = append(settings, [2]string{"core.eol", target.Eol})
	}
	for _, setting := range settings {
		if _, err := git.Run(ctx, temporary, "config", setting[0], setting[1]); err != nil {
			return failure(false, "it couldn't be configured: %v", err)
		}
	}

	// Point HEAD at the commit, on the branch if there's one, and make the
	// index match the commit. Reading the tree only writes the index, and
	// unlike a checkout, it doesn't involve the working tree, which is the
	// root's, and whose files are synchronized separately. The repository is
	// new, so the branch can't exist yet.
	if target.Branch != "" {
		if err := gitCheckBranchName(ctx, temporary, target.Branch); err != nil {
			return failure(false, "the name of its branch isn't valid here: %v", err)
		} else if _, err := git.Run(ctx, temporary, "update-ref", "-m", gitRootAttachMessage, "refs/heads/"+target.Branch, commit, ""); err != nil {
			return failure(false, "its branch couldn't be created: %v", err)
		} else if _, err := git.Run(ctx, temporary, "symbolic-ref", "-m", gitRootAttachMessage, "HEAD", "refs/heads/"+target.Branch); err != nil {
			return failure(false, "HEAD couldn't be set: %v", err)
		}
	} else if _, err := git.Run(ctx, temporary, "update-ref", "--no-deref", "-m", gitRootAttachMessage, "HEAD", commit); err != nil {
		return failure(false, "HEAD couldn't be set: %v", err)
	}
	if _, err := git.Run(ctx, temporary, "read-tree", "--no-recurse-submodules", commit); err != nil {
		return failure(false, "its index couldn't be set: %v", err)
	}

	// Move the Git directory into place, without replacing anything that has
	// appeared there in the mean time.
	if err := filesystem.Rename(nil, filepath.Join(temporary, ".git"), nil, gitEntry, false); err != nil {
		if os.IsExist(err) {
			return failure(true, "a .git entry appeared at the root since the scan; the next synchronization cycle takes it into account")
		}
		return failure(false, "it couldn't be moved into place: %v", err)
	}

	// Refresh the index, which records the information about the files in the
	// working tree that tells Git that they haven't changed. That only saves
	// Git some work later, so a failure doesn't matter.
	git.Run(ctx, e.root, "update-index", "-q", "--refresh")

	// Success.
	return gitRootResult(core.RepositoryOutcome_Moved, "", false)
}

// gitRootRestore holds a Git command that undoes part of a follow.
type gitRootRestore []string

// followRootRepository moves HEAD in the repository at the root as an action
// requests, for TransitionRepository. It doesn't touch the working tree, it
// keeps the changes that are staged in the index where possible, it doesn't
// write ORIG_HEAD, and it doesn't run hooks (see git.Command).
func (e *endpoint) followRootRepository(ctx context.Context, action *core.RepositoryAction) *core.RepositoryResult {
	// Extract the old state and the target.
	old, target := action.Old, action.Target
	oldCommit, targetCommit := hex.EncodeToString(old.Commit), hex.EncodeToString(target.Commit)

	// Create functions to report failures, both for transient conditions,
	// which are warnings, and for others.
	failure := func(warning bool, format string, arguments ...any) *core.RepositoryResult {
		return gitRootResult(core.RepositoryOutcome_Failed, fmt.Sprintf(
			"HEAD didn't follow the other endpoint to %s, because %s",
			gitRootTarget(target), fmt.Sprintf(format, arguments...),
		), warning)
	}
	changed := func() *core.RepositoryResult {
		return failure(true, "HEAD changed here since the scan; the next synchronization cycle takes the change into account")
	}

	// Don't follow while an operation is in progress whose state is in the
	// index and the Git directory, such as a rebase or a merge with conflicts,
	// since moving HEAD and the index would disrupt it, or while another
	// command is using the index (see gitOperationInProgress).
	if operation, err := gitOperationInProgress(ctx, e.root); err != nil {
		return failure(false, "%v", err)
	} else if operation != "" {
		return failure(true, "%s here, which moving HEAD would disrupt; HEAD follows once that's over", operation)
	}

	// Verify that HEAD hasn't changed since the scan, since moving it would
	// discard the change (e.g. a commit). If it's already at the target, then
	// there's nothing to do.
	isCurrent := func() (*core.RepositoryResult, bool) {
		if _, err := os.Lstat(filepath.Join(e.root, ".git")); os.IsNotExist(err) {
			return changed(), false
		}
		commit, branch, err := git.Head(ctx, e.root)
		if err != nil {
			return failure(false, "Git couldn't determine what HEAD identifies here: %v", err), false
		} else if bytes.Equal(commit, target.Commit) && branch == target.Branch {
			return gitRootResult(core.RepositoryOutcome_Moved, "", false), false
		} else if !bytes.Equal(commit, old.Commit) || branch != old.Branch {
			return changed(), false
		}
		return nil, true
	}
	if result, ok := isCurrent(); !ok {
		return result
	}

	// Verify that the target commit is available.
	if !gitHasHistory(ctx, e.root, targetCommit) {
		if e.repositoryProblem != "" {
			return failure(false, "the commit isn't available here: %s", e.repositoryProblem)
		}
		return failure(false, "the commit didn't arrive from the other endpoint in full")
	}

	// Apply the policy. HEAD can always move from an unborn branch.
	if action.Policy != core.RepositoryPolicy_Follow && oldCommit != "" {
		if contained, err := gitIsAncestor(ctx, e.root, oldCommit, targetCommit); err != nil {
			return failure(false, "Git couldn't compare the commits: %v", err)
		} else if !contained {
			if ahead, err := gitIsAncestor(ctx, e.root, targetCommit, oldCommit); err != nil {
				return failure(false, "Git couldn't compare the commits: %v", err)
			} else if ahead {
				return gitRootResult(core.RepositoryOutcome_Ahead, "", false)
			} else if action.Policy == core.RepositoryPolicy_FastForward {
				return gitRootResult(core.RepositoryOutcome_Diverged, "", false)
			}
		}
	}

	// If HEAD moves to another branch, then make sure that the branch can be
	// used: it may only be created or fast-forwarded, so that no commits are
	// lost, and it mustn't be checked out in another worktree, which would
	// have its HEAD changed as well.
	switching := target.Branch != "" && target.Branch != old.Branch
	var branchTip string
	if switching {
		if err := gitCheckBranchName(ctx, e.root, target.Branch); err != nil {
			return failure(false, "the name of the branch isn't valid here: %v", err)
		}
		output, err := git.Run(ctx, e.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+target.Branch)
		if err == nil {
			branchTip = output
		} else if !git.ExitedWith(err, 1) {
			return failure(false, "Git couldn't examine branch %s here: %v", target.Branch, err)
		}
		if branchTip != "" && branchTip != targetCommit {
			if contained, err := gitIsAncestor(ctx, e.root, branchTip, targetCommit); err != nil || !contained {
				return failure(false,
					"this endpoint has a branch %s at commit %s, which commit %s doesn't contain; rename or delete that branch here (e.g. git branch -m %s %s-local), and HEAD follows",
					target.Branch, gitShortCommit(branchTip), gitShortCommit(targetCommit), target.Branch, target.Branch,
				)
			}
		}
		if worktree, err := gitBranchWorktree(ctx, e.root, target.Branch); err != nil {
			return failure(false, "Git couldn't list worktrees here: %v", err)
		} else if worktree != "" {
			return failure(false,
				"branch %s is checked out in another worktree here (%s); switch that worktree to another branch (or run git worktree prune if it no longer exists), and HEAD follows",
				target.Branch, worktree,
			)
		}
	}

	// Take the index's lock, which keeps other Git commands from changing the
	// index (and, by extension, from committing) while we work. The new index
	// is written to the lock file and then renamed into place, which releases
	// the lock, as Git itself does.
	output, err := git.Run(ctx, e.root, "rev-parse", "--git-path", "index")
	if err != nil {
		return failure(false, "Git couldn't locate the index: %v", err)
	}
	index := filepath.FromSlash(output)
	if !filepath.IsAbs(index) {
		index = filepath.Join(e.root, index)
	}
	lock := index + ".lock"
	lockFile, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		if os.IsExist(err) {
			return failure(true, "another Git command is using the index (index.lock exists); HEAD follows once it's done (if no Git command is running, remove %s)", lock)
		}
		return failure(false, "the index couldn't be locked: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			os.Remove(lock)
		}
	}()

	// Now that nothing else can commit, verify again that HEAD hasn't changed.
	if result, ok := isCurrent(); !ok {
		lockFile.Close()
		return result
	}

	// Copy the index into the lock file. If there's no index, then the lock
	// file stays empty, which Git treats as an empty index when it's
	// replacing it.
	existingIndex, err := os.ReadFile(index)
	if err != nil && !os.IsNotExist(err) {
		lockFile.Close()
		return failure(false, "the index couldn't be read: %v", err)
	}
	_, err = lockFile.Write(existingIndex)
	if closeErr := lockFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return failure(false, "the index couldn't be copied: %v", err)
	}

	// Compute the new index by merging the change from the old commit to the
	// target commit into the index, which keeps the changes that are staged
	// and the information about unchanged files. Only the index is involved,
	// not the working tree, whose files are synchronized separately and are
	// thus already the target's. If that's not possible (because staged
	// changes conflict with the change, or because there's no old commit),
	// then the index is replaced with the target's tree, which unstages any
	// changes. The latter doesn't merit a warning if there was no index.
	var warning string
	merged := false
	if oldCommit != "" && existingIndex != nil {
		_, err := git.RunWithIndex(ctx, e.root, lock, "read-tree", "--no-recurse-submodules", "-m", "-i", oldCommit, targetCommit)
		merged = err == nil
		if !merged {
			warning = fmt.Sprintf("HEAD followed the other endpoint to %s, but the changes staged here had to be unstaged (%v); the files themselves are unchanged, so stage them again if needed",
				gitRootTarget(target), err,
			)
		}
	} else if existingIndex != nil {
		warning = fmt.Sprintf("HEAD followed the other endpoint to %s, but any changes staged here had to be unstaged, since there were no commits here; the files themselves are unchanged, so stage them again if needed",
			gitRootTarget(target),
		)
	}
	if !merged {
		if _, err := git.RunWithIndex(ctx, e.root, lock, "read-tree", "--no-recurse-submodules", targetCommit); err != nil {
			return failure(false, "the index couldn't be computed: %v", err)
		}
	}

	// Move the references, verifying their old values where Git allows it, and
	// record how to undo each step, in case a later one fails.
	var restores []gitRootRestore
	move := func(arguments ...string) error {
		_, err := git.Run(ctx, e.root, arguments...)
		return err
	}
	switch {
	case target.Branch == "":
		// Detach HEAD at the target commit. An unborn HEAD can't be verified,
		// because Git treats it as existing, though it was verified above while
		// holding the lock.
		arguments := []string{"update-ref", "--no-deref", "-m", gitRootFollowMessage, "HEAD", targetCommit}
		if oldCommit != "" {
			arguments = append(arguments, oldCommit)
		}
		if err := move(arguments...); err != nil {
			return failure(false, "HEAD couldn't be moved: %v", err)
		}
		if old.Branch != "" {
			restores = append(restores, gitRootRestore{"symbolic-ref", "HEAD", "refs/heads/" + old.Branch})
		} else {
			restores = append(restores, gitRootRestore{"update-ref", "--no-deref", "HEAD", oldCommit, targetCommit})
		}
	case !switching:
		// Move the branch that HEAD refers to, creating it if it's unborn.
		if err := move("update-ref", "-m", gitRootFollowMessage, "refs/heads/"+target.Branch, targetCommit, oldCommit); err != nil {
			return failure(false, "branch %s couldn't be moved: %v", target.Branch, err)
		}
		if oldCommit != "" {
			restores = append(restores, gitRootRestore{"update-ref", "refs/heads/" + target.Branch, oldCommit, targetCommit})
		} else {
			restores = append(restores, gitRootRestore{"update-ref", "-d", "refs/heads/" + target.Branch, targetCommit})
		}
	default:
		// Create or fast-forward the target branch, and point HEAD at it. The
		// old branch (if any) stays where it is.
		if err := move("update-ref", "-m", gitRootFollowMessage, "refs/heads/"+target.Branch, targetCommit, branchTip); err != nil {
			return failure(false, "branch %s couldn't be moved: %v", target.Branch, err)
		}
		if branchTip != "" {
			restores = append(restores, gitRootRestore{"update-ref", "refs/heads/" + target.Branch, branchTip, targetCommit})
		} else {
			restores = append(restores, gitRootRestore{"update-ref", "-d", "refs/heads/" + target.Branch, targetCommit})
		}
		if err := move("symbolic-ref", "-m", gitRootFollowMessage, "HEAD", "refs/heads/"+target.Branch); err != nil {
			e.restoreRootRepository(ctx, restores)
			return failure(false, "HEAD couldn't be moved to branch %s: %v", target.Branch, err)
		}
		if old.Branch != "" {
			restores = append(restores, gitRootRestore{"symbolic-ref", "HEAD", "refs/heads/" + old.Branch})
		} else {
			restores = append(restores, gitRootRestore{"update-ref", "--no-deref", "HEAD", oldCommit})
		}
	}

	// Rename the new index into place, which releases the lock. If that fails,
	// then undo the moves.
	err = os.Rename(lock, index)
	for attempt := 1; err != nil && runtime.GOOS == "windows" && attempt < gitIndexRenameAttempts; attempt++ {
		time.Sleep(gitIndexRenameInterval)
		err = os.Rename(lock, index)
	}
	if err != nil {
		e.restoreRootRepository(ctx, restores)
		return failure(false, "the index couldn't be replaced: %v", err)
	}
	locked = false

	// Success.
	return gitRootResult(core.RepositoryOutcome_Moved, warning, true)
}

// restoreRootRepository undoes the steps of a follow that failed, in reverse
// order, as far as possible. The commands run even if the follow was
// cancelled, since they undo what it did, and they only write a few small
// files. Each uses the values that the follow set as its old values where Git
// allows that, so that nothing that has changed since is undone.
func (e *endpoint) restoreRootRepository(ctx context.Context, restores []gitRootRestore) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitRestoreTimeout)
	defer cancel()
	for r := len(restores) - 1; r >= 0; r-- {
		arguments := append([]string{restores[r][0], "-m", gitRootFollowMessage + " failed"}, restores[r][1:]...)
		if _, err := git.Run(ctx, e.root, arguments...); err != nil {
			e.logger.Warn("Unable to undo part of a failed follow of the repository at the root:", err)
		}
	}
}
