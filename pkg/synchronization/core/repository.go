package core

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

const (
	// maximumRepositoryStringLength is the maximum length of the string fields
	// of a Repository, which only bounds what endpoints can send.
	maximumRepositoryStringLength = 4096
)

// validCommit determines whether or not a commit identifier has the length of
// a SHA-1 or a SHA-256 commit identifier.
func validCommit(commit []byte) bool {
	return len(commit) == 20 || len(commit) == 32
}

// validBranch performs basic validation of a branch name. It doesn't implement
// Git's rules, which endpoints apply before using a name, but only rejects
// names that could never be valid or that could be mistaken for options.
func validBranch(branch string) bool {
	if branch == "" || len(branch) > maximumRepositoryStringLength {
		return false
	} else if strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return false
	} else if strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") {
		return false
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validRepositoryString determines whether or not a string field of a
// Repository other than its branch is acceptable.
func validRepositoryString(value string) bool {
	return len(value) <= maximumRepositoryStringLength && !strings.ContainsRune(value, 0)
}

// EnsureValid ensures that Repository's invariants are respected. A nil
// repository is valid, since it represents an endpoint without a repository.
func (r *Repository) EnsureValid() error {
	// A nil repository is valid.
	if r == nil {
		return nil
	}

	// An unavailable repository's other fields aren't used.
	if r.Unavailable != "" {
		if !validRepositoryString(r.Unavailable) {
			return errors.New("invalid unavailability description")
		}
		return nil
	}

	// Ensure that the commit and branch are valid. A repository without a
	// commit (an unborn HEAD) always names a branch.
	if len(r.Commit) > 0 && !validCommit(r.Commit) {
		return errors.New("invalid commit")
	} else if r.Branch != "" && !validBranch(r.Branch) {
		return errors.New("invalid branch")
	} else if len(r.Commit) == 0 && r.Branch == "" {
		return errors.New("repository without commit or branch")
	}

	// Ensure that the settings are valid.
	if !validRepositoryString(r.Url) || !validRepositoryString(r.AutoCRLF) || !validRepositoryString(r.Eol) {
		return errors.New("invalid repository setting")
	}

	// Success.
	return nil
}

// ensureValidAncestor ensures that a Repository is a valid record of the
// commit and branch on which both endpoints last agreed. A nil repository is
// valid, since it means that they never have.
func (r *Repository) ensureValidAncestor() error {
	if r == nil {
		return nil
	} else if !validCommit(r.Commit) {
		return errors.New("invalid ancestor commit")
	} else if r.Branch != "" && !validBranch(r.Branch) {
		return errors.New("invalid ancestor branch")
	} else if r.Unavailable != "" {
		return errors.New("unavailable ancestor")
	}
	return nil
}

// Equal determines whether or not two repositories have the same commit and
// branch, which is what the session keeps the same on both endpoints. Two nil
// repositories are equal, while a nil and a non-nil repository aren't.
func (r *Repository) Equal(other *Repository) bool {
	if r == nil || other == nil {
		return r == other
	}
	return bytes.Equal(r.Commit, other.Commit) && r.Branch == other.Branch
}

// Supported indicates whether or not a particular repository policy is a
// valid value.
func (p RepositoryPolicy) Supported() bool {
	switch p {
	case RepositoryPolicy_Follow, RepositoryPolicy_FastForward, RepositoryPolicy_FastForwardOrFollow:
		return true
	default:
		return false
	}
}

// EnsureValid ensures that RepositoryAction's invariants are respected.
func (a *RepositoryAction) EnsureValid() error {
	// A nil action is not valid.
	if a == nil {
		return errors.New("nil repository action")
	}

	// Ensure that the old state is valid and available.
	if err := a.Old.EnsureValid(); err != nil {
		return fmt.Errorf("invalid old repository: %w", err)
	} else if a.Old != nil && a.Old.Unavailable != "" {
		return errors.New("unavailable old repository")
	}

	// Ensure that the target is valid, available, and has a commit.
	if a.Target == nil {
		return errors.New("nil target repository")
	} else if err := a.Target.EnsureValid(); err != nil {
		return fmt.Errorf("invalid target repository: %w", err)
	} else if a.Target.Unavailable != "" {
		return errors.New("unavailable target repository")
	} else if len(a.Target.Commit) == 0 {
		return errors.New("target repository without commit")
	}

	// Ensure that the policy is valid. Attaching a repository is always
	// unconditional.
	if !a.Policy.Supported() {
		return errors.New("invalid repository policy")
	} else if a.Old == nil && a.Policy != RepositoryPolicy_Follow {
		return errors.New("conditional attachment of repository")
	}

	// Success.
	return nil
}

// Supported indicates whether or not a particular repository outcome is a
// valid value.
func (o RepositoryOutcome) Supported() bool {
	switch o {
	case RepositoryOutcome_Failed, RepositoryOutcome_Moved, RepositoryOutcome_Ahead, RepositoryOutcome_Diverged:
		return true
	default:
		return false
	}
}

// EnsureValid ensures that RepositoryResult's invariants are respected.
func (r *RepositoryResult) EnsureValid() error {
	// A nil result is not valid.
	if r == nil {
		return errors.New("nil repository result")
	}

	// Ensure that the outcome is valid, that failures have a problem, and that
	// other outcomes have at most a warning.
	if !r.Outcome.Supported() {
		return errors.New("invalid repository outcome")
	} else if r.Outcome == RepositoryOutcome_Failed && r.Problem == nil {
		return errors.New("repository failure without problem")
	} else if r.Outcome != RepositoryOutcome_Failed && r.Problem != nil && !r.Problem.Warning {
		return errors.New("repository problem without failure")
	} else if r.Problem != nil {
		if err := r.Problem.EnsureValid(); err != nil {
			return fmt.Errorf("invalid repository problem: %w", err)
		}
	}

	// Success.
	return nil
}

// EnsureValid ensures that RepositoryConflict's invariants are respected.
func (c *RepositoryConflict) EnsureValid() error {
	// A nil conflict is not valid.
	if c == nil {
		return errors.New("nil repository conflict")
	}

	// Ensure that the ancestor is valid.
	if err := c.Ancestor.ensureValidAncestor(); err != nil {
		return fmt.Errorf("invalid repository conflict ancestor: %w", err)
	}

	// Ensure that both endpoints have a valid repository with a commit.
	for _, repository := range []*Repository{c.Alpha, c.Beta} {
		if repository == nil {
			return errors.New("repository conflict without repository")
		} else if err := repository.EnsureValid(); err != nil {
			return fmt.Errorf("invalid repository in conflict: %w", err)
		} else if len(repository.Commit) == 0 || repository.Unavailable != "" {
			return errors.New("repository conflict without commit")
		}
	}

	// Success.
	return nil
}
