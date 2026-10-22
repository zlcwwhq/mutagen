package cli

import (
	"bytes"
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
// reporting the outcome for each path.
func pull(router *router, paths []string) error {
	target, err := router.locate()
	if err != nil {
		return err
	}
	locator := newLocator(target)
	failed := false
	for _, argument := range paths {
		display := terminal.NeutralizeControlCharacters(argument)
		if summary, err := pullPath(router, target, locator, argument); err != nil {
			fmt.Fprintf(router.stderr, "remote: unable to pull %s: %v\n", display, err)
			failed = true
		} else {
			fmt.Fprintf(router.stderr, "remote: pulled %s (%s)\n", display, summary)
		}
	}
	if failed {
		return errReported
	}
	return nil
}

// pullSummary counts the files and symbolic links that a pull added,
// replaced, or left unchanged, and records the entries that it couldn't merge.
type pullSummary struct {
	// added is the number of entries added.
	added int
	// replaced is the number of entries replaced.
	replaced int
	// unchanged is the number of entries left unchanged.
	unchanged int
	// problems describe the entries that couldn't be merged.
	problems []string
}

// String implements fmt.Stringer.String.
func (s *pullSummary) String() string {
	return fmt.Sprintf("%d added, %d replaced, %d unchanged", s.added, s.replaced, s.unchanged)
}

// pullPath copies a path that the session ignores from the opposite endpoint.
// The entry is transferred to a staging directory next to the local path,
// which scans skip, and then merged into the local path, which replaces files
// and symbolic links that differ, atomically for each of them, but never
// deletes local content. The session's root and paths that the session
// synchronizes, entirely or partially, aren't pulled, since writing them would
// race with synchronization, as would writing paths that another session
// synchronizes.
func pullPath(router *router, target *remote.Target, locator *locator, argument string) (*pullSummary, error) {
	relative, err := locator.resolve(argument)
	if err != nil {
		return nil, err
	} else if relative == "" {
		return nil, errors.New("the session's root can't be pulled")
	} else if target.Ignores.Excluded(relative, false) == "" && target.Ignores.Excluded(relative, true) == "" {
		return nil, errors.New("the session synchronizes it, so remote doesn't write it; remote --flush synchronizes it, and remote --ignores shows what the session ignores")
	}
	destination := filepath.Join(target.Root, filepath.FromSlash(relative))
	if err := prepareParents(target, locator, relative); err != nil {
		return nil, err
	}

	// Transfer the entry.
	staging, err := os.MkdirTemp(filepath.Dir(destination), filesystem.TemporaryNamePrefix+"pull-")
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
	// is: a directory must be ignored entirely, not just content beneath it.
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
	if target.Ignores.Excluded(relative, directory) == "" {
		if directory {
			return nil, errors.New("the session synchronizes some content beneath it, so remote doesn't pull it as a whole; pull the ignored content beneath it instead")
		}
		return nil, errors.New("the session synchronizes it, so remote doesn't write it; remote --flush synchronizes it")
	} else if sessions, err := remote.Synchronizing(router.ctx, destination, directory, target); err != nil {
		return nil, err
	} else if len(sessions) > 0 {
		return nil, fmt.Errorf("it's synchronized by %s, so remote doesn't write it", strings.Join(sessions, ", "))
	}

	// Merge the entry into the local path.
	summary := &pullSummary{}
	summary.merge(staged, destination, locator.display(relative))
	if len(summary.problems) > 0 {
		return nil, fmt.Errorf("%s (%s)", strings.Join(summary.problems, "; "), summary)
	}
	return summary, nil
}

// prepareParents ensures that the parent directories of a path relative to the
// root exist as directories. Missing parents are only created if the session
// ignores them, since synchronization creates the others. Symbolic links are
// only followed if the session ignores them, since synchronization could
// otherwise replace them.
func prepareParents(target *remote.Target, locator *locator, relative string) error {
	components := strings.Split(relative, "/")
	for c := 1; c < len(components); c++ {
		parent := strings.Join(components[:c], "/")
		native := filepath.Join(target.Root, filepath.FromSlash(parent))
		info, err := os.Lstat(native)
		if errors.Is(err, fs.ErrNotExist) {
			if target.Ignores.Excluded(parent, true) == "" {
				return fmt.Errorf("%s doesn't exist locally, but the session synchronizes it, so remote doesn't create it; remote --flush synchronizes it", locator.display(parent))
			} else if err := os.Mkdir(native, 0777); err != nil {
				return fmt.Errorf("unable to create %s: %w", locator.display(parent), err)
			}
			continue
		} else if err != nil {
			return fmt.Errorf("unable to access %s: %w", locator.display(parent), err)
		} else if info.Mode()&os.ModeSymlink != 0 {
			if target.Ignores.Excluded(parent, false) == "" {
				return fmt.Errorf("%s is a symbolic link that the session synchronizes, so remote doesn't write through it", locator.display(parent))
			} else if info, err = os.Stat(native); err != nil {
				return fmt.Errorf("unable to access %s: %w", locator.display(parent), err)
			}
		}
		if !info.IsDir() {
			return fmt.Errorf("%s isn't a directory locally", locator.display(parent))
		}
	}
	return nil
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
	} else if same, err := sameEntries(source, sourceInfo, destination, destinationInfo); err != nil {
		problem("couldn't be compared: %v", err)
	} else if same {
		s.unchanged++
	} else if err := os.Rename(source, destination); err != nil {
		problem("couldn't be replaced: %v", err)
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
