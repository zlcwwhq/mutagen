package local

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/logging"
)

const (
	// gitFollowMessage is the reflog message for the changes that follow the
	// other endpoint's repository, at the root or in a submodule.
	gitFollowMessage = "mutagen: follow"
	// gitAttachMessage is the reflog message for the changes that set up a
	// repository for attachment, at the root or in a submodule.
	gitAttachMessage = "mutagen: attach"
	// gitRestoreTimeout is the maximum amount of time that the Git commands
	// that undo a failed follow can take (see restoreHead). They run even if
	// the follow was cancelled, since they undo what the follow did, and they
	// only read and write a few small files.
	gitRestoreTimeout = 10 * time.Second
	// gitIndexRenameAttempts is the number of times that renaming a new index
	// into place is attempted on Windows, where it fails while another process
	// has the index open, such as a Git command that only reads it.
	gitIndexRenameAttempts = 20
	// gitIndexRenameInterval is the interval between those attempts.
	gitIndexRenameInterval = 50 * time.Millisecond
)

// gitHead describes what HEAD identifies in a repository.
type gitHead struct {
	// commit is the hexadecimal identifier of the commit, which is empty if
	// HEAD is unborn (i.e. if it refers to a branch without commits).
	commit string
	// branch is the short name of the branch (beneath refs/heads/) that HEAD
	// refers to, which is empty if HEAD is detached.
	branch string
}

// gitFollowError is the reason why HEAD didn't follow (see followHead). Its
// message completes a sentence that says that HEAD didn't follow, because of
// it.
type gitFollowError struct {
	// transient indicates that the reason is expected to pass by itself, such
	// as a lock that another Git command holds, which only merits a warning.
	transient bool
	// reason is the reason.
	reason string
}

// Error implements error.Error.
func (e *gitFollowError) Error() string {
	return e.reason
}

// gitFollowFailure creates a gitFollowError that isn't transient.
func gitFollowFailure(format string, arguments ...any) error {
	return &gitFollowError{reason: fmt.Sprintf(format, arguments...)}
}

// errGitHeadChanged indicates that HEAD has changed since the scan.
var errGitHeadChanged = &gitFollowError{
	transient: true,
	reason:    "HEAD changed here since the scan; the next synchronization cycle takes the change into account",
}

// checkHead verifies that HEAD in the repository in a directory still
// identifies what it did at the scan (old), or else what a follow would make it
// identify (target), in which case it returns true, since there's nothing to
// do. Any other state is an errGitHeadChanged, since moving HEAD would discard
// the change (e.g. a commit).
func checkHead(ctx context.Context, directory string, old, target gitHead) (bool, error) {
	if _, err := os.Lstat(filepath.Join(directory, ".git")); os.IsNotExist(err) {
		return false, errGitHeadChanged
	}
	commit, branch, err := git.Head(ctx, directory)
	if err != nil {
		return false, gitFollowFailure("Git couldn't determine what HEAD identifies here: %v", err)
	}
	head := gitHead{commit: hex.EncodeToString(commit), branch: branch}
	if head == target {
		return true, nil
	} else if head != old {
		return false, errGitHeadChanged
	}
	return false, nil
}

// gitRestore holds a Git command that undoes part of a follow.
type gitRestore []string

// followHead moves HEAD in the repository in a directory from what it
// identified at the scan (old) to a target, which is how both the repository
// at the root and those of submodules follow the other endpoint's. It doesn't
// touch the working tree, whose files are synchronized separately, it keeps the
// changes that are staged in the index where possible, it doesn't write
// ORIG_HEAD, and it doesn't run hooks (see git.Command). If the target has no
// branch, then HEAD is detached at the target commit, and any branch that it
// referred to stays where it is. If the target's branch is the old one, then
// that branch moves. Otherwise, the target's branch is created, or
// fast-forwarded from its tip (its commit, or empty if it doesn't exist), and
// HEAD refers to it, while the old branch stays where it is.
//
// The caller has verified that HEAD was the old state, that no operation is in
// progress (see gitOperationInProgress), that the target commit is available,
// and that the target branch can be used. Another Git command can start in the
// mean time, though, so this takes the index's lock, which keeps other Git
// commands from changing the index (and, by extension, from committing) while
// HEAD moves, and verifies HEAD again while holding it (see checkHead). If HEAD
// is already at the target, then there's nothing to do. The new index is
// computed in the lock file and then renamed into place, which releases the
// lock, as Git itself does.
//
// Failures are *gitFollowError values, and if one occurs, then nothing has
// changed. On success, it returns a warning if staged changes had to be
// unstaged, which completes a sentence that says that HEAD followed, but.
func followHead(ctx context.Context, logger *logging.Logger, directory string, old, target gitHead, branchTip string) (string, error) {
	// Take the index's lock.
	output, err := git.Run(ctx, directory, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", gitFollowFailure("Git couldn't locate the index: %v", err)
	}
	index := filepath.FromSlash(output)
	if !filepath.IsAbs(index) {
		index = filepath.Join(directory, index)
	}
	lock := index + ".lock"
	lockFile, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		if os.IsExist(err) {
			return "", &gitFollowError{transient: true, reason: fmt.Sprintf(
				"another Git command is using the index (index.lock exists); HEAD follows once it's done (if no Git command is running, remove %s)", lock,
			)}
		}
		return "", gitFollowFailure("the index couldn't be locked: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			os.Remove(lock)
		}
	}()

	// Now that nothing else can commit, verify again that HEAD hasn't changed.
	if done, err := checkHead(ctx, directory, old, target); err != nil || done {
		lockFile.Close()
		return "", err
	}

	// Copy the index into the lock file. If there's no index, then the lock
	// file stays empty, which Git treats as an empty index when it's
	// replacing it.
	existingIndex, err := os.ReadFile(index)
	if err != nil && !os.IsNotExist(err) {
		lockFile.Close()
		return "", gitFollowFailure("the index couldn't be read: %v", err)
	}
	_, err = lockFile.Write(existingIndex)
	if closeErr := lockFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", gitFollowFailure("the index couldn't be copied: %v", err)
	}

	// Compute the new index by merging the change from the old commit to the
	// target commit into the index, which keeps the changes that are staged
	// and the information about unchanged files. Only the index is involved,
	// not the working tree, whose files are synchronized separately and are
	// thus already the target's. If that's not possible (because staged
	// changes conflict with the change, or because there's no old commit),
	// then the index is replaced with the target's tree, which unstages any
	// changes. The latter doesn't merit a warning if nothing was staged, which
	// is also the case if there was no index. The merge can fail without
	// staged changes, because writing an index compares the contents of
	// entries whose files were written within the same second as the index
	// (such as right after a checkout), which runs clean filters, which can
	// fail (e.g. one that's required, but whose program is missing). Reading a
	// tree without merging doesn't compare anything, because the new entries
	// don't have any cached file information.
	oldCommit, targetCommit := old.commit, target.commit
	var warning string
	merged := false
	if oldCommit != "" && existingIndex != nil {
		_, err := git.RunWithIndex(ctx, directory, lock, "read-tree", "--no-recurse-submodules", "-m", "-i", oldCommit, targetCommit)
		merged = err == nil
		if !merged && gitHasStagedChanges(ctx, directory, lock, oldCommit) {
			warning = fmt.Sprintf("the changes staged here had to be unstaged (%v); the files themselves are unchanged, so stage them again if needed", err)
		}
	} else if existingIndex != nil {
		warning = "any changes staged here had to be unstaged, since there were no commits here; the files themselves are unchanged, so stage them again if needed"
	}
	if !merged {
		if _, err := git.RunWithIndex(ctx, directory, lock, "read-tree", "--no-recurse-submodules", targetCommit); err != nil {
			return "", gitFollowFailure("the index couldn't be computed: %v", err)
		}
	}

	// Move the references, verifying their old values where Git allows it, and
	// record how to undo each step, in case a later one fails.
	var restores []gitRestore
	move := func(arguments ...string) error {
		_, err := git.Run(ctx, directory, arguments...)
		return err
	}
	switching := target.branch != "" && target.branch != old.branch
	switch {
	case target.branch == "":
		// Detach HEAD at the target commit. An unborn HEAD can't be verified,
		// because Git treats it as existing, though it was verified above while
		// holding the lock.
		arguments := []string{"update-ref", "--no-deref", "-m", gitFollowMessage, "HEAD", targetCommit}
		if oldCommit != "" {
			arguments = append(arguments, oldCommit)
		}
		if err := move(arguments...); err != nil {
			return "", gitFollowFailure("HEAD couldn't be moved: %v", err)
		}
		if old.branch != "" {
			restores = append(restores, gitRestore{"symbolic-ref", "HEAD", "refs/heads/" + old.branch})
		} else {
			restores = append(restores, gitRestore{"update-ref", "--no-deref", "HEAD", oldCommit, targetCommit})
		}
	case !switching:
		// Move the branch that HEAD refers to, creating it if it's unborn.
		if err := move("update-ref", "-m", gitFollowMessage, "refs/heads/"+target.branch, targetCommit, oldCommit); err != nil {
			return "", gitFollowFailure("branch %s couldn't be moved: %v", target.branch, err)
		}
		if oldCommit != "" {
			restores = append(restores, gitRestore{"update-ref", "refs/heads/" + target.branch, oldCommit, targetCommit})
		} else {
			restores = append(restores, gitRestore{"update-ref", "-d", "refs/heads/" + target.branch, targetCommit})
		}
	default:
		// Create or fast-forward the target branch, and point HEAD at it. The
		// old branch (if any) stays where it is.
		if err := move("update-ref", "-m", gitFollowMessage, "refs/heads/"+target.branch, targetCommit, branchTip); err != nil {
			return "", gitFollowFailure("branch %s couldn't be moved: %v", target.branch, err)
		}
		if branchTip != "" {
			restores = append(restores, gitRestore{"update-ref", "refs/heads/" + target.branch, branchTip, targetCommit})
		} else {
			restores = append(restores, gitRestore{"update-ref", "-d", "refs/heads/" + target.branch, targetCommit})
		}
		if err := move("symbolic-ref", "-m", gitFollowMessage, "HEAD", "refs/heads/"+target.branch); err != nil {
			restoreHead(ctx, logger, directory, restores)
			return "", gitFollowFailure("HEAD couldn't be moved to branch %s: %v", target.branch, err)
		}
		if old.branch != "" {
			restores = append(restores, gitRestore{"symbolic-ref", "HEAD", "refs/heads/" + old.branch})
		} else {
			restores = append(restores, gitRestore{"update-ref", "--no-deref", "HEAD", oldCommit})
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
		restoreHead(ctx, logger, directory, restores)
		return "", gitFollowFailure("the index couldn't be replaced: %v", err)
	}
	locked = false

	// Success.
	return warning, nil
}

// gitHasStagedChanges determines whether or not an index file of the
// repository in a directory has changes staged relative to a commit, which
// doesn't involve the working tree. If that can't be determined, then it
// assumes that it does.
func gitHasStagedChanges(ctx context.Context, directory, index, commit string) bool {
	_, err := git.RunWithIndex(ctx, directory, index, "diff-index", "--cached", "--quiet", commit, "--")
	return err != nil
}

// restoreHead undoes the steps of a follow that failed, in reverse order, as
// far as possible. The commands run even if the follow was cancelled, since
// they undo what it did, and they only write a few small files. Each uses the
// values that the follow set as its old values where Git allows that, so that
// nothing that has changed since is undone.
func restoreHead(ctx context.Context, logger *logging.Logger, directory string, restores []gitRestore) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitRestoreTimeout)
	defer cancel()
	for r := len(restores) - 1; r >= 0; r-- {
		arguments := append([]string{restores[r][0], "-m", gitFollowMessage + " failed"}, restores[r][1:]...)
		if _, err := git.Run(ctx, directory, arguments...); err != nil {
			logger.Warn("Unable to undo part of a failed follow of the repository in", directory+":", err)
		}
	}
}

// gitPrepareAttach makes a repository that's being prepared for attachment,
// in a directory without a working tree, identify a commit, specified by its
// hexadecimal identifier, on a branch (which mustn't exist yet), or detached if
// the branch is empty, and makes the index match the commit. Reading the tree
// only writes the index, and unlike a checkout, it doesn't involve the working
// tree, so it can't fail because of the files that the working tree will
// have, which are synchronized separately. If the session stops before the Git
// directory is moved into place, then nothing's changed.
func gitPrepareAttach(ctx context.Context, directory, commit, branch string) error {
	if branch != "" {
		if _, err := git.Run(ctx, directory, "update-ref", "-m", gitAttachMessage, "refs/heads/"+branch, commit, ""); err != nil {
			return fmt.Errorf("its branch couldn't be created: %w", err)
		} else if _, err := git.Run(ctx, directory, "symbolic-ref", "-m", gitAttachMessage, "HEAD", "refs/heads/"+branch); err != nil {
			return fmt.Errorf("HEAD couldn't be set: %w", err)
		}
	} else if _, err := git.Run(ctx, directory, "update-ref", "--no-deref", "-m", gitAttachMessage, "HEAD", commit); err != nil {
		return fmt.Errorf("HEAD couldn't be set: %w", err)
	}
	if _, err := git.Run(ctx, directory, "read-tree", "--no-recurse-submodules", commit); err != nil {
		return fmt.Errorf("its index couldn't be set: %w", err)
	}
	return nil
}

// gitRefreshIndex refreshes the index of the repository in a directory whose
// Git directory was just attached, which records the information about the
// files in the working tree that tells Git that they haven't changed. That
// only saves Git some work later.
func gitRefreshIndex(ctx context.Context, directory string) error {
	_, err := git.Run(ctx, directory, "update-index", "-q", "--refresh")
	return err
}
