package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
)

const (
	// gitDefaultStallTimeout is the amount of time that a command that uses the
	// network can run without producing any output before it's considered to
	// have stalled. There's no limit on the overall duration of such a command,
	// because a clone of a large repository can legitimately take a long time,
	// but it reports progress all the while.
	gitDefaultStallTimeout = 60 * time.Second

	// gitDefaultStallBackoff is the amount of time for which a URL won't be
	// contacted after a command that used it has stalled. A transition holds up
	// its session's synchronization cycle, and thus flushes, so without this,
	// each flush on a system that can't reach the repository would take at
	// least as long as the stall timeout.
	gitDefaultStallBackoff = 5 * time.Minute

	// gitShortCommitLength is the length of the abbreviated commit identifiers
	// that are used in messages.
	gitShortCommitLength = 12

	// gitWarningTimeout is the maximum amount of time that the Git commands of
	// each warning check can take. Warnings are only informational, so they
	// mustn't be able to hold up a synchronization cycle indefinitely (e.g. on
	// a filesystem that's stopped responding).
	gitWarningTimeout = 30 * time.Second

	// maximumTrackedIgnoredWarnings is the maximum number of paths for which
	// individual warnings will be generated about files that are tracked by Git
	// but matched by ignore rules. It only limits the size of snapshots, so
	// it's somewhat arbitrary, though it exceeds what session listings will
	// display.
	maximumTrackedIgnoredWarnings = 20

	// maximumDirtySubmoduleWarnings is the maximum number of submodules for
	// which individual warnings will be generated about uncommitted changes.
	// It's limited for the same reason as maximumTrackedIgnoredWarnings.
	maximumDirtySubmoduleWarnings = 20

	// maximumNestedFailures is the maximum number of failures to update nested
	// submodules that are described by a single error. It limits the size of
	// problems, since each description can include a kilobyte of Git's output,
	// and a system without network access can fail for every submodule.
	maximumNestedFailures = 5
)

// gitCheckoutOptions are the options for every checkout that synchronization
// performs in a submodule, including the ones that Git runs for nested
// submodules (see gitNestedUpdateStrategy). By default, Git overwrites ignored
// files if the target commit tracks files at their paths, which is why that has
// to be disabled. Given the submodule.recurse setting, it would also update
// nested submodules as part of the checkout, in a way that overwrites them.
var gitCheckoutOptions = []string{"--detach", "--no-overwrite-ignore", "--no-recurse-submodules"}

// gitNestedUpdateStrategy is the update strategy for nested submodules. There's
// no option or setting that makes Git's own checkout strategy, or the merge and
// rebase strategies that a repository can ask for in its .gitmodules file, leave
// ignored files alone. Git passes the commit as an additional argument, and the
// settings that Mutagen gives Git (such as those that disable hooks) reach the
// command through its environment. Progress has to be requested because the
// output isn't a terminal, and without it, a large checkout would look like a
// stall to runNetwork, which would stop it.
var gitNestedUpdateStrategy = "!git checkout --progress " + strings.Join(gitCheckoutOptions, " ")

// gitShortCommit returns the abbreviated form of a commit's hexadecimal
// identifier for use in messages.
func gitShortCommit(commit string) string {
	return commit[:min(len(commit), gitShortCommitLength)]
}

// gitStallError indicates that a command was stopped because it stalled, or
// that it wasn't started because of a recent stall. In either case, there's no
// point in trying anything else that uses the same URL.
type gitStallError struct {
	// message is the error message.
	message string
}

// Error implements error.Error.
func (e *gitStallError) Error() string {
	return e.message
}

// gitProvider implements core.GitProvider by running Git. It's safe for
// concurrent usage. Stalls are tracked here rather than in the core package
// because they have to outlive individual calls to core.Transition.
type gitProvider struct {
	// stallTimeout is the amount of time that a command that uses the network
	// can run without producing any output.
	stallTimeout time.Duration
	// stallBackoff is the amount of time for which a URL won't be contacted
	// after a command that used it has stalled.
	stallBackoff time.Duration
	// logger is the underlying logger.
	logger *logging.Logger
	// backoffsLock serializes access to backoffs.
	backoffsLock sync.Mutex
	// backoffs maps URLs that have stalled to the time until which they won't
	// be contacted.
	backoffs map[string]time.Time
}

// newGitProvider creates a new provider.
func newGitProvider(logger *logging.Logger) *gitProvider {
	return &gitProvider{
		stallTimeout: gitDefaultStallTimeout,
		stallBackoff: gitDefaultStallBackoff,
		logger:       logger,
		backoffs:     make(map[string]time.Time),
	}
}

// backoffRemaining returns the amount of time until a URL can be contacted
// again, which is zero or less if it can be contacted now.
func (g *gitProvider) backoffRemaining(url string) time.Duration {
	g.backoffsLock.Lock()
	defer g.backoffsLock.Unlock()
	until, ok := g.backoffs[url]
	if !ok {
		return 0
	}
	remaining := time.Until(until)
	if remaining <= 0 {
		delete(g.backoffs, url)
	}
	return remaining
}

// runNetwork runs a Git command that may contact the specified URL, stopping it
// if it stalls, which is the only failure that results in the URL not being
// contacted for a while. Other failures, such as a missing commit or a failure
// to authenticate, are worth retrying right away, because they don't cost much
// time and because the commit may be pushed or the credentials fixed at any
// moment. The command must be one that reports its progress.
func (g *gitProvider) runNetwork(ctx context.Context, directory, url string, args ...string) error {
	// If this URL has stalled recently, then don't contact it.
	if remaining := g.backoffRemaining(url); remaining > 0 {
		return &gitStallError{fmt.Sprintf(
			"not contacting %s, because it showed no progress recently; it will be tried again in %s",
			url, remaining.Round(time.Second),
		)}
	}

	// Start the command in a context that we can cancel if it stalls.
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := git.Command(commandCtx, directory, args...)
	output := git.NewOutputRecorder()
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		return &git.CommandError{Err: err}
	}

	// Watch for the command to stop producing output. We wait for this
	// Goroutine to exit before we look at its result.
	stalled := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(g.stallTimeout)
		defer timer.Stop()
		for {
			select {
			case <-done:
				stalled <- false
				return
			case <-timer.C:
				idle := output.Idle()
				if idle >= g.stallTimeout {
					stalled <- true
					cancel()
					return
				}
				timer.Reset(g.stallTimeout - idle)
			}
		}
	}()

	// Wait for the command, then for the Goroutine.
	err := command.Wait()
	close(done)
	didStall := <-stalled

	// If the command succeeded, then it doesn't matter what else happened. This
	// includes helper processes outliving the command and keeping its output
	// open.
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}

	// If the command stalled, then avoid this URL for a while.
	if didStall {
		g.backoffsLock.Lock()
		g.backoffs[url] = time.Now().Add(g.stallBackoff)
		g.backoffsLock.Unlock()
		g.logger.Debugf("Git command for %s stalled, avoiding it for %s", url, g.stallBackoff)
		return &gitStallError{fmt.Sprintf(
			"stopped because %s showed no progress for %s; it won't be tried again for %s",
			url, g.stallTimeout, g.stallBackoff,
		)}
	}

	// Otherwise, it's a normal failure.
	return &git.CommandError{Err: err, Output: output.Message()}
}

// hasCommit determines whether or not the repository in a directory has a
// commit, specified by its hexadecimal identifier.
func (g *gitProvider) hasCommit(ctx context.Context, directory, commit string) bool {
	_, err := git.Run(ctx, directory, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// fetchCommit fetches a commit, specified by its hexadecimal identifier, into
// the repository in a directory. The remote is what Git is told to fetch from,
// which is either the name of a remote or a URL, and the URL is what it refers
// to, which must not have credentials. Nothing but objects is added to the
// repository: no references change, and FETCH_HEAD isn't written.
func (g *gitProvider) fetchCommit(ctx context.Context, directory, remote, url, commit string) error {
	// Try to fetch only the commit that we need.
	err := g.runNetwork(ctx, directory, url, "fetch", "--no-write-fetch-head", "--progress", "--", remote, commit)
	if err == nil {
		return nil
	}

	// Stalls and cancellation aren't going to be any different the second time.
	var stall *gitStallError
	if errors.As(err, &stall) {
		return err
	} else if ctx.Err() != nil {
		return ctx.Err()
	}

	// Not all servers allow commits to be requested by their identifiers, in
	// which case the commit might still be reachable from what they do provide,
	// so see if we can get it that way. If we're fetching from a URL rather
	// than a remote, then there's no configuration to indicate what to fetch,
	// and Git will only fetch the repository's default branch.
	err = g.runNetwork(ctx, directory, url, "fetch", "--no-write-fetch-head", "--progress", "--", remote)
	if errors.As(err, &stall) {
		return err
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else if err != nil {
		return fmt.Errorf("unable to fetch commit %s from %s: %w", gitShortCommit(commit), url, err)
	}

	// The fetch has succeeded, so we can tell if the commit is available.
	if !g.hasCommit(ctx, directory, commit) {
		return fmt.Errorf("commit %s isn't available from %s; if it hasn't been pushed, push it, and the next synchronization will fetch it", gitShortCommit(commit), url)
	}
	return nil
}

// checkout checks out a commit, specified by its hexadecimal identifier, on a
// detached HEAD in the repository in a directory. It leaves any conflicting
// local changes for Git to refuse.
func (g *gitProvider) checkout(ctx context.Context, directory, commit string) error {
	arguments := slices.Concat([]string{"checkout"}, gitCheckoutOptions, []string{commit})
	if _, err := git.Run(ctx, directory, arguments...); err != nil {
		return fmt.Errorf("unable to check out commit %s: %w", gitShortCommit(commit), err)
	}
	return nil
}

// Probe implements core.GitProvider.Probe.
func (g *gitProvider) Probe(ctx context.Context, directory string) ([]byte, string, error) {
	return git.Probe(ctx, directory)
}

// Head implements core.GitProvider.Head.
func (g *gitProvider) Head(ctx context.Context, directory string) ([]byte, error) {
	output, err := git.Run(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	return git.CommitFromOutput(output)
}

// Clone implements core.GitProvider.Clone.
func (g *gitProvider) Clone(ctx context.Context, directory, url string, commit []byte) error {
	// Verify that the URL can be used and convert the commit.
	url, err := git.UsableURL(url)
	if err != nil {
		return fmt.Errorf("unable to clone submodule: %w", err)
	}
	identifier := hex.EncodeToString(commit)

	// Clone the repository without checking anything out, since what the
	// default branch has checked out isn't what we're after. If we've been
	// given a URL with credentials, then we don't use them, though nothing that
	// we report would.
	if err := g.runNetwork(ctx, directory, url, "clone", "--no-checkout", "--progress", "--", url, "."); err != nil {
		var stall *gitStallError
		if errors.As(err, &stall) || ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("unable to clone %s: %w", url, err)
	}

	// The commit might not be reachable from any branch, in which case it may
	// still be possible to request it.
	if !g.hasCommit(ctx, directory, identifier) {
		if err := g.fetchCommit(ctx, directory, "origin", url, identifier); err != nil {
			return err
		}
	}

	// Check out the commit.
	return g.checkout(ctx, directory, identifier)
}

// Update implements core.GitProvider.Update.
func (g *gitProvider) Update(ctx context.Context, directory, url string, commit []byte) error {
	// Fetch the commit if it's missing.
	identifier := hex.EncodeToString(commit)
	if !g.hasCommit(ctx, directory, identifier) {
		if err := g.fetchMissing(ctx, directory, url, identifier); err != nil {
			return err
		}
	}

	// Check out the commit.
	return g.checkout(ctx, directory, identifier)
}

// fetchMissing fetches a commit, specified by its hexadecimal identifier, into
// the repository in a directory, from its own origin and then from a URL, which
// is the one that the other endpoint has for the repository, since they may
// reach it in different ways. If both attempts are unsuccessful, then the error
// describes both.
func (g *gitProvider) fetchMissing(ctx context.Context, directory, url, commit string) error {
	var failures []string

	// Try the repository's own origin. It's okay if there's no longer an
	// origin, because we have another URL to try. We refer to the origin by its
	// name, which allows Git to use whatever credentials are in its URL, but we
	// only refer to it without them, and backoffs for it are shared with all of
	// its other uses.
	var originURL string
	if origin, err := git.Run(ctx, directory, "config", "--get", "remote.origin.url"); err == nil && origin != "" {
		originURL = git.StripCredentials(origin)
		err := g.fetchCommit(ctx, directory, "origin", originURL, commit)
		if err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		failures = append(failures, err.Error())
	}

	// Try the other endpoint's URL, unless it's the same one that just failed.
	usable, err := git.UsableURL(url)
	if err != nil {
		failures = append(failures, fmt.Sprintf("unable to use the other endpoint's URL: %v", err))
	} else if usable != originURL {
		err := g.fetchCommit(ctx, directory, usable, usable, commit)
		if err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		failures = append(failures, err.Error())
	}

	// Report the failures.
	return errors.New(strings.Join(failures, "; "))
}

// gitNestedSubmodule is a submodule recorded by a repository.
type gitNestedSubmodule struct {
	// path is the submodule's slash-separated path relative to the repository.
	// These come from the repository's index, because Git validates its paths,
	// unlike the ones in a .gitmodules file, which can lead anywhere.
	path string
	// commit is the hexadecimal identifier of the recorded commit.
	commit string
	// names are the names that the .gitmodules file gives the path, which is
	// usually just one, leaving out any for which updates are disabled. Without
	// any names at all, Git refuses to update the submodule.
	names []string
	// disabled indicates that the .gitmodules file names the path and that the
	// update strategy for each name is none, in which case Git leaves the
	// submodule alone.
	disabled bool
}

// gitSubmoduleSetting is a setting from a submodule section of a configuration.
type gitSubmoduleSetting struct {
	// name is the name of the submodule.
	name string
	// variable is the name of the setting.
	variable string
	// value is the value of the setting.
	value string
}

// readSubmoduleSettings reads the settings with names matching a pattern from
// the configuration of the repository in a directory, in order of precedence
// from lowest to highest, or those from the specified file instead if it isn't
// empty.
func readSubmoduleSettings(ctx context.Context, directory, file, pattern string) ([]gitSubmoduleSetting, error) {
	arguments := []string{"config", "-z"}
	if file != "" {
		arguments = append(arguments, "--file", file)
	}
	output, err := git.Run(ctx, directory, append(arguments, "--get-regexp", pattern)...)
	if git.ExitedWith(err, 1) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var settings []gitSubmoduleSetting
	for _, entry := range strings.Split(output, "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		name := strings.TrimPrefix(key, "submodule.")
		if dot := strings.LastIndexByte(name, '.'); dot != -1 {
			settings = append(settings, gitSubmoduleSetting{name[:dot], name[dot+1:], value})
		}
	}
	return settings, nil
}

// listNestedSubmodules lists the submodules that the repository in a directory
// records in its index. The directory's .gitmodules file must exist.
func listNestedSubmodules(ctx context.Context, directory string) ([]*gitNestedSubmodule, error) {
	// Read the settings that determine the names of submodules and whether or
	// not updates are disabled. As far as updates go, the repository's
	// configuration takes precedence over its .gitmodules file.
	declared, err := readSubmoduleSettings(ctx, directory, ".gitmodules", `^submodule\..*\.(path|update)$`)
	if err != nil {
		return nil, fmt.Errorf("unable to read .gitmodules: %w", err)
	}
	configured, err := readSubmoduleSettings(ctx, directory, "", `^submodule\..*\.update$`)
	if err != nil {
		return nil, fmt.Errorf("unable to read configuration: %w", err)
	}
	updates := make(map[string]string)
	for _, setting := range slices.Concat(declared, configured) {
		if setting.variable == "update" {
			updates[setting.name] = setting.value
		}
	}
	names := make(map[string][]string)
	for _, setting := range declared {
		if setting.variable == "path" {
			names[setting.value] = append(names[setting.value], setting.name)
		}
	}

	// Find the submodules, which have their own mode in the index. Paths aren't
	// quoted because we've requested null-terminated output. Each entry is
	// formatted as "mode object stage<TAB>path".
	output, err := git.Run(ctx, directory, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, fmt.Errorf("unable to list submodules: %w", err)
	}
	var submodules []*gitNestedSubmodule
	for _, entry := range strings.Split(output, "\x00") {
		metadata, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[0] != "160000" || fields[2] != "0" {
			continue
		}
		submodule := &gitNestedSubmodule{path: path, commit: fields[1]}
		for _, name := range names[path] {
			if updates[name] != "none" {
				submodule.names = append(submodule.names, name)
			}
		}
		submodule.disabled = len(names[path]) > 0 && len(submodule.names) == 0
		submodules = append(submodules, submodule)
	}
	return submodules, nil
}

// updateNestedSubmodule updates a single submodule nested in the repository in a
// directory, using gitNestedUpdateStrategy. Git still does everything else, such
// as initializing the submodule, cloning it to the location where Git expects to
// find it, and fetching what's missing. A strategy can only be specified by the
// name of a submodule, so a literal pathspec ensures that the command doesn't
// update anything else, which wouldn't have that strategy.
func (g *gitProvider) updateNestedSubmodule(ctx context.Context, directory, description string, submodule *gitNestedSubmodule) error {
	arguments := []string{"--literal-pathspecs"}
	for _, name := range submodule.names {
		// The first equals sign in a -c option ends the name of the setting, so
		// this would set something else, and Git would use another strategy.
		// There isn't a way to avoid that in all supported versions of Git.
		if strings.Contains(name, "=") {
			return fmt.Errorf("its name in .gitmodules (%q) contains an equals sign, so it can't be updated without risking ignored local files; rename it, or update it by hand", name)
		}
		arguments = append(arguments, "-c", "submodule."+name+".update="+gitNestedUpdateStrategy)
	}
	arguments = append(arguments, "submodule", "update", "--init", "--progress", "--", submodule.path)
	return g.runNetwork(ctx, directory, description, arguments...)
}

// updateNested updates the submodules nested within the repository in a
// directory, and those nested within them, adding a description of each failure
// to a list. Each one is updated by a separate command, because Git stops at the
// first failure of a strategy like gitNestedUpdateStrategy, and we do the
// recursion, because Git's would use a strategy that we can't specify in advance
// for submodules that it has yet to find. The location is the directory's path
// relative to the repository that the operation started with, which is empty for
// that repository. It returns false if there's no point in trying anything else,
// either because of a cancellation or because of a stall, after which the URLs
// of nested submodules are all being avoided.
func (g *gitProvider) updateNested(ctx context.Context, directory, location, description string, failures *[]string) bool {
	// Record a failure, along with where it happened. Git's output only
	// identifies a submodule relative to the repository in which it ran.
	fail := func(location string, err error) {
		if location != "" {
			*failures = append(*failures, location+": "+err.Error())
		} else {
			*failures = append(*failures, err.Error())
		}
	}

	// Without a .gitmodules file, there are no nested submodules. Git would
	// figure that out for itself, but there's no need to run it for what's the
	// most common case by far.
	if _, err := os.Lstat(filepath.Join(directory, ".gitmodules")); err != nil {
		if !os.IsNotExist(err) {
			fail(location, fmt.Errorf("unable to query .gitmodules: %w", err))
		}
		return true
	}

	// Find the submodules.
	submodules, err := listNestedSubmodules(ctx, directory)
	if err != nil {
		fail(location, err)
		return ctx.Err() == nil
	}

	// Update each of them and then whatever's nested in it, leaving out those
	// that are disabled, as Git would. Git also skips those that aren't active,
	// so we only go further if the expected commit is now checked out.
	for _, submodule := range submodules {
		if submodule.disabled {
			continue
		}
		path := submodule.path
		if location != "" {
			path = location + "/" + path
		}
		if err := g.updateNestedSubmodule(ctx, directory, description, submodule); err != nil {
			fail(path, err)
			var stall *gitStallError
			if errors.As(err, &stall) || ctx.Err() != nil {
				return false
			}
			continue
		}
		nested := filepath.Join(directory, filepath.FromSlash(submodule.path))
		head, err := git.Run(ctx, nested, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
		if err == nil && head == submodule.commit && !g.updateNested(ctx, nested, path, description, failures) {
			return false
		}
	}
	return true
}

// UpdateNested implements core.GitProvider.UpdateNested.
func (g *gitProvider) UpdateNested(ctx context.Context, directory, url string) error {
	// Update them. We don't know which URLs this will use, because they're
	// specified by the repository's content, so we track backoff for them as a
	// group. This has a different key than the repository's own URL, which has
	// shown that it works, and which we must not avoid as a result of this.
	description := "the submodules nested in " + git.StripCredentials(url)
	var failures []string
	g.updateNested(ctx, directory, "", description, &failures)
	if len(failures) == 0 {
		return nil
	}

	// Report the failures. Paths aren't necessarily valid UTF-8, but messages
	// have to be, since they're transmitted using Protocol Buffers.
	if omitted := len(failures) - maximumNestedFailures; omitted > 0 {
		failures = append(failures[:maximumNestedFailures], fmt.Sprintf("and %d more", omitted))
	}
	return errors.New(strings.ToValidUTF8(strings.Join(failures, "; "), "\uFFFD"))
}

// containsGitRulePath determines whether or not any of the specified re-check
// paths identifies a file whose contents affect ignore evaluation in Git mode.
// This includes the exclude file, for which gitModeEventRelevant lets events
// through, and which isn't synchronized, but which local edits can change.
func containsGitRulePath(recheckPaths map[string]bool) bool {
	for path := range recheckPaths {
		if gitignore.AffectsIgnoreEvaluation(path) {
			return true
		}
	}
	return false
}

// gitModeEventRelevant determines whether or not a filesystem event from a
// recursive watcher should wake a session in Git mode. The path is specified in
// its watch-root-relative, slash-separated form.
//
// The contents of temporary directories are never relevant, wherever they are
// in the hierarchy, because they aren't scanned. Their contents may have
// arbitrary names and can be numerous (e.g. those of a temporary clone).
//
// The contents of Git directories are never synchronized and most of their
// contents change constantly, even as a result of commands that fail or don't
// change anything. If those changes were to wake the session, then anything
// that the session itself runs (or that runs in response to its activity) in
// the repository could keep it awake indefinitely. Thus, only events for
// entries that indicate a change of the current commit (a repository's HEAD,
// its references, and its packed references) or of ignore rules (its
// info/exclude file) are relevant. The Git directories of submodules are nested
// within that of the enclosing repository (e.g. .git/modules/x/y/HEAD), or are
// found at the submodule's path, and the same applies to them.
func gitModeEventRelevant(path string) bool {
	// Split the path into components and reject any with a temporary name.
	components := strings.Split(path, "/")
	for _, component := range components {
		if strings.HasPrefix(component, filesystem.TemporaryNamePrefix) {
			return false
		}
	}

	// If the path isn't in a Git directory, then it's relevant.
	gitDirectory := slices.IndexFunc(components, func(component string) bool {
		return strings.EqualFold(component, ".git")
	})
	if gitDirectory == -1 {
		return true
	}
	components = components[gitDirectory+1:]

	// The Git directory itself isn't relevant. Events are reported for it when
	// entries are created or removed within it, which happens continuously due
	// to lock files.
	if len(components) == 0 {
		return false
	}

	// Look for the components that identify references and reference logs.
	// These have to be distinguished because reference logs mirror the
	// locations of what they record (e.g. logs/HEAD and logs/refs/heads/main),
	// and they are also updated in response to the same changes as what they
	// record, so we don't lose anything by excluding them. Whichever occurs
	// first determines the classification (e.g. a branch can be named
	// logs/refs). The component following one named modules is (at least the
	// first part of) the name of a submodule, which can be anything, including
	// refs or logs.
	for c := 0; c < len(components); c++ {
		switch components[c] {
		case "modules":
			c++
		case "logs":
			return false
		case "refs":
			return true
		}
	}

	// Otherwise, only a few specific files are relevant. We don't have to worry
	// about the lock files that are used to update them, because these have
	// different names, and the update is reported for the file when the lock
	// file is renamed.
	last := len(components) - 1
	switch components[last] {
	case "HEAD", "packed-refs":
		return true
	case "exclude":
		return last > 0 && components[last-1] == "info"
	}
	return false
}

// gitWarnings computes warnings about the content of a snapshot in Git mode,
// such as content that's missing from synchronization despite being part of a
// repository. It's called for each synchronization cycle. Warnings are
// advisory, so a check that can't run for any reason (e.g. because Git isn't
// installed or because the root isn't a repository) simply results in no
// warnings from that check rather than an error.
func (e *endpoint) gitWarnings(ctx context.Context, snapshot *core.Snapshot) []*core.Problem {
	var warnings []*core.Problem
	warnings = append(warnings, e.trackedIgnoredWarnings(ctx, snapshot)...)
	warnings = append(warnings, e.dirtySubmoduleWarnings(ctx, snapshot)...)
	return warnings
}

// dirtySubmoduleWarnings computes warnings about submodules with uncommitted
// changes, which aren't synchronized, because the other endpoint only gets the
// commit. There's one warning for each such submodule, up to a maximum, with
// any remainder being covered by a single summary warning.
//
// Checking the status of a repository would normally update its index, which is
// inside the directory that's being watched, but git.Command prevents that,
// because optional locks are what allow this kind of update.
func (e *endpoint) dirtySubmoduleWarnings(ctx context.Context, snapshot *core.Snapshot) []*core.Problem {
	// Find submodules, which is usually the end of it.
	paths := snapshot.Content.GitLinkPaths()
	if len(paths) == 0 {
		return nil
	}

	// Bound the duration of the commands.
	ctx, cancel := context.WithTimeout(ctx, gitWarningTimeout)
	defer cancel()

	// Check each submodule.
	var warnings []*core.Problem
	for _, path := range paths {
		status, err := git.Run(ctx, filepath.Join(e.root, filepath.FromSlash(path)), "status", "--porcelain")
		if err != nil {
			e.logger.Debug("Unable to determine the status of submodule:", err)
		} else if strings.TrimSpace(status) != "" {
			warnings = append(warnings, &core.Problem{
				Path:    path,
				Error:   "submodule has uncommitted changes, which aren't synchronized; commit and push them to synchronize them",
				Warning: true,
			})
		}
	}

	// Sort the warnings and limit them in the same way as for tracked files.
	core.SortProblems(warnings)
	if len(warnings) > maximumDirtySubmoduleWarnings+1 {
		omitted := len(warnings) - maximumDirtySubmoduleWarnings
		warnings = warnings[:maximumDirtySubmoduleWarnings+1]
		warnings[maximumDirtySubmoduleWarnings] = &core.Problem{
			Path: warnings[maximumDirtySubmoduleWarnings].Path,
			Error: fmt.Sprintf(
				"this and %d more submodules after it have uncommitted changes, which aren't synchronized; commit and push them to synchronize them",
				omitted-1,
			),
			Warning: true,
		}
	}
	return warnings
}

// trackedIgnoredWarnings computes warnings about files that are tracked by the
// repository at the root (if any), but matched by ignore rules, and which thus
// aren't synchronized, even though they're probably assumed to be. There's one
// warning for each such file in the snapshot or, if a file is in an ignored
// directory, for the directory, up to a maximum, with any remainder being
// covered by a single summary warning.
func (e *endpoint) trackedIgnoredWarnings(ctx context.Context, snapshot *core.Snapshot) []*core.Problem {
	// This check only applies to repositories at the root. We don't need to
	// involve Git to determine that a root doesn't have a repository, which is
	// the common case for an endpoint that only receives content. Note that Git
	// won't discover a repository above the root (see git.Command).
	if snapshot.Content == nil || snapshot.Content.Kind != core.EntryKind_Directory {
		return nil
	} else if _, err := os.Lstat(filepath.Join(e.root, ".git")); err != nil {
		return nil
	}

	// List the tracked files that match ignore rules.
	paths, err := e.listTrackedIgnored(ctx)
	if err != nil {
		e.logger.Debug("Unable to list files tracked by Git but matching ignore rules:", err)
		return nil
	}

	// Identify the shallowest untracked entry for each file, if there is one.
	// If the snapshot has the file itself, then the session's ignores must have
	// overridden the rules that match it, in which case there's nothing to
	// report. If the snapshot doesn't have an entry for the file or its parent
	// directories at all, then there's also nothing to report, since the file
	// isn't on disk (or is reported separately because of its name). Paths need
	// to be normalized in the same way as the snapshot's names to be found.
	// Git's paths are not necessarily valid UTF-8, but the paths that we find
	// in the snapshot are, and only those are used in warnings, which must be.
	var warnings []*core.Problem
	reported := make(map[string]bool)
	for _, path := range paths {
		if snapshot.DecomposesUnicode {
			path = norm.NFC.String(path)
		}
		untracked := shallowestUntracked(snapshot.Content, path)
		if untracked == "" || reported[untracked] {
			continue
		}
		reported[untracked] = true
		var message string
		if untracked == path {
			message = fmt.Sprintf(
				"tracked by Git but matched by .gitignore, so it isn't synchronized; add %q to ignore.paths to synchronize it",
				"!"+untracked,
			)
		} else {
			// Unignore rules can't reach beneath an ignored directory because
			// traversal doesn't continue into it, so the only option is to
			// synchronize the whole directory.
			message = fmt.Sprintf(
				"contains files tracked by Git but is matched by .gitignore, so they aren't synchronized; add %q to ignore.paths to synchronize the whole directory",
				"!"+untracked,
			)
		}
		warnings = append(warnings, &core.Problem{Path: untracked, Error: message, Warning: true})
	}

	// Sort the warnings so that they don't vary between synchronization cycles,
	// and limit them, using the path of the first one that's omitted for the
	// summary. The summary must have a real path, because tools match the paths
	// of problems against the paths that they're working on, and they treat the
	// empty path (which identifies the root) as affecting everything.
	core.SortProblems(warnings)
	if len(warnings) > maximumTrackedIgnoredWarnings+1 {
		omitted := len(warnings) - maximumTrackedIgnoredWarnings
		warnings = warnings[:maximumTrackedIgnoredWarnings+1]
		warnings[maximumTrackedIgnoredWarnings] = &core.Problem{
			Path: warnings[maximumTrackedIgnoredWarnings].Path,
			Error: fmt.Sprintf(
				"this and %d more paths after it are tracked by Git or contain files tracked by Git, but are matched by .gitignore, so they aren't synchronized",
				omitted-1,
			),
			Warning: true,
		}
	}
	return warnings
}

// listTrackedIgnored lists the root-relative paths of the files that are
// tracked by the repository at the root but that match its .gitignore files or
// its exclude file. Like the ignorer used for scanning, it doesn't consider
// global configuration.
func (e *endpoint) listTrackedIgnored(ctx context.Context) ([]string, error) {
	// Bound the duration of the commands.
	ctx, cancel := context.WithTimeout(ctx, gitWarningTimeout)
	defer cancel()

	// Locate the exclude file, which isn't necessarily in the root (e.g. if the
	// root is a linked worktree). Git treats the file as relative to the
	// working directory, and it refuses to list files if the file doesn't
	// exist.
	arguments := []string{"ls-files", "-z", "-c", "-i", "--exclude-per-directory=.gitignore"}
	output, err := git.Command(ctx, e.root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return nil, fmt.Errorf("unable to locate exclude file: %w", err)
	}
	excludePath := strings.TrimRight(string(output), "\r\n")
	excludeFile := excludePath
	if !filepath.IsAbs(excludeFile) {
		excludeFile = filepath.Join(e.root, excludeFile)
	}
	if _, err := os.Stat(excludeFile); err == nil {
		arguments = append(arguments, "--exclude-from="+excludePath)
	}

	// List and split paths. Paths aren't quoted because we've requested
	// null-terminated output.
	output, err = git.Command(ctx, e.root, arguments...).Output()
	if err != nil {
		return nil, fmt.Errorf("unable to list files: %w", err)
	}
	var paths []string
	for _, path := range bytes.Split(output, []byte{0}) {
		if len(path) > 0 {
			paths = append(paths, string(path))
		}
	}
	return paths, nil
}

// shallowestUntracked returns the path of the shallowest untracked entry at or
// above the specified root-relative path in an entry hierarchy, or an empty
// string if there's no such entry, either because the entry at the path isn't
// untracked or because the path can't be followed that far.
func shallowestUntracked(root *core.Entry, path string) string {
	entry := root
	for end := 0; end < len(path); {
		// Find the end of the next path component.
		start := end
		if slash := strings.IndexByte(path[start:], '/'); slash >= 0 {
			end = start + slash
		} else {
			end = len(path)
		}

		// Descend to the content with that name.
		entry = entry.Contents[path[start:end]]
		if entry == nil {
			return ""
		} else if entry.Kind == core.EntryKind_Untracked {
			return path[:end]
		}

		// Move past the separator.
		end++
	}
	return ""
}
