package local

import (
	"fmt"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/fastpath"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

// gitReceivableFile determines whether or not an endpoint in Git mode should
// accept a file for a path, given the rules on its own filesystem. A file for a
// path that can't be evaluated isn't accepted, and neither is one for a path
// that's excluded, unless it's a rule file that can be delivered (see
// gitignore.IsDeliverableRuleFile and gitTransitionPruner). This is the
// decision that gitTransitionPruner makes for files, which it reports on,
// whereas this only determines what's worth transferring.
func gitReceivableFile(ignorer ignore.DirectoryIgnorer, path string) bool {
	excluded, err := ignore.Excluded(ignorer, path, false)
	return err == nil && (excluded == "" || gitignore.IsDeliverableRuleFile(path))
}

// gitTransitionPruner computes the part of a change's new content that an
// endpoint in Git mode may write, given the rules on its own filesystem. Scans
// only represent paths that exist, so the Untracked entries that they produce
// can't protect paths that are excluded but absent, which have to be protected
// by the endpoint that receives them. The endpoints' rules can differ for as
// long as a session runs, so what's left out is simply computed again as a
// change of its own by each cycle, and it's refused on its own by each cycle,
// without standing in the way of anything else.
//
// Regular files that provide ignore rules are never left out for being
// excluded, and neither are the directories that lead to them. Otherwise, a
// session would compute their delivery in every cycle, and cycles that have
// rules to deliver don't do anything else (see ruleOnlyTransitions in the
// synchronization package). Once delivered, the excluded directory that
// contains them is Untracked on this endpoint, which ends any further
// reconciliation of it. None of this applies at or beneath a Git entry, which
// isn't content that a session could ever need to deliver, and which is never
// written or removed (see refuseGitEntry and removesGitContent).
type gitTransitionPruner struct {
	// ignorer evaluates this endpoint's rules.
	ignorer ignore.DirectoryIgnorer
	// problems are the problems reported so far.
	problems []*core.Problem
	// reported records the paths that have been reported by reportOnce, since
	// many paths in a change can be left out because of the same parent.
	reported map[string]bool
}

// reportOnce records a problem, unless one has already been recorded by this
// method for its path.
func (p *gitTransitionPruner) reportOnce(problem *core.Problem) {
	if p.reported[problem.Path] {
		return
	} else if p.reported == nil {
		p.reported = make(map[string]bool)
	}
	p.reported[problem.Path] = true
	p.problems = append(p.problems, problem)
}

// warn reports an excluded path, once, which should be the shallowest excluded
// path. It's a warning because endpoints are allowed to disagree about what's
// ignored (.git/info/exclude isn't synchronized, for example), so this is
// content that isn't synchronized by design.
func (p *gitTransitionPruner) warn(excluded string) {
	p.reportOnce(&core.Problem{
		Path: excluded,
		Error: fmt.Sprintf(
			"ignored by this endpoint's own rules, so it isn't written here; add %q to ignore.paths to synchronize it",
			"!"+excluded,
		),
		Warning: true,
	})
}

// refuseGitEntry reports a change at or beneath a Git entry, once, which should
// be the shallowest one. Unlike an exclusion, this isn't by design: scans don't
// represent what's in a Git entry, on any endpoint, so the change can't have
// come from reconciling them. It thus isn't a warning, and since no pattern can
// unignore a Git entry, it doesn't suggest one.
func (p *gitTransitionPruner) refuseGitEntry(entry string) {
	p.reportOnce(&core.Problem{
		Path:  entry,
		Error: "refused a change to Git metadata, which is never synchronized; scans don't produce such changes, so the daemon may be malfunctioning or compromised",
	})
}

// removesGitContent determines whether or not removing a change's old content
// would remove a Git entry or anything beneath one, reporting each Git entry
// involved. Old content comes from the session's ancestor or the synchronizable
// part of a scan, and neither of those has Git entries, since they're never
// synchronized (a directory that has one on disk is left alone, as a conflict).
// So only a malfunctioning or compromised controller can ask for this, and
// removal acts on what it's given, unlike scans, which never look inside of Git
// entries. Files are safe from this, since they have to be in the scan's cache to
// be removed, but symbolic links and empty directories aren't.
func (p *gitTransitionPruner) removesGitContent(change *core.Change) bool {
	if gitEntry := gitignore.ShallowestGitEntry(change.Path); gitEntry != "" {
		p.refuseGitEntry(gitEntry)
		return true
	}
	return p.containsGitEntry(change.Path, change.Old)
}

// containsGitEntry determines whether or not an entry at a path that isn't at or
// beneath a Git entry contains a Git entry, reporting each one that it finds.
func (p *gitTransitionPruner) containsGitEntry(path string, entry *core.Entry) bool {
	var found bool
	if entry != nil && len(entry.Contents) > 0 {
		contentPathPrefix := fastpath.Joinable(path)
		for name, content := range entry.Contents {
			if gitignore.IsGitEntry(name) {
				p.refuseGitEntry(contentPathPrefix + name)
				found = true
			} else if p.containsGitEntry(contentPathPrefix+name, content) {
				found = true
			}
		}
	}
	return found
}

// fail reports a path that can't be evaluated. Unlike an exclusion, this isn't
// by design, so it isn't a warning.
func (p *gitTransitionPruner) fail(path string, err error) {
	p.problems = append(p.problems, &core.Problem{Path: path, Error: err.Error()})
}

// prune returns the part of an entry at a path that may be written, or nil if
// no part of it may be. Entries are immutable and shared, so an entry that has
// to change is copied, and an entry that doesn't is returned as is. The path is
// evaluated from the root for each entry, which is negligible compared to the
// cost of creating an entry on disk, and which ensures that this always agrees
// with gitReceivableFile.
func (p *gitTransitionPruner) prune(path string, entry *core.Entry) *core.Entry {
	// Nothing at or beneath a Git entry can be written, and it's the Git entry
	// that's worth reporting, even if a parent is excluded too.
	if gitEntry := gitignore.ShallowestGitEntry(path); gitEntry != "" {
		p.refuseGitEntry(gitEntry)
		return nil
	}

	// Evaluate the path. A submodule is a directory on disk, and rules treat it
	// as one, even though only directories have contents that we can prune.
	directory := entry.Kind == core.EntryKind_Directory
	excluded, err := ignore.Excluded(p.ignorer, path, directory || entry.Kind == core.EntryKind_GitLink)
	if err != nil {
		p.fail(path, err)
		return nil
	}

	// Anything other than a directory is either written in full or not at all.
	if !directory {
		if excluded != "" && !(entry.Kind == core.EntryKind_File && gitignore.IsDeliverableRuleFile(path)) {
			p.warn(excluded)
			return nil
		}
		return entry
	}

	// A directory has to have rules that we can read for us to tell what they
	// exclude from its contents. That doesn't apply to an excluded directory,
	// since all that can be written to it are rule files, which are exempt.
	if excluded == "" {
		if err := p.ignorer.EnsureDirectory(path); err != nil {
			p.fail(path, err)
			return nil
		}
	}

	// Determine which of the directory's contents to retain.
	contentPathPrefix := fastpath.Joinable(path)
	contents := make(map[string]*core.Entry, len(entry.Contents))
	modified := false
	for name, content := range entry.Contents {
		if retained := p.prune(contentPathPrefix+name, content); retained != nil {
			contents[name] = retained
			modified = modified || retained != content
		} else {
			modified = true
		}
	}

	// An excluded directory only exists for the sake of the rule files beneath
	// it. If there aren't any, then it's left out too, whereas a directory that
	// isn't excluded is created even if it ends up without any contents, which
	// faithfully reflects a type change on the other endpoint.
	if excluded != "" && len(contents) == 0 {
		p.warn(excluded)
		return nil
	} else if !modified {
		return entry
	}

	// Create the pruned copy.
	pruned := entry.Copy(core.EntryCopyBehaviorSlim)
	pruned.Contents = contents
	return pruned
}

// pruneGitTransitions computes what an endpoint in Git mode should apply for
// each transition (see gitTransitionPruner), along with any resulting problems.
// The returned slice corresponds to the transitions, with a change that has the
// pruned content for each transition that's still to be applied, and with nil
// for each that's refused, which leaves its old content in place. Rules don't
// affect deletions, whose old content is what the endpoint's scan tracked, but a
// change that would remove Git metadata can't have come from a scan and is
// refused, whatever else it does.
func pruneGitTransitions(ignorer ignore.DirectoryIgnorer, transitions []*core.Change) ([]*core.Change, []*core.Problem) {
	pruner := &gitTransitionPruner{ignorer: ignorer}
	results := make([]*core.Change, len(transitions))
	for t, transition := range transitions {
		if pruner.removesGitContent(transition) {
			continue
		} else if transition.New == nil {
			results[t] = transition
		} else if pruned := pruner.prune(transition.Path, transition.New); pruned == transition.New {
			results[t] = transition
		} else if pruned != nil {
			results[t] = &core.Change{Path: transition.Path, Old: transition.Old, New: pruned}
		}
	}
	return results, pruner.problems
}
