package cli

import (
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// locator maps between slash-separated paths relative to the submitting root
// and local paths.
type locator struct {
	// root is the submitting root.
	root string
	// base is the slash-separated working directory relative to root, if
	// contained is true.
	base string
	// contained indicates whether the working directory lies inside root.
	contained bool
}

// newLocator creates a locator for the target of a submission.
func newLocator(target *remote.Target) *locator {
	return &locator{root: target.Root, base: target.Path, contained: target.Contained}
}

// local converts a path relative to the root to a local path, relative to the
// working directory if it's inside the root and absolute otherwise.
func (l *locator) local(relative string) string {
	local := filepath.Join(l.root, filepath.FromSlash(relative))
	if l.contained {
		if result, err := filepath.Rel(filepath.Join(l.root, filepath.FromSlash(l.base)), local); err == nil {
			local = result
		}
	}
	return local
}

// display formats a path relative to the root for display (see local), with
// control characters neutralized.
func (l *locator) display(relative string) string {
	return terminal.NeutralizeControlCharacters(l.local(relative))
}

// resolve converts a local path argument to a path relative to the root.
// Relative arguments are interpreted against the working directory if it's
// inside the root and against the root otherwise.
func (l *locator) resolve(argument string) (string, error) {
	var relative string
	if filepath.IsAbs(argument) {
		// Absolute arguments are often derived from the working directory,
		// which is a physical path, but the root may not be.
		local, err := filepath.Rel(l.root, argument)
		if resolved, resolveErr := filepath.EvalSymlinks(l.root); (err != nil || escapes(local)) && resolveErr == nil {
			local, err = filepath.Rel(resolved, argument)
		}
		if err != nil {
			return "", fmt.Errorf("unable to locate %s relative to the synchronization root: %w", argument, err)
		}
		relative = filepath.ToSlash(local)
	} else if l.contained {
		relative = path.Join(l.base, filepath.ToSlash(argument))
	} else {
		relative = path.Clean(filepath.ToSlash(argument))
	}
	if escapes(relative) {
		return "", errors.New(argument + " is outside the synchronization root")
	} else if relative == "." {
		return "", nil
	}
	return relative, nil
}

// escapes determines whether a relative path refers to a location outside the
// directory that it's relative to.
func escapes(relative string) bool {
	relative = filepath.ToSlash(relative)
	return relative == ".." || strings.HasPrefix(relative, "../")
}

// reporter prints synchronization reports as warnings from the submitting
// endpoint's perspective, omitting lines that it has already printed.
type reporter struct {
	// Node names replace local/remote labels for tree edge reports.
	localNode, remoteNode string
	// output is the warning output.
	output io.Writer
	// locator maps paths for display.
	locator *locator
	// alpha indicates whether the submitting endpoint is alpha.
	alpha bool
	// printed records printed lines.
	printed map[string]bool
}

// newReporter creates a reporter for the target of a submission.
func newReporter(output io.Writer, target *remote.Target) *reporter {
	reporter := &reporter{output: output, locator: newLocator(target), alpha: target.Alpha, printed: make(map[string]bool)}
	if target.Tree {
		reporter.localNode, reporter.remoteNode = target.SourceNode, target.Node
	}
	return reporter
}

// print prints a report.
func (r *reporter) print(report *remote.Report) {
	if report.Empty() {
		return
	}
	var conflicts []string
	for _, conflict := range report.Conflicts {
		conflicts = append(conflicts, r.describeConflict(conflict))
	}
	if report.ExcludedConflicts > 0 {
		conflicts = append(conflicts, fmt.Sprintf("...and %d more conflicts", report.ExcludedConflicts))
	}
	// Only sessions that support resolution refer to it.
	heading := "these conflicts aren't synchronized until one side's copy is deleted (see remote --diff):"
	if report.Resolution {
		heading = "these conflicts aren't synchronized until resolved (see remote --diff and remote --help conflicts):"
	}
	r.section(heading, conflicts)
	var problems []string
	for _, problem := range report.Problems {
		side := "remote"
		if problem.Alpha == r.alpha {
			side = "local"
		}
		if r.localNode != "" {
			side = r.remoteNode
			if problem.Alpha == r.alpha {
				side = r.localNode
			}
		}
		problems = append(problems, fmt.Sprintf("%s (%s): %s",
			r.locator.display(problem.Path), side, terminal.NeutralizeControlCharacters(problem.Error)))
	}
	if report.ExcludedProblems > 0 {
		problems = append(problems, fmt.Sprintf("...and %d more problems", report.ExcludedProblems))
	}
	r.section("this content couldn't be synchronized:", problems)
}

// section prints a warning followed by the lines that haven't been printed.
func (r *reporter) section(warning string, lines []string) {
	var unprinted []string
	for _, line := range lines {
		if !r.printed[line] {
			r.printed[line] = true
			unprinted = append(unprinted, line)
		}
	}
	if len(unprinted) == 0 {
		return
	}
	fmt.Fprintln(r.output, "remote: warning:", warning)
	for _, line := range unprinted {
		fmt.Fprintln(r.output, "remote:  ", line)
	}
}

// sides returns a conflict's local and remote changes.
func (r *reporter) sides(conflict remote.Conflict) ([]remote.Change, []remote.Change) {
	if r.alpha {
		return conflict.Alpha, conflict.Beta
	}
	return conflict.Beta, conflict.Alpha
}

// describeConflict describes a conflict on one line, such as "out: deleted
// directory locally, modified file out/log remotely".
func (r *reporter) describeConflict(conflict remote.Conflict) string {
	local, opposite := r.sides(conflict)
	var changes []string
	localName, remoteName := "locally", "remotely"
	if r.localNode != "" {
		localName = "on node " + terminal.NeutralizeControlCharacters(r.localNode)
		remoteName = "on node " + terminal.NeutralizeControlCharacters(r.remoteNode)
	}
	for _, side := range []struct {
		changes []remote.Change
		name    string
	}{{local, localName}, {opposite, remoteName}} {
		for _, change := range side.changes {
			description := describeChange(change)
			if change.Path != conflict.Root {
				description += " " + r.locator.display(change.Path)
			}
			changes = append(changes, description+" "+side.name)
		}
	}
	if conflict.ExcludedChanges > 0 {
		changes = append(changes, fmt.Sprintf("...and %d more changes (specify paths with remote --diff to compare them)", conflict.ExcludedChanges))
	}
	return r.locator.display(conflict.Root) + ": " + strings.Join(changes, ", ")
}

// describeChange describes a change, such as "modified file".
func describeChange(change remote.Change) string {
	switch {
	case change.Old == "":
		return "created " + change.New
	case change.New == "":
		return "deleted " + change.Old
	case change.Old == change.New:
		return "modified " + change.New
	default:
		return "changed " + change.Old + " to " + change.New
	}
}

// conflictPaths returns the distinct paths of a report's conflicting changes,
// in conflict order.
func conflictPaths(report *remote.Report) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, conflict := range report.Conflicts {
		// A truncated list may omit the root change, but comparing the root
		// still shows file differences or a directory replaced by a file.
		if conflict.ExcludedChanges > 0 && !seen[conflict.Root] {
			seen[conflict.Root] = true
			paths = append(paths, conflict.Root)
		}
		for _, changes := range [][]remote.Change{conflict.Alpha, conflict.Beta} {
			for _, change := range changes {
				if !seen[change.Path] {
					seen[change.Path] = true
					paths = append(paths, change.Path)
				}
			}
		}
	}
	return paths
}
