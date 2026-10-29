package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// testingContentManager generates and removes test content on disk using
// Entry-based definitions and Transition. For testing content that can't be
// expressed using these definitions (e.g. intentionally problematic content
// used to test error handling), custom functions can be specified to tweak
// baseline content by operating directly on the underlying filesystem.
type testingContentManager struct {
	// storage is the path where temporary directories (that serve as parent
	// directories to content) will be created. This will be used as the first
	// argument to os.MkdirTemp and may be empty to indicate that the default
	// temporary directory should be used.
	storage string
	// parent is the temporary directory where content currently exists on disk.
	// It is an internal member and should not be set or modified directly.
	parent string
	// baseline is the baseline content to be generated.
	baseline *Entry
	// baselineContentMap is the content map for baseline.
	baselineContentMap testingContentMap
	// tweak is an optional callback that will be provided with the path to the
	// generated baseline content root when generating content. It can perform
	// arbitrary filesystem modifications.
	tweak func(string) error
	// untweak is an optional callback that will be provided with the path to
	// the generated baseline content root when removing content. By default,
	// removal uses os.RemoveAll to remove on-disk content, but for content that
	// is created with intentionally problematic characteristics by a tweak
	// operation (or perhaps by the test using the generator), os.RemoveAll may
	// fail. If untweak is specified, it will be called with the path to the
	// generated content root and can attempt to remove content or at least make
	// it suitable for removal by os.RemoveAll.
	untweak func(string) error
}

// generate generates test content on disk. It returns the path to the generated
// content root.
func (g *testingContentManager) generate() (string, error) {
	// If there's already on-disk content, then don't regenerate.
	if g.parent != "" {
		return "", errors.New("on-disk content already generated")
	}

	// Track success.
	var successful bool

	// Create a temporary directory to serve as the parent of the content root.
	if p, err := os.MkdirTemp(g.storage, "mutagen_testing_content"); err != nil {
		return "", fmt.Errorf("unable to generate temporary directory: %w", err)
	} else {
		g.parent = p
	}

	// If we don't generate content successfully, then remove and unset the
	// parent directory when we return.
	defer func() {
		if !successful {
			os.RemoveAll(g.parent)
			g.parent = ""
		}
	}()

	// Compute the path to the content root.
	root := filepath.Join(g.parent, "root")

	// Transition the baseline content into existence.
	//
	// HACK: We're relying on internal knowledge of Transition's behavior given
	// the provided argument, specifically the fact that it doesn't validate the
	// symbolic link mode on a per-platform basis like Scan does, the fact that
	// POSIX raw mode will work fine for a pure creation operation on Windows,
	// and the fact that Unicode composition doesn't enter the picture for pure
	// creation operations. This allows us to make the interface for and usage
	// of testingContentManager much simpler. Fortunately, if any of these
	// invariants change, it'll be picked up immediately by tests.
	creation := &Change{New: g.baseline}
	provider := &testingProvider{
		storage:    g.parent,
		contentMap: g.baselineContentMap,
		hasher:     newTestingHasher(),
	}
	results, problems, missingFiles := Transition(
		context.Background(),
		root,
		[]*Change{creation},
		nil,
		newTestingHasher(),
		SymbolicLinkMode_SymbolicLinkModePOSIXRaw,
		0600,
		0700,
		nil,
		false,
		provider,
	)
	if missingFiles {
		return "", errors.New("content map missing file definitions")
	} else if len(problems) > 0 {
		return "", errors.New("problems encountered during creation")
	} else if len(results) != 1 {
		return "", errors.New("invalid number of results from Transition")
	} else if !results[0].Equal(g.baseline, true) {
		return "", errors.New("generated content did not match baseline")
	}

	// Run the tweak operation, if specified.
	if g.tweak != nil {
		if err := g.tweak(root); err != nil {
			return "", fmt.Errorf("unable to tweak content root: %w", err)
		}
	}

	// Backdate the generated files, including any that the tweak operation
	// created, so that they appear to have been modified well before any scan
	// of them. Otherwise, their digests would be racy (see CacheEntry.Racy),
	// and every scan would hash them again, which would make it impossible to
	// test cache reuse. Tests still modify files at the current time between
	// scans, which scans detect as modifications. Content that doesn't exist
	// (such as an absent root) or that the tweak operation made inaccessible
	// has nothing to backdate.
	modificationTime := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		} else if entry.Type().IsRegular() {
			return os.Chtimes(path, modificationTime, modificationTime)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("unable to backdate content: %w", err)
	}

	// Success.
	successful = true
	return root, nil
}

// remove removes test content from disk.
func (g *testingContentManager) remove() error {
	// If there's no on-disk content, then there's nothing to remove.
	if g.parent == "" {
		return errors.New("no on-disk content generated")
	}

	// Unset the parent (because it'll be invalid after this point anyway).
	parent := g.parent
	g.parent = ""

	// Compute the content root path.
	root := filepath.Join(parent, "root")

	// Run the untweak operation, if specified. Even if it fails, we'll still
	// try a removal operation to attempt cleanup.
	if g.untweak != nil {
		if err := g.untweak(root); err != nil {
			os.RemoveAll(parent)
			return fmt.Errorf("unable to untweak content root: %w", err)
		}
	}

	// Perform the removal operation.
	if err := os.RemoveAll(parent); err != nil {
		return fmt.Errorf("unable to remove content: %w", err)
	}

	// Success.
	return nil
}
