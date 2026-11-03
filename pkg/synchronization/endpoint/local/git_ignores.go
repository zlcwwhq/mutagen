package local

import (
	"fmt"
	"strings"

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
// written or removed (see refuseGitEntry and removesGitContent), with one
// exception: a submodule's Git entry (a GitLink entry) can be attached to a
// directory or moved to another commit, which only changes what's in it. A
// submodule that's being created is populated by a clone, whose content is
// what this endpoint's own rules allow, so only its directory is evaluated.
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

// refuseSubmoduleRemoval reports the removal of a submodule's Git entry (a
// GitLink entry), once. Unlike other changes to Git metadata, this is expected:
// a submodule that's deleted on the other endpoint, or whose directory is
// replaced by something else there, would be removed here. It's refused
// because a repository can hold work that exists nowhere else, such as
// unpushed commits and stashes, and it isn't synchronized.
func (p *gitTransitionPruner) refuseSubmoduleRemoval(entry string) {
	p.reportOnce(&core.Problem{
		Path:  entry,
		Error: "submodule isn't removed automatically, since its repository isn't synchronized; remove it manually if it should go",
	})
}

// isGitLinkChange determines whether or not a change at a Git entry is one
// that may be applied to a submodule's Git entry: attaching one to a directory
// that doesn't have one, or moving one to another commit.
func isGitLinkChange(change *core.Change) bool {
	return isGitLinkPath(change.Path) &&
		change.New != nil && change.New.Kind == core.EntryKind_GitLink &&
		(change.Old == nil || change.Old.Kind == core.EntryKind_GitLink)
}

// isGitLinkPath determines whether or not a path can be that of a submodule's
// Git entry, which is never at or beneath another Git entry, and never the
// root's Git entry.
func isGitLinkPath(path string) bool {
	directory, ok := strings.CutSuffix(path, "/"+core.GitLinkName)
	return ok && gitignore.ShallowestGitEntry(directory) == ""
}

// removesGitContent determines whether or not a change would remove or replace
// a Git entry or anything beneath one, reporting each Git entry involved, with
// the exception of the changes to a submodule's Git entry that isGitLinkChange
// allows. Old content comes from the session's ancestor or the synchronizable
// part of a scan, and the only Git entries that those have are those of
// submodules, which are never removed (see refuseSubmoduleRemoval). Anything
// else can only be asked for by a malfunctioning or compromised controller, and
// removal acts on what it's given, unlike scans, which never look inside of Git
// entries. Files are safe from this, since they have to be in the scan's cache
// to be removed, but symbolic links and empty directories aren't.
func (p *gitTransitionPruner) removesGitContent(change *core.Change) bool {
	if gitEntry := gitignore.ShallowestGitEntry(change.Path); gitEntry != "" {
		if gitEntry == change.Path && isGitLinkChange(change) {
			return false
		} else if gitEntry == change.Path && change.Old != nil && change.Old.Kind == core.EntryKind_GitLink {
			p.refuseSubmoduleRemoval(gitEntry)
		} else {
			p.refuseGitEntry(gitEntry)
		}
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
				if content.Kind == core.EntryKind_GitLink {
					p.refuseSubmoduleRemoval(contentPathPrefix + name)
				} else {
					p.refuseGitEntry(contentPathPrefix + name)
				}
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
	// that's worth reporting, even if a parent is excluded too. The exception is
	// a submodule's Git entry, which may be written, but only into a directory
	// that isn't excluded, which is where scans would represent it.
	if gitEntry := gitignore.ShallowestGitEntry(path); gitEntry != "" {
		if gitEntry != path || entry.Kind != core.EntryKind_GitLink || !isGitLinkPath(path) {
			p.refuseGitEntry(gitEntry)
			return nil
		}
		directory := strings.TrimSuffix(path, "/"+core.GitLinkName)
		if excluded, err := ignore.Excluded(p.ignorer, directory, true); err != nil {
			p.fail(path, err)
			return nil
		} else if excluded != "" {
			p.warn(excluded)
			return nil
		}
		return entry
	}

	// Evaluate the path.
	directory := entry.Kind == core.EntryKind_Directory
	excluded, err := ignore.Excluded(p.ignorer, path, directory)
	if err != nil {
		p.fail(path, err)
		return nil
	}

	// A submodule that's being created is populated by a clone, so its contents
	// aren't written as they are here, but are what the clone yields, which is
	// subject to this endpoint's rules when it's scanned. Thus, it's either
	// populated or not at all.
	if entry.IsSubmodule() {
		if excluded != "" {
			p.warn(excluded)
			return nil
		}
		return entry
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
