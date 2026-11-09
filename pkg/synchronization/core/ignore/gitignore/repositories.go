package gitignore

import (
	"crypto/sha256"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// repositoryState is what a directory contributes to ignore evaluation as the
// potential top level of a repository's working tree (see readExcludeFile).
type repositoryState struct {
	// present indicates whether or not the directory has a ".git" entry.
	present bool
	// repository indicates whether or not the directory is the top level of a
	// repository's working tree.
	repository bool
	// gitDirectory is the location of the Git directory that the directory's
	// ".git" entry points to, whether or not Git accepts it, if any.
	gitDirectory string
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
func newRepositoryState(location repositoryLocation, contents []byte, err error) repositoryState {
	state := repositoryState{
		present:      location.present,
		repository:   location.repository,
		gitDirectory: location.gitDirectory,
		digest:       sha256.Sum256(contents),
	}
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
// top level of a repository's working tree depends on its Git directory too,
// which a ".git" file that Git doesn't accept yet (such as one that points to a
// Git directory that doesn't exist) can locate. Watching the root doesn't
// reveal changes to either, so a caller that reuses decisions from an earlier
// scan has to check for them.
type Repositories struct {
	// root is the synchronization root.
	root string
	// states maps the root-relative paths of the recorded directories to what
	// they contributed.
	states map[string]repositoryState
}

// RecordedRepositories returns what repositories contributed to the rules of an
// ignorer created by NewIgnorer so far. It records the root, whether or not it's
// in a repository, and each other directory found to have a ".git" entry,
// whether or not Git accepts it, including those whose rules couldn't be read.
// So a directory that the ignorer evaluated (see EnsureDirectory) and that
// isn't recorded had no ".git" entry. It returns nil for any other ignorer.
// Anything recorded by the ignorer after the call isn't included.
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

// Include adds what another record has for directories that this one doesn't
// have. What this record has for a directory is retained even if the other
// record differs, so that Changed still compares the directory with what this
// record's ignorer found. This extends the record of a scan with directories
// that a later scan found, when the later scan reused decisions from the earlier
// one. A nil record (of either kind) leaves the receiver as it is.
func (r *Repositories) Include(other *Repositories) {
	if r == nil || other == nil {
		return
	}
	for directory, state := range other.states {
		if _, ok := r.states[directory]; !ok {
			r.states[directory] = state
		}
	}
}

// Changed determines whether or not a new ignorer would find something
// different for any of the recorded directories: a change to whether or not it
// has a ".git" entry, to whether or not it's the top level of a repository's
// working tree, to the Git directory that its ".git" entry points to, to its
// exclude file's contents, or to the failure to read them. This includes the
// removal of a nested repository's directory, which then no longer has a ".git"
// entry, and a ".git" entry that Git didn't accept becoming one that it does,
// such as when the Git directory that a ".git" file points to is created. Only
// the recorded directories are checked. A nil record counts as changed, since
// nothing is known about the rules.
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

// ChangedAt determines whether or not a new ignorer would find something
// different for a single directory, specified by its root-relative path, as
// Changed does for the recorded directories. A directory that isn't recorded
// is compared with a directory without a ".git" entry, which is what an ignorer
// that evaluated it without recording it found (see RecordedRepositories). A
// nil record counts as changed.
func (r *Repositories) ChangedAt(directory string) bool {
	if r == nil {
		return true
	}
	state, ok := r.states[directory]
	if !ok {
		state = newRepositoryState(repositoryLocation{}, nil, nil)
	}
	return newRepositoryState(readExcludeFile(r.root, directory)) != state
}

// Agrees determines whether or not an ignorer created by NewIgnorer for a scan
// that reused decisions from the scans that this record is about found the same
// as they did, so that the scan's decisions are consistent. That's the case if
// it found the same for each recorded directory that it evaluated, including
// whether or not the directory has a ".git" entry, and if it found no other
// directory with a ".git" entry beneath which the scan may have reused
// decisions, which the specified function reports. The scans that this record
// is about evaluated any such directory and found no ".git" entry in it, since
// they would otherwise have recorded it (see RecordedRepositories), so the
// decisions beneath it were made for an ordinary directory, whose rules are
// those of the directories above it. Otherwise, the scan's decisions may be a
// mix of those for what each found. A directory whose rules the ignorer
// determined without examining its ".git" entry, because those of its parent
// couldn't be read, or because it isn't one that a scan would reach, counts as
// one without a ".git" entry, which errs towards disagreeing. Any other ignorer
// agrees with anything, since it doesn't read the rules, while a nil receiver
// doesn't agree with anything, since nothing is known about the rules (as with
// Changed).
func (r *Repositories) Agrees(i ignore.DirectoryIgnorer, reused func(directory string) bool) bool {
	scan, ok := i.(*ignorer)
	if r == nil {
		return false
	} else if !ok {
		return true
	}
	for directory, state := range scan.repositories {
		if recorded, ok := r.states[directory]; ok {
			if recorded != state {
				return false
			}
		} else if reused(directory) {
			return false
		}
	}
	for directory := range r.states {
		if _, found := scan.repositories[directory]; !found {
			if _, evaluated := scan.directories[directory]; evaluated {
				return false
			}
		}
	}
	return true
}
