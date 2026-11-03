package local

import (
	"bytes"
	"context"
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
)

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
// concurrent usage. Stalls and failures are tracked here rather than in the
// core package because they have to outlive individual calls to
// core.Transition, and so does the fetching of commits in the background.
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
	// fetcher fetches commits in the background.
	fetcher *gitFetcher
}

// newGitProvider creates a new provider. Its fetcher has to be started by the
// caller (see gitFetcher.run).
func newGitProvider(logger *logging.Logger, signal func()) *gitProvider {
	provider := &gitProvider{
		stallTimeout: gitDefaultStallTimeout,
		stallBackoff: gitDefaultStallBackoff,
		logger:       logger,
		backoffs:     make(map[string]time.Time),
	}
	provider.fetcher = newGitFetcher(provider, signal)
	return provider
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

// gitFetchOptions are the options of the fetches that fetchCommit performs,
// which make sure that they add nothing but objects to the repository, whatever
// its configuration says.
var gitFetchOptions = []string{
	"--no-write-fetch-head",
	"--no-tags",
	"--no-prune",
	"--recurse-submodules=no",
	"--progress",
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
	// Try to fetch only the commit that we need. The options override any
	// configuration that would fetch or prune tags, or fetch into nested
	// submodules, none of which is wanted.
	err := g.runNetwork(ctx, directory, url, slices.Concat([]string{"fetch"}, gitFetchOptions, []string{"--", remote, commit})...)
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
	// so see if we can get it that way. If we're fetching from a remote, then
	// that's what its configured refspecs name, but they'd also update its
	// remote-tracking references (and prune them, if that's configured), so
	// this is a dry run, which downloads the objects without updating any
	// references. If we're fetching from a URL rather than a remote, then
	// there's no configuration to indicate what to fetch, and Git will only
	// fetch the repository's default branch.
	err = g.runNetwork(ctx, directory, url, slices.Concat([]string{"fetch", "--dry-run"}, gitFetchOptions, []string{"--", remote})...)
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

// Probe implements core.GitProvider.Probe.
func (g *gitProvider) Probe(ctx context.Context, directory, _ string) (*core.Entry, error) {
	// Examine the repository.
	repository, err := git.Probe(ctx, directory)
	if err != nil {
		return nil, err
	} else if repository == nil {
		return nil, nil
	}

	// Success.
	return &core.Entry{
		Kind:     core.EntryKind_GitLink,
		Digest:   repository.Commit,
		Url:      repository.URL,
		AutoCRLF: repository.AutoCRLF,
		Eol:      repository.EOL,
	}, nil
}

// errGitUnsupported is the error of the operations that populate submodules,
// attach repositories to them, and move their HEAD, which the provider doesn't
// support yet.
var errGitUnsupported = errors.New("submodules aren't supported by this endpoint yet")

// Clone implements core.GitProvider.Clone.
func (g *gitProvider) Clone(context.Context, string, *core.Entry) (*core.Entry, []string, error) {
	return nil, nil, errGitUnsupported
}

// ScanClone implements core.GitProvider.ScanClone.
func (g *gitProvider) ScanClone(context.Context, string, string) (*core.Entry, error) {
	return nil, errGitUnsupported
}

// RecordSeed implements core.GitProvider.RecordSeed.
func (g *gitProvider) RecordSeed(context.Context, string, *core.Entry) error {
	return errGitUnsupported
}

// PrepareAttach implements core.GitProvider.PrepareAttach.
func (g *gitProvider) PrepareAttach(context.Context, string, *core.Entry) (*core.Entry, []string, error) {
	return nil, nil, errGitUnsupported
}

// Refresh implements core.GitProvider.Refresh.
func (g *gitProvider) Refresh(context.Context, string) error {
	return errGitUnsupported
}

// Follow implements core.GitProvider.Follow.
func (g *gitProvider) Follow(context.Context, string, *core.Entry, *core.Entry) error {
	return errGitUnsupported
}

// containsGitRulePath determines whether or not any of the specified re-check
// paths identifies a file whose contents affect ignore evaluation in Git mode.
// This includes the exclude files of the root repository and of nested
// repositories, for which gitModeEventRelevant lets events through, and which
// aren't synchronized, but which local edits can change. An exclude file
// applies throughout its repository's working tree, so a change to one
// requires a full scan, like a change to a rule file.
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
// arbitrary names and can be numerous.
//
// The contents of Git directories are never synchronized and most of their
// contents change constantly, even as a result of commands that fail or don't
// change anything. If those changes were to wake the session, then anything
// that the session itself runs (or that runs in response to its activity) in
// the repository could keep it awake indefinitely. Thus, only events for
// entries that indicate a change of the current commit (a repository's HEAD,
// its references, its packed references, and, for repositories that store
// references in reftables, the list of tables, which is the only file that
// changes when any reference does), of ignore rules (its info/exclude file), or
// of what makes a nested repository a submodule (its config file, which holds
// its origin and line ending settings) are relevant. Nothing that the session
// runs in a repository writes its config file. The Git directories of
// submodules are nested within that of the enclosing repository (e.g.
// .git/modules/x/y/HEAD), or are found at the submodule's path, and the same
// applies to them.
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
			// Git refuses reference names that end in .lock, so such a file can
			// only be the lock file for a reference, which says nothing by
			// itself: Git takes the one for refs/remotes/<remote>/HEAD in every
			// fetch of a remote, even if nothing changes. An update is reported
			// for the reference itself when the lock file is renamed.
			return !strings.HasSuffix(components[len(components)-1], ".lock")
		}
	}

	// Otherwise, only a few specific files are relevant. Their lock files have
	// different names, and an update is reported for the file itself when its
	// lock file is renamed.
	last := len(components) - 1
	switch components[last] {
	case "HEAD", "packed-refs", "config":
		return true
	case "exclude":
		return last > 0 && components[last-1] == "info"
	case "tables.list":
		return last > 0 && components[last-1] == "reftable"
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
	return warnings
}

// trackedIgnoredWarnings computes warnings about files that are tracked by the
// repository at the root (if any) or by a submodule, but matched by ignore
// rules, and which thus aren't synchronized, even though they're probably
// assumed to be. There's one warning for each such file in the snapshot or, if
// a file is in an ignored directory, for the directory, up to a maximum, with
// any remainder being covered by a single summary warning.
func (e *endpoint) trackedIgnoredWarnings(ctx context.Context, snapshot *core.Snapshot) []*core.Problem {
	// Identify the repositories to check: the one at the root, if there is
	// one, and those of submodules, whose rules are their own (see
	// gitignore.NewIgnorer). We don't need to involve Git to determine that a
	// root doesn't have a repository, which is the common case for an endpoint
	// that only receives content. Note that Git won't discover a repository
	// above the root (see git.Command).
	if snapshot.Content == nil || snapshot.Content.Kind != core.EntryKind_Directory {
		return nil
	}
	var repositories []string
	if _, err := os.Lstat(filepath.Join(e.root, ".git")); err == nil {
		repositories = append(repositories, "")
	}
	for path := range snapshot.Content.Submodules() {
		repositories = append(repositories, path)
	}
	if len(repositories) == 0 {
		return nil
	}

	// Bound the duration of the commands, for all repositories together.
	ctx, cancel := context.WithTimeout(ctx, gitWarningTimeout)
	defer cancel()

	// List the tracked files that match ignore rules, as root-relative paths.
	var paths []string
	for _, repository := range repositories {
		repositoryPaths, err := e.listTrackedIgnored(ctx, repository)
		if err != nil {
			e.logger.Debug("Unable to list files tracked by Git but matching ignore rules:", err)
			continue
		}
		for _, path := range repositoryPaths {
			if repository != "" {
				path = repository + "/" + path
			}
			paths = append(paths, path)
		}
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

// listTrackedIgnored lists the files that are tracked by the repository at the
// specified root-relative path (the root or a submodule) but that match its
// .gitignore files or its exclude file, relative to the repository. Like the
// ignorer used for scanning, it doesn't consider global configuration.
func (e *endpoint) listTrackedIgnored(ctx context.Context, repository string) ([]string, error) {
	// Compute the repository's directory.
	directory := filepath.Join(e.root, filepath.FromSlash(repository))

	// Locate the exclude file, which isn't necessarily in the root (e.g. if the
	// root is a linked worktree). Git treats the file as relative to the
	// working directory, and it refuses to list files if the file doesn't
	// exist.
	arguments := []string{"ls-files", "-z", "-c", "-i", "--exclude-per-directory=.gitignore"}
	output, err := git.Command(ctx, directory, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return nil, fmt.Errorf("unable to locate exclude file: %w", err)
	}
	excludePath := strings.TrimRight(string(output), "\r\n")
	excludeFile := excludePath
	if !filepath.IsAbs(excludeFile) {
		excludeFile = filepath.Join(directory, excludeFile)
	}
	if _, err := os.Stat(excludeFile); err == nil {
		arguments = append(arguments, "--exclude-from="+excludePath)
	}

	// List and split paths. Paths aren't quoted because we've requested
	// null-terminated output.
	output, err = git.Command(ctx, directory, arguments...).Output()
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
