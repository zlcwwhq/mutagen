package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// pull copies paths that the session ignores from the opposite endpoint,
// reporting the outcome for each path. The daemon resolves the destination,
// which may refuse it, before anything is read or written.
func pull(router *router, paths []string) error {
	target, err := router.resolve()
	if err != nil {
		return err
	}
	locator := newLocator(target)
	var failures []error
	for _, argument := range paths {
		display := terminal.NeutralizeControlCharacters(argument)
		if summary, err := pullPath(router, target, locator, argument); err != nil {
			fmt.Fprintf(router.stderr, "remote: unable to pull %s: %v\n", display, err)
			failures = append(failures, err)
		} else {
			fmt.Fprintf(router.stderr, "remote: pulled %s (%s)\n", display, summary)
		}
	}
	if len(failures) > 0 {
		return errors.Join(append([]error{errReported}, failures...)...)
	}
	return nil
}

// pullSummary counts the files and symbolic links that a pull added,
// replaced, updated only in metadata, or left unchanged, and records the entries
// that it couldn't merge.
type pullSummary struct {
	// added is the number of entries added.
	added int
	// replaced is the number of entries replaced.
	replaced int
	// metadataUpdated is the number of files replaced only to update metadata.
	metadataUpdated int
	// unchanged is the number of entries left unchanged.
	unchanged int
	// problems describe the entries that couldn't be merged.
	problems []string
}

// String implements fmt.Stringer.String.
func (s *pullSummary) String() string {
	if s.metadataUpdated > 0 {
		return fmt.Sprintf("%d added, %d replaced, %d metadata updated, %d unchanged", s.added, s.replaced, s.metadataUpdated, s.unchanged)
	}
	return fmt.Sprintf("%d added, %d replaced, %d unchanged", s.added, s.replaced, s.unchanged)
}

// pullPath copies a path that the session ignores from the opposite endpoint.
// The entry is transferred to a staging directory next to the local path,
// which scans skip, and then merged into the local path, which replaces files
// and symbolic links that differ, atomically for each of them, but never
// deletes local content. The session's root and paths that the session
// synchronizes, entirely or partially, aren't pulled, since writing them would
// race with synchronization, as would writing paths that another session
// synchronizes, which are refused for the sessions known on this system (see
// remote.Synchronizing). Paths are checked both as given and as the paths that
// writes to them land at, since symbolic links among their parents, which are
// only followed if the session ignores them (see prepareParents), can lead to
// content that the session or another session synchronizes.
func pullPath(router *router, target *remote.Target, locator *locator, argument string) (*pullSummary, error) {
	relative, err := locator.resolve(argument)
	if err != nil {
		return nil, err
	} else if relative == "" {
		return nil, errors.New("the session's root can't be pulled")
	}

	// Check that the session ignores the path as some kind of entry before
	// creating its parents, and check where writes to it land once they
	// exist, before transferring anything.
	followUp := router.followUp()
	fileErr, directoryErr := checkIgnored(target.Ignores, relative, false, followUp), checkIgnored(target.Ignores, relative, true, followUp)
	if fileErr != nil && directoryErr != nil {
		if target.Ignores.ExcludedInAnyForm(relative, true) != "" {
			return nil, directoryErr
		}
		return nil, fileErr
	}
	root, landing, err := prepareParents(router.ctx, target, locator, relative, followUp)
	if err != nil {
		return nil, err
	} else if err := checkLanding(target, locator, root, landing, false, "it", "write it"); err != nil &&
		checkLanding(target, locator, root, landing, true, "it", "write it") != nil {
		return nil, err
	}

	// Transfer the entry.
	staging, err := os.MkdirTemp(filepath.Dir(landing), filesystem.TemporaryNamePrefix+"pull-")
	if err != nil {
		return nil, fmt.Errorf("unable to create staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	staged := filepath.Join(staging, "entry")
	entry, err := extractVerified(router.ctx, staged, remote.ExtractSharedArchive, func(output io.Writer) (*remote.Entry, error) {
		connection, _, err := router.dial(remote.OperationExecute)
		if err != nil {
			return nil, err
		}
		return remote.Archive(router.ctx, connection, relative, output)
	})
	if err != nil {
		return nil, err
	}

	// Check that the session ignores the entry as the kind of entry that it
	// is, both as given and where it lands: a directory must be ignored
	// entirely, not just content beneath it. Then check that no other session
	// known on this system synchronizes it, whether or not it runs remote
	// commands, and that the session doesn't synchronize it through another of
	// its endpoints on this system, as both endpoints of local sessions are
	// (see remote.Synchronizing).
	var directory bool
	switch entry.Kind {
	case remote.EntryMissing:
		return nil, errors.New("it doesn't exist on the opposite endpoint")
	case remote.EntryDirectory:
		directory = true
	case remote.EntryFile, remote.EntrySymbolicLink:
	default:
		return nil, errors.New("it isn't a file, directory, or symbolic link on the opposite endpoint")
	}
	if err := checkIgnored(target.Ignores, relative, directory, followUp); err != nil {
		return nil, err
	} else if err := checkLanding(target, locator, root, landing, directory, "it", "write it"); err != nil {
		return nil, err
	} else if sessions, err := remote.Synchronizing(router.ctx, landing, directory); err != nil {
		return nil, err
	} else if len(sessions) > 0 {
		if physical, inside := within(root, landing); !inside || physical != relative {
			return nil, fmt.Errorf("it resolves to %s through a symbolic link, and that's synchronized by %s, so remote doesn't write it",
				describeLanding(locator, root, landing), strings.Join(sessions, ", "))
		}
		return nil, fmt.Errorf("it's synchronized by %s, so remote doesn't write it", strings.Join(sessions, ", "))
	}

	// Merge the entry into the local path.
	summary := &pullSummary{}
	summary.merge(staged, landing, locator.display(relative))
	if len(summary.problems) > 0 {
		return nil, fmt.Errorf("%s (%s)", strings.Join(summary.problems, "; "), summary)
	}
	return summary, nil
}

// partiallyIgnored explains that the session may synchronize an entry that its
// ignores exclude in some, but not all, of the forms in which scans could see
// its path, since scans may see it in a form that the session synchronizes
// (see remote.Ignores.ExcludedInEveryForm).
const partiallyIgnored = "the session ignores it in only some Unicode normalization forms of its path and may synchronize it"

// checkIgnored checks that the session ignores the entry at a path relative to
// the root, which is a directory if directory is true, in every form in which
// scans could see it (see remote.Ignores.ExcludedInEveryForm), since remote
// only writes content that the session doesn't synchronize. Otherwise, it
// explains why remote doesn't write it, suggesting remote command lines that
// start with followUp (see followUpCommand).
func checkIgnored(ignores *remote.Ignores, relative string, directory bool, followUp string) error {
	if ignores.ExcludedInEveryForm(relative, directory) != "" {
		return nil
	} else if ignores.ExcludedInAnyForm(relative, directory) != "" {
		return fmt.Errorf("%s, so remote doesn't write it; %s --ignores shows what the session ignores", partiallyIgnored, followUp)
	} else if directory {
		return errors.New("the session synchronizes some content beneath it, so remote doesn't pull it as a whole; pull the ignored content beneath it instead")
	}
	return fmt.Errorf("the session synchronizes it, so remote doesn't write it; %[1]s --flush synchronizes it, and %[1]s --ignores shows what the session ignores", followUp)
}

// within determines whether a path lies inside a root, both with symbolic
// links resolved, and, if so, computes its slash-separated path relative to the
// root.
func within(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(relative) || escapes(relative) {
		return "", false
	} else if relative == "." {
		return "", true
	}
	return filepath.ToSlash(relative), true
}

// describeLanding formats the path that writes land at for display, given the
// root, both with symbolic links resolved: relative to the working directory if
// it's inside the root (see locator.display), and absolute otherwise.
func describeLanding(locator *locator, root, landing string) string {
	if physical, inside := within(root, landing); inside {
		return locator.display(physical)
	}
	return terminal.NeutralizeControlCharacters(landing)
}

// checkLanding checks that the session ignores the path that writes to an
// entry land at, which is a directory if directory is true, in every form in
// which scans could see it, given that path, with the symbolic links among its
// parents resolved, and the root, with symbolic links resolved (see
// prepareParents). Inside the root, that's the path at which scans see the
// entry, whatever path it was written through. Paths that land outside the
// root may be synchronized by other sessions, which remote.Synchronizing
// determines for the sessions known on this system, but directories that
// contain the root are refused, since the session synchronizes content
// beneath them. If the check fails, the error explains why remote doesn't
// perform the specified action on the specified subject.
func checkLanding(target *remote.Target, locator *locator, root, landing string, directory bool, subject, action string) error {
	physical, inside := within(root, landing)
	if !inside {
		if _, contains := within(landing, root); directory && contains {
			return fmt.Errorf("%s resolves to %s through a symbolic link, which contains the session's root, so remote doesn't %s",
				subject, describeLanding(locator, root, landing), action)
		}
		return nil
	} else if target.Ignores.ExcludedInEveryForm(physical, directory) != "" {
		return nil
	}
	at := describeLanding(locator, root, landing)
	if physical == "" {
		return fmt.Errorf("%s resolves to the session's root (%s) through a symbolic link, so remote doesn't %s", subject, at, action)
	} else if target.Ignores.ExcludedInAnyForm(physical, directory) != "" {
		return fmt.Errorf("%s resolves to %s through a symbolic link, and the session ignores that in only some Unicode normalization forms of its path and may synchronize it, so remote doesn't %s",
			subject, at, action)
	}
	return fmt.Errorf("%s resolves to %s through a symbolic link, and the session synchronizes that, so remote doesn't %s", subject, at, action)
}

// prepareParents ensures that the parent directories of a path relative to the
// root exist as directories, and returns the root and the path that writes to
// the path land at, both with symbolic links resolved: symbolic links among its
// parents are resolved, since writes follow them, but its final component
// isn't, since merging replaces it rather than following it (see merge).
// Missing parents are only created if the session ignores them, since
// synchronization creates the others. Symbolic links are only followed if the
// session ignores them, since synchronization could otherwise replace them.
// Parents are checked both as given and as the paths that they land at once
// symbolic links are followed, which are the paths that scans see (see
// checkLanding). Missing parents are also only created if no other session
// known on this system synchronizes them, wherever they land (see
// remote.Synchronizing), since another session's root may lie in content that
// the session ignores, and that session would propagate them even if the pull
// were refused afterwards. Inside the root, the session's own endpoints don't
// count, since it ignores the parents where they land. Refusals suggest remote
// command lines that start with followUp (see followUpCommand).
func prepareParents(ctx context.Context, target *remote.Target, locator *locator, relative, followUp string) (string, string, error) {
	root, err := filepath.EvalSymlinks(target.Root)
	if err != nil {
		return "", "", fmt.Errorf("unable to access the synchronization root: %w", err)
	}
	components := strings.Split(relative, "/")
	resolved := root
	for c := 1; c < len(components); c++ {
		parent := strings.Join(components[:c], "/")
		display := locator.display(parent)
		native := filepath.Join(resolved, components[c-1])
		info, err := os.Lstat(native)
		if errors.Is(err, fs.ErrNotExist) {
			if target.Ignores.ExcludedInEveryForm(parent, true) == "" {
				if target.Ignores.ExcludedInAnyForm(parent, true) != "" {
					return "", "", fmt.Errorf("%s doesn't exist locally, and %s, so remote doesn't create it", display, partiallyIgnored)
				}
				return "", "", fmt.Errorf("%s doesn't exist locally, but the session synchronizes it, so remote doesn't create it; %s --flush synchronizes it", display, followUp)
			} else if err := checkLanding(target, locator, root, native, true, display, "create it"); err != nil {
				return "", "", err
			} else if sessions, err := remote.Synchronizing(ctx, native, true); err != nil {
				return "", "", err
			} else if len(sessions) > 0 {
				if physical, inside := within(root, native); !inside || physical != parent {
					return "", "", fmt.Errorf("%s resolves to %s through a symbolic link, and that's synchronized by %s, so remote doesn't create it",
						display, describeLanding(locator, root, native), strings.Join(sessions, ", "))
				}
				return "", "", fmt.Errorf("%s doesn't exist locally, and it's synchronized by %s, so remote doesn't create it",
					display, strings.Join(sessions, ", "))
			}
			if err := os.Mkdir(native, 0777); err != nil {
				return "", "", fmt.Errorf("unable to create %s: %w", display, err)
			}
			resolved = native
			continue
		} else if err != nil {
			return "", "", fmt.Errorf("unable to access %s: %w", display, err)
		} else if info.Mode()&os.ModeSymlink != 0 {
			if target.Ignores.ExcludedInEveryForm(parent, false) == "" {
				if target.Ignores.ExcludedInAnyForm(parent, false) != "" {
					return "", "", fmt.Errorf("%s is a symbolic link, and %s, so remote doesn't write through it", display, partiallyIgnored)
				}
				return "", "", fmt.Errorf("%s is a symbolic link that the session synchronizes, so remote doesn't write through it", display)
			} else if err := checkLanding(target, locator, root, native, false, display, "write through it"); err != nil {
				return "", "", err
			} else if resolved, err = filepath.EvalSymlinks(native); err != nil {
				return "", "", fmt.Errorf("unable to access %s: %w", display, err)
			} else if info, err = os.Stat(resolved); err != nil {
				return "", "", fmt.Errorf("unable to access %s: %w", display, err)
			}
		} else {
			resolved = native
		}
		if !info.IsDir() {
			return "", "", fmt.Errorf("%s isn't a directory locally", display)
		}
	}
	return root, filepath.Join(resolved, components[len(components)-1]), nil
}

// merge moves a staged entry to a destination, replacing files and symbolic
// links that differ, merging directories, and recording entries that can't be
// merged, such as directories whose destinations aren't directories, without
// deleting local content.
func (s *pullSummary) merge(source, destination, display string) {
	problem := func(format string, arguments ...any) {
		s.problems = append(s.problems, display+" "+fmt.Sprintf(format, arguments...))
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		problem("couldn't be staged: %v", err)
		return
	}
	destinationInfo, err := os.Lstat(destination)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(source, destination); err != nil {
			problem("couldn't be added: %v", err)
		} else {
			s.added += countEntries(destination)
		}
		return
	} else if err != nil {
		problem("couldn't be accessed: %v", err)
		return
	}
	if sourceInfo.IsDir() {
		if !destinationInfo.IsDir() {
			problem("exists locally but isn't a directory")
			return
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			problem("couldn't be read: %v", err)
			return
		}
		for _, entry := range entries {
			s.merge(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()), path.Join(display, entry.Name()))
		}
		return
	} else if destinationInfo.IsDir() {
		problem("exists locally as a directory")
		return
	}
	same, err := sameEntries(source, sourceInfo, destination, destinationInfo)
	if err != nil {
		problem("couldn't be compared: %v", err)
		return
	}
	// Compare the staged file's actual time so that the destination
	// filesystem's precision is already accounted for. Even metadata-only
	// changes replace the file atomically: changing the destination's time
	// directly would follow symbolic links and modify other hard links. Since
	// only the time is meant to change, such a replacement takes the
	// destination's permissions, which may be stricter than the staged file's
	// default ones (sameEntries already compared executability). It's still a
	// new file, so it doesn't share the destination's hard links, owner, or
	// extended attributes.
	metadataChanged := same && sourceInfo.Mode().IsRegular() && !sourceInfo.ModTime().Equal(destinationInfo.ModTime())
	if same && !metadataChanged {
		s.unchanged++
		return
	} else if metadataChanged {
		if err := os.Chmod(source, destinationInfo.Mode().Perm()); err != nil {
			problem("couldn't be replaced: unable to keep its permissions: %v", err)
			return
		}
	}
	if err := os.Rename(source, destination); err != nil {
		problem("couldn't be replaced: %v", err)
	} else if metadataChanged {
		s.metadataUpdated++
	} else {
		s.replaced++
	}
}

// countEntries counts the files and symbolic links at and beneath a path.
func countEntries(root string) int {
	count := 0
	filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// sameEntries determines whether two entries other than directories are the
// same: files with the same contents and executability, or symbolic links with
// the same target.
func sameEntries(a string, aInfo os.FileInfo, b string, bInfo os.FileInfo) (bool, error) {
	if aInfo.Mode().Type() != bInfo.Mode().Type() {
		return false, nil
	} else if aInfo.Mode()&os.ModeSymlink != 0 {
		aTarget, err := os.Readlink(a)
		if err != nil {
			return false, err
		}
		bTarget, err := os.Readlink(b)
		return err == nil && aTarget == bTarget, err
	} else if !aInfo.Mode().IsRegular() || aInfo.Size() != bInfo.Size() || aInfo.Mode()&0111 != bInfo.Mode()&0111 {
		return false, nil
	}
	aFile, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer aFile.Close()
	bFile, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer bFile.Close()
	aBuffer, bBuffer := make([]byte, 64*1024), make([]byte, 64*1024)
	for {
		aCount, aErr := io.ReadFull(aFile, aBuffer)
		bCount, bErr := io.ReadFull(bFile, bBuffer)
		if aCount != bCount || !bytes.Equal(aBuffer[:aCount], bBuffer[:bCount]) {
			return false, nil
		} else if aErr == io.EOF || aErr == io.ErrUnexpectedEOF {
			return bErr == aErr, nil
		} else if aErr != nil {
			return false, aErr
		} else if bErr != nil {
			return false, bErr
		}
	}
}
