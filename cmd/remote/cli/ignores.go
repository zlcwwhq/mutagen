package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
)

// describeExclusion describes whether the session synchronizes the path,
// relative to the root, given the path of the entry that keeps it from being
// synchronized, if any (see remote.Ignores.Excluded).
func describeExclusion(locator *locator, relative, excluded string) string {
	if excluded == "" {
		return "synchronized"
	} else if excluded == relative {
		return "ignored"
	}
	return fmt.Sprintf("ignored (inside %s, which the session ignores)", locator.display(excluded))
}

// printIgnores prints the session's ignores or, if paths are specified,
// whether the session synchronizes them, and returns 1 if it ignores any of
// them, and 0 otherwise.
func printIgnores(router *router, paths []string, output io.Writer) (int, error) {
	target, err := router.locate()
	if err != nil {
		return 0, err
	}
	ignores := target.Ignores
	session := terminal.NeutralizeControlCharacters(target.Session)
	if target.Name != "" {
		session = terminal.NeutralizeControlCharacters(target.Name)
	}
	if len(paths) == 0 {
		if ignores == nil {
			fmt.Fprintf(output, "The ignores of session %s aren't known.\n", session)
			return 0, nil
		} else if len(ignores.Patterns) == 0 && !ignores.VCS {
			fmt.Fprintf(output, "Session %s ignores nothing.\n", session)
			return 0, nil
		}
		fmt.Fprintf(output, "Session %s ignores (%s syntax):\n", session, ignores.Syntax.Description())
		for _, pattern := range ignores.Patterns {
			fmt.Fprintf(output, "  %s\n", terminal.NeutralizeControlCharacters(pattern))
		}
		if ignores.VCS {
			fmt.Fprintln(output, "  VCS directories (.git, .svn, .hg, .bzr, and _darcs)")
		}
		fmt.Fprintln(output, "Ignored paths aren't synchronized; remote --ignores <path> checks a path, and remote --pull <path> copies ignored paths from the opposite endpoint.")
		return 0, nil
	}

	// Check each path, as the kind of entry that it is locally, or as both a
	// file and a directory if it doesn't exist locally.
	locator := newLocator(target)
	code := 0
	for _, path := range paths {
		relative, err := locator.resolve(path)
		if err != nil {
			return 0, err
		}
		display := terminal.NeutralizeControlCharacters(path)
		if relative == "" {
			fmt.Fprintf(output, "%s: synchronized (the session's root)\n", display)
			continue
		}
		file, directory := ignores.Excluded(relative, false), ignores.Excluded(relative, true)
		if info, err := os.Lstat(filepath.Join(target.Root, filepath.FromSlash(relative))); err == nil {
			if info.IsDir() {
				file = directory
			} else {
				directory = file
			}
		}
		if file != "" || directory != "" {
			code = 1
		}
		if file == directory {
			fmt.Fprintf(output, "%s: %s\n", display, describeExclusion(locator, relative, file))
		} else {
			fmt.Fprintf(output, "%s: %s as a file, %s as a directory\n", display,
				describeExclusion(locator, relative, file), describeExclusion(locator, relative, directory))
		}
	}
	return code, nil
}
