package gitignore

import (
	"crypto/sha256"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// repositoryState is what a directory contributes to ignore evaluation as the
// potential top level of a repository's working tree (see readExcludeFile).
type repositoryState struct {
	// repository indicates whether or not the directory is the top level of a
	// repository's working tree.
	repository bool
	// digest is the SHA-256 digest of the exclude file's contents. A missing
	// exclude file has the digest of empty contents, since it has the same
	// effect as an empty one.
	digest [sha256.Size]byte
	// err is the failure to locate or read the exclude file, or an empty string
	// if there was none.
	err string
}

// newRepositoryState creates a repositoryState from the results of
// readExcludeFile.
func newRepositoryState(repository bool, contents []byte, err error) repositoryState {
	state := repositoryState{repository: repository, digest: sha256.Sum256(contents)}
	if err != nil {
		state.err = err.Error()
	}
	return state
}

// Repositories records what repositories contributed to the rules of an
// ignorer, so that it can later be determined whether or not a new ignorer
// would find the same (see Changed). A repository's exclude file is in its Git
// directory, which can be outside of the synchronization root, as is the case
// when the root is a linked worktree, whose exclude file is in the common
// directory of its repository, or a submodule, whose Git directory is usually
// within that of the enclosing repository. Whether or not a directory is the
// top level of a repository's working tree depends on its Git directory too.
// Watching the root doesn't reveal changes to either, so a caller that reuses
// decisions from an earlier scan has to check for them.
type Repositories struct {
	// root is the synchronization root.
	root string
	// states maps the root-relative paths of the recorded directories to what
	// they contributed.
	states map[string]repositoryState
}

// RecordedRepositories returns what repositories contributed to the rules of an
// ignorer created by NewIgnorer so far. It records the root, whether or not it's
// in a repository, and each directory found to be the top level of a nested
// repository's working tree, including those whose rules couldn't be read. It
// returns nil for any other ignorer. Anything recorded by the ignorer after the
// call isn't included.
func RecordedRepositories(i ignore.DirectoryIgnorer) *Repositories {
	recording, ok := i.(*ignorer)
	if !ok {
		return nil
	}
	states := make(map[string]repositoryState, len(recording.repositories))
	for directory, state := range recording.repositories {
		states[directory] = state
	}
	return &Repositories{root: recording.root, states: states}
}

// Contains determines whether or not a record has the same as another record
// for every directory in the other record. A scan that reuses decisions from
// earlier scans evaluates the directories along its re-check paths again, and
// if it finds something other than what the earlier scans recorded for one of
// them, then the decisions that it reused beneath that directory were made
// with other rules. That includes a directory that the earlier scans didn't
// record, such as an existing directory that has since become the top level
// of a repository's working tree, but also a new one, for which nothing was
// reused. A nil record (of either kind) doesn't count as containing anything,
// since nothing is known about the rules.
func (r *Repositories) Contains(other *Repositories) bool {
	if r == nil || other == nil {
		return false
	}
	for directory, state := range other.states {
		if recorded, ok := r.states[directory]; !ok || recorded != state {
			return false
		}
	}
	return true
}

// Changed determines whether or not a new ignorer would find something
// different for any of the recorded directories: a change to whether or not
// it's the top level of a repository's working tree, to its exclude file's
// contents, or to the failure to read them. This includes the removal of a
// nested repository's directory, which is then no longer a repository's top
// level. Only the recorded directories are checked. A nil record counts as
// changed, since nothing is known about the rules.
func (r *Repositories) Changed() bool {
	if r == nil {
		return true
	}
	for directory, state := range r.states {
		if newRepositoryState(readExcludeFile(r.root, directory)) != state {
			return true
		}
	}
	return false
}
