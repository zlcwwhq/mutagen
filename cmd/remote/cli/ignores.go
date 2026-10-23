package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// describeExclusion describes whether the session synchronizes the entry at a
// path relative to the root, which is a directory if directory is true, as
// remote --pull determines it (see checkIgnored and checkLanding), given the
// root and the path that writes to the entry land at, both with symbolic links
// resolved (see resolveLanding), and returns whether the session ignores it.
// The entry is ignored if the session ignores it in every form in which scans
// could see its path (see remote.Ignores.ExcludedInEveryForm), and possibly
// synchronized if it ignores it in only some of them, so that remote --pull
// doesn't write it. An ignored path that resolves through symbolic links among
// its parents to another path inside the root, which is the path that scans
// see, is synchronized as that path unless the session ignores it too. Other
// sessions, and the requirements that remote --pull places on parents, such as
// that symbolic links among them be ignored, aren't considered.
func describeExclusion(target *remote.Target, locator *locator, relative, root, landing string, directory bool) (string, bool) {
	ignores := target.Ignores
	excluded := ignores.ExcludedInEveryForm(relative, directory)
	if excluded == "" {
		if partially := ignores.ExcludedInAnyForm(relative, directory); partially != "" {
			return fmt.Sprintf("possibly synchronized (%s)", describePartialExclusion(locator, relative, partially)), false
		}
		return "synchronized", false
	} else if physical, inside := within(root, landing); inside && physical != relative && ignores.ExcludedInEveryForm(physical, directory) == "" {
		at := locator.display(physical)
		if partially := ignores.ExcludedInAnyForm(physical, directory); partially != "" {
			return fmt.Sprintf("possibly synchronized as %s (through a symbolic link; %s)", at, describePartialExclusion(locator, physical, partially)), false
		}
		return fmt.Sprintf("synchronized as %s (through a symbolic link)", at), false
	} else if excluded == relative {
		return "ignored", true
	}
	return fmt.Sprintf("ignored (inside %s, which the session ignores)", locator.display(excluded)), true
}

// describePartialExclusion explains that the session ignores the entry at a
// path relative to the root in only some of the forms in which scans could see
// its path, given the path of the entry that's excluded in some of them, which
// is either the entry itself or one of its parents (see
// remote.Ignores.ExcludedInAnyForm).
func describePartialExclusion(locator *locator, relative, excluded string) string {
	subject := "it"
	if excluded != relative {
		subject = locator.display(excluded)
	}
	return fmt.Sprintf("the session ignores %s in only some Unicode normalization forms of its path", subject)
}

// resolveLanding determines the path that writes to a path relative to the
// root would land at, as prepareParents does, but without creating or checking
// anything: symbolic links among the parents that exist are resolved, while
// missing parents and the final component are joined as given. It returns the
// root and that path, both with symbolic links resolved, or, if the root can't
// be accessed, the root as given and the path joined to it as given.
func resolveLanding(root, relative string) (string, string) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root, filepath.Join(root, filepath.FromSlash(relative))
	}
	root = resolvedRoot
	native := filepath.Join(root, filepath.FromSlash(relative))
	parent, rest := filepath.Dir(native), filepath.Base(native)
	for {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return root, filepath.Join(resolved, rest)
		} else if parent == root || parent == filepath.Dir(parent) {
			return root, native
		}
		parent, rest = filepath.Dir(parent), filepath.Join(filepath.Base(parent), rest)
	}
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
	// file and a directory if it doesn't exist locally, and as the path that
	// it resolves to through symbolic links among its parents.
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
		root, landing := resolveLanding(target.Root, relative)
		file, fileIgnored := describeExclusion(target, locator, relative, root, landing, false)
		directory, directoryIgnored := describeExclusion(target, locator, relative, root, landing, true)
		if info, err := os.Lstat(filepath.Join(target.Root, filepath.FromSlash(relative))); err == nil {
			if info.IsDir() {
				file, fileIgnored = directory, directoryIgnored
			} else {
				directory, directoryIgnored = file, fileIgnored
			}
		}
		if fileIgnored || directoryIgnored {
			code = 1
		}
		if file == directory {
			fmt.Fprintf(output, "%s: %s\n", display, file)
		} else {
			fmt.Fprintf(output, "%s: %s as a file, %s as a directory\n", display, file, directory)
		}
	}
	return code, nil
}
