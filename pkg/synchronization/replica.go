package synchronization

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/fastpath"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

// exposedContentProblem is the message of the problem that beta reports for
// content that a session in the one-way-replica mode keeps (see
// withholdExposedChanges).
const exposedContentProblem = "isn't removed or overwritten automatically, since rules ignored it on this endpoint until they changed, so it was never synchronized; " +
	"delete it to let the session replace it with alpha's content, or ignore it again, such as in this endpoint's .git/info/exclude or with ignore.paths"

// In the one-way-replica mode, beta mirrors alpha, so content that only beta
// has is removed, and content that beta has changed is overwritten. In Git
// mode, content that beta's ignore rules ignore is untracked, and it's thus left
// alone (see core.ReconcileGitMode), but only for as long as the rules ignore
// it. Rules change, and not always by beta's choice: the session itself removes
// a rule file that only beta has, replaces beta's copy of one with alpha's, and
// delivers the removal of a rule from alpha. Beta's own exclude file can change
// too. If the content that a rule ignored on beta was then tracked like any
// other content, the cycle after the change would remove or overwrite it, even
// though it was never synchronized, and even though a rule change can't delete
// anything in other modes.
//
// So a session in the one-way-replica mode with Git mode enabled keeps a record
// of the paths of the untracked entries in beta's last snapshot. If the content
// at one of those paths is tracked in the next snapshot, then a change of rules
// has exposed it, unless it's identical to alpha's, in which case there's
// nothing to protect. The session doesn't remove or overwrite exposed content,
// which beta reports as a problem, until it's gone from beta, identical to
// alpha's, or ignored again. Content at other paths is unaffected, so content
// that beta creates is still removed, as before, and alpha's content can still
// be added beneath exposed content, since that doesn't remove or overwrite
// anything of beta's.
//
// The record is kept by the controller rather than by beta's endpoint, so that
// it works the same for every kind of endpoint, without changes to their
// protocols or storage. It only involves the untracked entries that beta's
// snapshots contain, so beta's rules are only evaluated by beta. The record is
// saved after each scan that changes it, before anything is staged or
// transitioned, and in particular before the rule files that a cycle delivers
// change what's ignored, so that a restart between that cycle and the next one
// doesn't lose it. If it can't be saved or loaded, then synchronization stops,
// rather than proceeding without it.

// pathForReplicaRecord computes the path to the replica record (see
// ReplicaRecord) for the given session identifier, which is next to the
// session's archive.
func pathForReplicaRecord(session string) (string, error) {
	archivePath, err := pathForArchive(session)
	if err != nil {
		return "", err
	}
	return archivePath + ".replica", nil
}

// EnsureValid ensures that ReplicaRecord's invariants are respected.
func (r *ReplicaRecord) EnsureValid() error {
	// A nil record is invalid.
	if r == nil {
		return errors.New("nil replica record")
	}

	// Ensure that the paths are sorted, unique, and not the root, which can't
	// be exposed, since it's never untracked.
	for _, paths := range [][]string{r.Untracked, r.Exposed} {
		for p, path := range paths {
			if path == "" {
				return errors.New("root path in replica record")
			} else if p > 0 && paths[p-1] >= path {
				return errors.New("unsorted or duplicate paths in replica record")
			}
		}
	}

	// Success.
	return nil
}

// loadReplicaRecord loads a replica record. A record that doesn't exist is
// empty, since there's nothing to record before a session's first scan, but a
// record that can't be loaded is an error.
func loadReplicaRecord(path string) (*ReplicaRecord, error) {
	record := &ReplicaRecord{}
	if err := encoding.LoadAndUnmarshalProtobuf(path, record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ReplicaRecord{}, nil
		}
		return nil, err
	} else if err := record.EnsureValid(); err != nil {
		return nil, fmt.Errorf("invalid replica record: %w", err)
	}
	return record, nil
}

// presence describes what a snapshot's content has at a path.
type presence uint8

const (
	// presenceAbsent indicates that there's nothing at the path.
	presenceAbsent presence = iota
	// presenceUntracked indicates that the path is at or beneath an untracked
	// entry.
	presenceUntracked
	// presenceUnknown indicates that the path is at or beneath a problematic
	// entry, whose content is unknown.
	presenceUnknown
	// presencePresent indicates that there's synchronizable content at the
	// path, which may contain untracked or problematic content.
	presencePresent
)

// lookupPresence determines what an entry, which is at the root, has at a path,
// returning the entry at the path if it's present.
func lookupPresence(root *core.Entry, path string) (*core.Entry, presence) {
	entry := root
	for remaining := path; ; {
		// Determine what's at this level.
		if entry == nil {
			return nil, presenceAbsent
		} else if entry.Kind == core.EntryKind_Untracked {
			return nil, presenceUntracked
		} else if entry.Kind == core.EntryKind_Problematic {
			return nil, presenceUnknown
		} else if remaining == "" {
			return entry, presencePresent
		}

		// Descend into the next component, if this is a directory.
		if entry.Kind != core.EntryKind_Directory {
			return nil, presenceAbsent
		}
		var name string
		name, remaining, _ = strings.Cut(remaining, "/")
		entry = entry.Contents[name]
	}
}

// appendUntracked appends the paths of the untracked entries within an entry at
// a path to a list of paths. Git entries are excluded, since they're never
// synchronized, whatever the rules say, and since the only one that can stop
// being untracked is a submodule's, which beta keeps in any case (see the local
// endpoint's gitTransitionPruner).
func appendUntracked(paths []string, path string, entry *core.Entry) []string {
	if entry == nil {
		return paths
	} else if entry.Kind == core.EntryKind_Untracked {
		if path != "" && !gitignore.IsGitEntry(fastpath.Base(path)) {
			paths = append(paths, path)
		}
		return paths
	}
	contentPathPrefix := fastpath.Joinable(path)
	for name, content := range entry.Contents {
		paths = appendUntracked(paths, contentPathPrefix+name, content)
	}
	return paths
}

// updateReplicaRecord computes the replica record for the snapshots of a
// synchronization cycle from the previous cycle's record, and determines
// whether or not it differs from that record. Content that was untracked on
// beta, or that's already exposed, is exposed if beta has it, unless it's
// identical to alpha's. Recorded paths at or beneath problematic content on beta,
// which may still be there, are kept as they are.
func updateReplicaRecord(previous *ReplicaRecord, alpha, beta *core.Entry) (*ReplicaRecord, bool) {
	// Record the untracked content in beta's snapshot.
	untracked := appendUntracked(nil, "", beta)

	// Determine what's still exposed or newly exposed.
	var exposed []string
	consider := func(path string, wasUntracked bool) {
		content, betaPresence := lookupPresence(beta, path)
		if betaPresence == presenceUnknown {
			if wasUntracked {
				untracked = append(untracked, path)
			} else {
				exposed = append(exposed, path)
			}
		} else if betaPresence == presencePresent {
			if alphaContent, alphaPresence := lookupPresence(alpha, path); alphaPresence != presencePresent || !alphaContent.Equal(content, true) {
				exposed = append(exposed, path)
			}
		}
	}
	for _, path := range previous.Exposed {
		consider(path, false)
	}
	for _, path := range previous.Untracked {
		consider(path, true)
	}

	// Create the record.
	slices.Sort(untracked)
	slices.Sort(exposed)
	record := &ReplicaRecord{
		Untracked: slices.Compact(untracked),
		Exposed:   slices.Compact(exposed),
	}
	changed := !slices.Equal(record.Untracked, previous.Untracked) ||
		!slices.Equal(record.Exposed, previous.Exposed)
	return record, changed
}

// exposedContentAffected returns the exposed paths whose content on beta a
// transition would remove or replace, which is the case if beta has content at
// one of them, as the transition's old content shows, and the transition is at
// or above that path, or if the transition is beneath one of them and changes
// existing content. A transition that creates content where beta has nothing
// doesn't affect anything.
func exposedContentAffected(exposed []string, isExposed map[string]bool, transition *core.Change) []string {
	// A transition without old content doesn't remove or replace anything.
	if transition.Old == nil {
		return nil
	}

	// Check whether the transition is at or beneath exposed content.
	for path := transition.Path; path != ""; path = fastpath.Dir(path) {
		if isExposed[path] {
			return []string{path}
		}
	}

	// Check for exposed content beneath the transition.
	var affected []string
	prefix := fastpath.Joinable(transition.Path)
	for _, path := range exposed {
		if relative, ok := strings.CutPrefix(path, prefix); ok && relative != "" {
			if _, oldPresence := lookupPresence(transition.Old, relative); oldPresence != presenceAbsent {
				affected = append(affected, path)
			}
		}
	}
	return affected
}

// withholdExposedChanges removes the transitions for beta that would remove or
// replace exposed content (see exposedContentAffected), and returns the
// remaining transitions along with a problem for each exposed path whose
// content is kept. The transitions that are removed aren't performed, so the
// ancestor doesn't record any of their content, and they're computed again by
// the next cycle, and withheld again, for as long as the content is exposed.
func withholdExposedChanges(exposed []string, transitions []*core.Change) ([]*core.Change, []*core.Problem) {
	// If nothing is exposed, then nothing is withheld.
	if len(exposed) == 0 {
		return transitions, nil
	}

	// Index the exposed paths.
	isExposed := make(map[string]bool, len(exposed))
	for _, path := range exposed {
		isExposed[path] = true
	}

	// Filter the transitions and report what they would have affected.
	var retained []*core.Change
	var problems []*core.Problem
	reported := make(map[string]bool)
	for _, transition := range transitions {
		affected := exposedContentAffected(exposed, isExposed, transition)
		if len(affected) == 0 {
			retained = append(retained, transition)
			continue
		}
		for _, path := range affected {
			if !reported[path] {
				reported[path] = true
				problems = append(problems, &core.Problem{Path: path, Error: exposedContentProblem})
			}
		}
	}
	return retained, problems
}
