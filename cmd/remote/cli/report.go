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

// orient converts a pair of values between the order of alpha and beta and
// that of the local and remote endpoints, from the perspective of the
// submitting endpoint, which is alpha if alpha is true: it orders the values
// of alpha and beta as those of the local and remote endpoints, and vice
// versa. It and sideOf are the only places that convert between the two.
func orient[T any](alpha bool, first, second T) (T, T) {
	if alpha {
		return first, second
	}
	return second, first
}

// sideOf names the endpoint that alpha identifies, which is alpha if it's true
// and beta otherwise, from the perspective of the submitting endpoint, which
// is alpha if submitter is true: local or remote (see orient).
func sideOf(alpha, submitter bool) string {
	if alpha == submitter {
		return sideLocal
	}
	return sideRemote
}

// reporter prints synchronization reports as warnings from the submitting
// endpoint's perspective, omitting lines that it has already printed.
type reporter struct {
	// output is the warning output.
	output io.Writer
	// locator maps paths for display.
	locator *locator
	// alpha indicates whether the submitting endpoint is alpha.
	alpha bool
	// followUp starts the remote command lines that reports suggest (see
	// followUpCommand).
	followUp string
	// printed records printed lines.
	printed map[string]bool
}

// newReporter creates a reporter for the target of a submission, whose
// suggested remote command lines start with followUp (see followUpCommand).
func newReporter(output io.Writer, target *remote.Target, followUp string) *reporter {
	return &reporter{output: output, locator: newLocator(target), alpha: target.Alpha, followUp: followUp, printed: make(map[string]bool)}
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
	heading := fmt.Sprintf("these conflicts aren't synchronized until one side's copy is deleted (see %s --diff):", r.followUp)
	if report.Resolution {
		heading = fmt.Sprintf("these conflicts aren't synchronized until resolved (see %s --diff and remote --help conflicts):", r.followUp)
	}
	r.section(heading, conflicts)
	if conflict := report.Repository; conflict != nil {
		r.section(repositoryConflictHeading, r.describeRepositoryConflict(conflict))
	}
	var problems, byDesign []string
	for _, problem := range report.Problems {
		line := fmt.Sprintf("%s (%s): %s",
			r.locator.display(problem.Path), sideOf(problem.Alpha, r.alpha), terminal.NeutralizeControlCharacters(problem.Error))
		if problem.Warning {
			byDesign = append(byDesign, line)
		} else {
			problems = append(problems, line)
		}
	}
	// What's been omitted could be anything, so it doesn't count as by design.
	if report.ExcludedProblems > 0 {
		problems = append(problems, fmt.Sprintf("...and %d more problems", report.ExcludedProblems))
	}
	r.section("this content couldn't be synchronized:", problems)
	r.section("this content isn't synchronized, by design:", byDesign)
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
	return orient(r.alpha, conflict.Alpha, conflict.Beta)
}

// describeConflict describes a conflict on one line, such as "out: deleted
// directory locally, modified file out/log remotely".
func (r *reporter) describeConflict(conflict remote.Conflict) string {
	local, opposite := r.sides(conflict)
	var changes []string
	for _, side := range []struct {
		changes []remote.Change
		name    string
	}{{local, "locally"}, {opposite, "remotely"}} {
		for _, change := range side.changes {
			description := describeChange(change)
			if change.Path != conflict.Root {
				description += " " + r.locator.display(change.Path)
			}
			changes = append(changes, description+" "+side.name)
		}
	}
	if conflict.ExcludedChanges > 0 {
		changes = append(changes, fmt.Sprintf("...and %d more changes (specify paths with %s --diff to compare them)", conflict.ExcludedChanges, r.followUp))
	}
	return r.locator.display(conflict.Root) + ": " + strings.Join(changes, ", ")
}

// repositoryConflictHeading is the warning that introduces a repository
// conflict.
const repositoryConflictHeading = "the Git repositories conflict, so their commits and branches aren't synchronized until both sides have the same ones (files still are):"

// describeRepositoryConflict describes a repository conflict from the
// submitting endpoint's perspective on two lines: the repositories, such as
// "local main at 1a2b3c4d5e6f, remote main at 6f5e4d3c2b1a (they last agreed
// on main at 0123456789ab)", and how to resolve the conflict by giving one
// side the other's commit and branch without changing the files, which both
// sides still share, after which committing again keeps the changes of the
// side that gave up its commit. Only beta can give up its commit in one-way
// sessions, since alpha never takes beta's.
func (r *reporter) describeRepositoryConflict(conflict *remote.RepositoryConflict) []string {
	local, opposite := orient(r.alpha, conflict.Alpha, conflict.Beta)
	agreed := "they never agreed"
	if conflict.Ancestor != nil {
		agreed = "they last agreed on " + describeRepository(*conflict.Ancestor)
	}
	description := fmt.Sprintf("local %s, remote %s (%s)", describeRepository(local), describeRepository(opposite), agreed)
	here := strings.Join(resetCommands("", local, opposite), ", then ")
	remotely := strings.Join(resetCommands(r.followUp+" ", opposite, local), ", then ")
	var resolution string
	switch {
	case !conflict.OneWay:
		resolution = fmt.Sprintf("to resolve it, run %s here (which keeps the files) and commit again, or %s and commit again there", here, remotely)
	case r.alpha:
		resolution = fmt.Sprintf("to resolve it, run %s (which keeps the files there; alpha never takes beta's commits)", remotely)
	default:
		resolution = fmt.Sprintf("to resolve it, run %s here (which keeps the files; alpha never takes beta's commits)", here)
	}
	return []string{description, resolution}
}

// describeRepository describes a repository's commit and branch, such as "main
// at 1a2b3c4d5e6f".
func describeRepository(repository remote.Repository) string {
	if repository.Branch == "" {
		return "detached HEAD at " + shortRepositoryCommit(repository.Commit)
	}
	return neutralizeLine(repository.Branch) + " at " + shortRepositoryCommit(repository.Commit)
}

// shortRepositoryCommit abbreviates a hexadecimal commit identifier, with the
// same length as the identifiers of submodule commits in reports.
func shortRepositoryCommit(commit string) string {
	return neutralizeLine(commit[:min(len(commit), 12)])
}

// resetCommands returns the Git commands, starting with prefix, that give a
// repository another one's commit and branch without changing the files: a
// switch to the other's branch at the current commit, if the branches differ,
// followed by a reset to the other's commit.
func resetCommands(prefix string, from, to remote.Repository) []string {
	var commands []string
	if from.Branch != to.Branch && to.Branch == "" {
		commands = append(commands, prefix+"git switch --detach")
	} else if from.Branch != to.Branch {
		commands = append(commands, prefix+"git switch -C "+neutralizeLine(shellQuote(to.Branch)))
	}
	return append(commands, prefix+"git reset "+shortRepositoryCommit(to.Commit))
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
