package local

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
	"github.com/mutagen-io/mutagen/pkg/git"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
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

	// gitSeedDirectoryName is the name of the directory within a clone's Git
	// directory that holds the records of the populations that the endpoint
	// has performed with it (see RecordSeed).
	gitSeedDirectoryName = "mutagen"
	// gitSeedFilePrefix is the prefix of the names of those records.
	gitSeedFilePrefix = "seed-"
)

// gitCheckoutOptions are the options for the checkout that populates a clone.
// By default, Git overwrites ignored files if the target commit tracks files at
// their paths, which can't happen in a new clone, but which is disabled anyway.
// Given the submodule.recurse setting, Git would also populate nested
// submodules as part of the checkout, which is the session's job, since it
// does so in the same way as for any other submodule.
var gitCheckoutOptions = []string{"--detach", "--no-overwrite-ignore", "--no-recurse-submodules"}

// gitFilterOverride is the content of the attributes file that a clone's
// checkout falls back to (see Clone), which unsets the filter attribute for
// every path. The attributes in a repository's info/attributes file take
// precedence over those in its .gitattributes files.
const gitFilterOverride = "* -filter\n"

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
	// root is the synchronization root.
	root string
	// session is the identifier of the session.
	session string
	// sessionIgnorer is the session's ignorer (see endpoint.ignorer).
	sessionIgnorer ignore.Ignorer
	// hasherFactory creates hashers for scanning clones.
	hasherFactory func() hash.Hash
	// probeMode, symbolicLinkMode, and permissionsMode are the endpoint's
	// settings for scanning clones.
	probeMode        behavior.ProbeMode
	symbolicLinkMode core.SymbolicLinkMode
	permissionsMode  core.PermissionsMode
	// backoffsLock serializes access to backoffs.
	backoffsLock sync.Mutex
	// backoffs maps URLs that have stalled to the time until which they won't
	// be contacted.
	backoffs map[string]time.Time
	// cloneRetries throttles the clones that populate submodules and that
	// prepare repositories for attachment.
	cloneRetries *gitRetries
	// fetcher fetches commits in the background.
	fetcher *gitFetcher
}

// newGitProvider creates a new provider. Its fetcher has to be started by the
// caller (see gitFetcher.run).
func newGitProvider(logger *logging.Logger, root, session string, sessionIgnorer ignore.Ignorer, hasherFactory func() hash.Hash, probeMode behavior.ProbeMode, symbolicLinkMode core.SymbolicLinkMode, permissionsMode core.PermissionsMode, signal func()) *gitProvider {
	provider := &gitProvider{
		stallTimeout:     gitDefaultStallTimeout,
		stallBackoff:     gitDefaultStallBackoff,
		logger:           logger,
		root:             root,
		session:          session,
		sessionIgnorer:   sessionIgnorer,
		hasherFactory:    hasherFactory,
		probeMode:        probeMode,
		symbolicLinkMode: symbolicLinkMode,
		permissionsMode:  permissionsMode,
		backoffs:         make(map[string]time.Time),
		cloneRetries:     newGitRetries(),
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
	command := git.NetworkCommand(commandCtx, directory, args...)
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

// gitFetchConfiguration is the configuration that the fetches that fetchCommit
// performs run with. Since Git 2.48, a fetch with a remote's configured
// refspecs creates the remote's HEAD reference if it's missing, which this
// disables. Older versions of Git don't know the setting and ignore it.
var gitFetchConfiguration = []string{"-c", "remote.origin.followRemoteHEAD=never"}

// gitFetchOptions are the options of the fetches that fetchCommit performs,
// which, along with gitFetchConfiguration, make sure that they add nothing but
// objects to the repository, whatever its configuration says.
var gitFetchOptions = []string{
	"--no-write-fetch-head",
	"--no-tags",
	"--no-prune",
	"--recurse-submodules=no",
	"--progress",
}

// hasCommit determines whether or not the repository in a directory has a
// commit, specified by its hexadecimal identifier. A partial clone doesn't
// have a commit that it would have to fetch from its promisor remote, because
// commands that don't use the network can't fetch it (see git.Command), so
// that it's fetched by fetchMissing instead, as it would be in any other
// repository.
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
	err := g.runNetwork(ctx, directory, url, slices.Concat(gitFetchConfiguration, []string{"fetch"}, gitFetchOptions, []string{"--", remote, commit})...)
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
	err = g.runNetwork(ctx, directory, url, slices.Concat(gitFetchConfiguration, []string{"fetch", "--dry-run"}, gitFetchOptions, []string{"--", remote})...)
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

// head returns the hexadecimal identifier of the commit that the repository in
// a directory has checked out.
func (g *gitProvider) head(ctx context.Context, directory string) (string, error) {
	output, err := git.Run(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("unable to determine the commit that's checked out: %w", err)
	}
	commit, err := git.CommitFromOutput(output)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(commit), nil
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

	// Look for a record of having populated the submodule.
	seed, err := g.findSeed(directory)
	if err != nil {
		return nil, fmt.Errorf("unable to look for population record: %w", err)
	}

	// Success.
	return &core.Entry{
		Kind:     core.EntryKind_GitLink,
		Digest:   repository.Commit,
		Url:      repository.URL,
		Seed:     seed,
		AutoCRLF: repository.AutoCRLF,
		Eol:      repository.EOL,
	}, nil
}

// cloneOptions returns the options that make a clone use the line ending
// settings that a GitLink entry carries, so that a checkout has the same
// content as the other endpoint's. The values are validated by git.Probe on the
// other endpoint, but we check them again before handing them to Git.
func cloneOptions(target *core.Entry) []string {
	var options []string
	if git.UsableSetting(target.AutoCRLF) {
		options = append(options, "--config", "core.autocrlf="+target.AutoCRLF)
	}
	if git.UsableSetting(target.Eol) {
		options = append(options, "--config", "core.eol="+target.Eol)
	}
	return options
}

// cloneAndResolve clones the repository that a GitLink entry identifies into a
// directory, without checking anything out, determines the commit to use (the
// entry's commit if it's available, after fetching it if necessary, or
// otherwise the commit of the origin's default branch), and then calls prepare
// with that commit to make what the caller needs of the clone (such as a
// checkout). Failures of any of these steps are throttled per URL and commit
// (see gitRetries), since trying again means cloning again, so that a
// repository that can't be cloned, or whose commit can't be checked out, isn't
// contacted in every synchronization cycle. It returns the hexadecimal
// identifier of the commit and, if it isn't the entry's, a warning that says
// so.
func (g *gitProvider) cloneAndResolve(ctx context.Context, directory string, target *core.Entry, prepare func(commit string) error) (string, string, error) {
	// Verify that the URL can be used and convert the commit.
	url, err := git.UsableURL(target.Url)
	if err != nil {
		return "", "", fmt.Errorf("unable to clone submodule: %w", err)
	}
	commit := hex.EncodeToString(target.Digest)

	// If this has failed recently, then don't try again yet.
	key := gitRetryKey{url: url, commit: commit}
	if remaining, last := g.cloneRetries.wait(key); remaining > 0 {
		return "", "", fmt.Errorf("%w (it will be tried again in %s)", last, remaining.Round(time.Second))
	}

	// Perform the clone and the preparation, and record the outcome.
	commit, warning, err := g.cloneAndResolveOnce(ctx, directory, url, commit, target)
	if err == nil {
		err = prepare(commit)
	}
	if err != nil {
		if ctx.Err() == nil {
			g.cloneRetries.failed(key, err)
		}
		return "", "", err
	}
	g.cloneRetries.succeeded(key)
	return commit, warning, nil
}

// cloneAndResolveOnce implements cloneAndResolve without the throttling.
func (g *gitProvider) cloneAndResolveOnce(ctx context.Context, directory, url, commit string, target *core.Entry) (string, string, error) {
	// Clone the repository without checking anything out, since what the
	// default branch has checked out isn't what we're after. If we've been
	// given a URL with credentials, then we don't use them, though nothing that
	// we report would. The remote is named origin, whatever the user's
	// clone.defaultRemoteName says, since that's the name under which
	// everything else looks for it, including Probe, which wouldn't recognize
	// the result as a submodule otherwise.
	arguments := slices.Concat([]string{"clone", "--no-checkout", "--progress", "--origin", "origin"}, cloneOptions(target), []string{"--", url, "."})
	if err := g.runNetwork(ctx, directory, url, arguments...); err != nil {
		var stall *gitStallError
		if errors.As(err, &stall) || ctx.Err() != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("unable to clone %s: %w", url, err)
	}

	// The commit might not be reachable from any branch, in which case it may
	// still be possible to request it.
	if g.hasCommit(ctx, directory, commit) {
		return commit, "", nil
	}
	fetchErr := g.fetchCommit(ctx, directory, "origin", url, commit)
	if fetchErr == nil {
		return commit, "", nil
	} else if ctx.Err() != nil {
		return "", "", ctx.Err()
	}

	// The commit isn't available, which is expected if it hasn't been pushed,
	// so fall back to the commit of the origin's default branch, from which the
	// submodule can follow the commit later (see Follow).
	output, err := git.Run(ctx, directory, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("%w; the repository's default branch isn't available either", fetchErr)
	}
	fallback, err := git.CommitFromOutput(output)
	if err != nil {
		return "", "", err
	}
	warning := fmt.Sprintf(
		"cloned with the commit of the default branch (%s), since commit %s isn't available from %s; the submodule's HEAD follows once it is, and its files are synchronized meanwhile",
		gitShortCommit(hex.EncodeToString(fallback)), gitShortCommit(commit), url,
	)
	return hex.EncodeToString(fallback), warning, nil
}

// gitLinkFor creates the GitLink entry for a repository that's been cloned or
// prepared for a GitLink entry, with the commit that it actually has.
func gitLinkFor(target *core.Entry, commit string) (*core.Entry, error) {
	digest, err := hex.DecodeString(commit)
	if err != nil {
		return nil, fmt.Errorf("invalid commit identifier: %w", err)
	}
	return &core.Entry{
		Kind:     core.EntryKind_GitLink,
		Digest:   digest,
		Url:      target.Url,
		AutoCRLF: target.AutoCRLF,
		Eol:      target.Eol,
	}, nil
}

// Clone implements core.GitProvider.Clone.
func (g *gitProvider) Clone(ctx context.Context, directory string, target *core.Entry) (*core.Entry, []string, error) {
	// Clone the repository, determine the commit, and check it out.
	var checkoutWarning string
	commit, warning, err := g.cloneAndResolve(ctx, directory, target, func(commit string) error {
		var err error
		checkoutWarning, err = g.checkout(ctx, directory, commit)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	for _, w := range []string{warning, checkoutWarning} {
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	// Create the result.
	link, err := gitLinkFor(target, commit)
	if err != nil {
		return nil, nil, err
	}
	return link, warnings, nil
}

// checkout checks out a commit in a clone made without a checkout. If that's
// only possible without filters, then it returns a warning that says so.
func (g *gitProvider) checkout(ctx context.Context, directory, commit string) (string, error) {
	// Check out the commit. Progress has to be requested because the output
	// isn't a terminal, and checking out a large repository can take a while,
	// though nothing watches for stalls here. The checkout may use the network
	// through the filters that the repository's attributes ask for (such as
	// that of Git LFS).
	arguments := slices.Concat([]string{"checkout", "--progress"}, gitCheckoutOptions, []string{commit})
	_, err := git.NetworkRun(ctx, directory, arguments...)
	if err == nil {
		return "", nil
	} else if ctx.Err() != nil {
		return "", ctx.Err()
	}

	// A checkout can fail because a filter that the repository's
	// attributes ask for, and that this system's configuration marks as
	// required, fails, as Git LFS does if its server can't be reached. The
	// clone can't be used without a checkout, so we try again without
	// filters, which checks out what the repository stores (such as LFS
	// pointers), after which the other endpoint's content (such as the
	// files that the pointers stand for) propagates in the next cycle, as
	// it would if the filter weren't configured here at all. We use the
	// attributes file that takes precedence over all others, which doesn't
	// require knowing the names of the filters, and we remove it again
	// afterwards, so that the repository behaves as configured from then
	// on. The forced checkout discards whatever the failed one left behind.
	// If this fails too, then the original failure is the one to report.
	attributes := filepath.Join(directory, ".git", "info", "attributes")
	if mkdirErr := os.MkdirAll(filepath.Dir(attributes), 0700); mkdirErr != nil {
		return "", fmt.Errorf("unable to check out commit %s: %w", gitShortCommit(commit), err)
	} else if writeErr := os.WriteFile(attributes, []byte(gitFilterOverride), 0600); writeErr != nil {
		return "", fmt.Errorf("unable to check out commit %s: %w", gitShortCommit(commit), err)
	}
	forced := slices.Concat([]string{"checkout", "--progress", "--force"}, gitCheckoutOptions, []string{commit})
	_, retryErr := git.NetworkRun(ctx, directory, forced...)
	removeErr := os.Remove(attributes)
	if retryErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("unable to check out commit %s: %w", gitShortCommit(commit), err)
	} else if removeErr != nil {
		return "", fmt.Errorf("unable to restore attributes after checking out without filters: %w", removeErr)
	}
	return fmt.Sprintf(
		"checked out without filters, since checking out with them failed (%v); files that filters would produce (such as Git LFS files) are synchronized from the other endpoint instead",
		err,
	), nil
}

// gitPrefixedIgnorer is an ignore.Ignorer that evaluates the paths of a tree
// that's located elsewhere as if they were at a path within the
// synchronization root, using an ignorer for the synchronization root.
type gitPrefixedIgnorer struct {
	// prefix is the root-relative path at which the tree is evaluated.
	prefix string
	// ignorer is the ignorer for the synchronization root.
	ignorer ignore.Ignorer
}

// Ignore implements ignore.Ignorer.Ignore.
func (i *gitPrefixedIgnorer) Ignore(path string, directory bool) (ignore.IgnoreStatus, bool) {
	if path == "" {
		return i.ignorer.Ignore(i.prefix, directory)
	}
	return i.ignorer.Ignore(i.prefix+"/"+path, directory)
}

// ScanClone implements core.GitProvider.ScanClone.
func (g *gitProvider) ScanClone(ctx context.Context, directory, path string) (*core.Entry, error) {
	// Create the ignorer. The clone is the root of its own repository, so the
	// rule files above it don't apply to its contents, just as they don't once
	// it's in place (see gitignore.NewIgnorer). The session's own ignores
	// apply to it by its path.
	ignorer := gitignore.NewIgnorer(directory, &gitPrefixedIgnorer{prefix: path, ignorer: g.sessionIgnorer})

	// Perform the scan, with a hasher of its own, since this is called during a
	// transition, which doesn't hold the scan lock.
	snapshot, _, _, err := core.ScanGitMode(
		ctx,
		directory,
		nil, nil,
		g.hasherFactory(), nil,
		ignorer, nil,
		g.probeMode,
		g.symbolicLinkMode,
		g.permissionsMode,
		g,
	)
	if err != nil {
		return nil, err
	}
	return snapshot.Content, nil
}

// seedPrefix returns the prefix of the names of the files that record the
// populations that the endpoint has performed (see RecordSeed), which include
// the session's identifier, because sessions can share a directory (e.g. the
// edges of a star at its center), and each has its own ancestor.
func (g *gitProvider) seedPrefix() string {
	return gitSeedFilePrefix + g.session + "-"
}

// seedPath returns the path of the file that records a population of the
// submodule in a directory with the specified seed.
func (g *gitProvider) seedPath(directory, seed string) string {
	return filepath.Join(directory, core.GitLinkName, gitSeedDirectoryName, g.seedPrefix()+seed)
}

// findSeed looks for a record of a population of the submodule in a directory,
// returning its seed, or an empty string if there's none. A record can only be
// in a Git directory that's the directory's Git entry, since that's what Clone
// creates.
func (g *gitProvider) findSeed(directory string) (string, error) {
	if metadata, err := os.Lstat(filepath.Join(directory, core.GitLinkName)); err != nil || !metadata.IsDir() {
		return "", nil
	}
	// The error is wrapped, which os.IsNotExist doesn't look through. Most
	// submodules don't have the directory, since only clones that the endpoint
	// made do.
	names, err := filesystem.DirectoryContentsByPath(filepath.Join(directory, core.GitLinkName, gitSeedDirectoryName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", nil
		}
		return "", err
	}
	prefix := g.seedPrefix()
	for _, name := range names {
		if seed, ok := strings.CutPrefix(name.Name(), prefix); ok && seed != "" {
			return seed, nil
		}
	}
	return "", nil
}

// RecordSeed implements core.GitProvider.RecordSeed.
func (g *gitProvider) RecordSeed(_ context.Context, directory string, content *core.Entry) error {
	link := content.Contents[core.GitLinkName]
	if link == nil || link.Seed == "" {
		return errors.New("content doesn't have a seed")
	}
	path := g.seedPath(directory, link.Seed)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return encoding.MarshalAndSaveProtobuf(path, content)
}

// populations processes the records of populations that the scan that yielded
// the specified content found (see Probe). Those that the ancestor has
// absorbed, which carries the specified seeds, are discarded. The content of
// the others is returned, keyed by the paths of the submodules, for inclusion
// in the snapshot (see core.Snapshot.Seeds). A record that can't be used is
// reported as a warning, since the only consequence is that the submodule's
// content is reconciled without a merge base, which can result in conflicts.
// It fails if a record that's been absorbed can't be discarded, since the
// record would be used again if the ancestor were to lose its seed.
func (g *gitProvider) populations(content *core.Entry, seeds []string) (map[string]*core.Entry, []*core.Problem, error) {
	// Find the records, which is usually the end of it.
	var populations map[string]*core.Entry
	var warnings []*core.Problem
	for path, link := range content.Submodules() {
		if link.Seed == "" {
			continue
		}
		file := g.seedPath(filepath.Join(g.root, filepath.FromSlash(path)), link.Seed)

		// If the ancestor has absorbed the population, then discard its record.
		// The record may be gone already if the snapshot isn't fresh.
		if slices.Contains(seeds, link.Seed) {
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("unable to remove absorbed population record for %s: %w", path, err)
			}
			continue
		}

		// Otherwise, load the record and verify that it's the content of a
		// population with the same seed.
		record := &core.Entry{}
		if err := encoding.LoadAndUnmarshalProtobuf(file, record); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			warnings = append(warnings, &core.Problem{
				Path:    path,
				Error:   fmt.Sprintf("unable to read the record of this submodule's population, so its content is reconciled without one (%v); remove %s once synchronization is complete", err, file),
				Warning: true,
			})
			continue
		} else if err := record.EnsureValid(true); err != nil || !record.IsSubmodule() ||
			record.Contents[core.GitLinkName].Seed != link.Seed {
			warnings = append(warnings, &core.Problem{
				Path:    path,
				Error:   fmt.Sprintf("the record of this submodule's population is invalid, so its content is reconciled without one; remove %s once synchronization is complete", file),
				Warning: true,
			})
			continue
		}
		if populations == nil {
			populations = make(map[string]*core.Entry)
		}
		populations[path] = record
	}
	return populations, warnings, nil
}

// PrepareAttach implements core.GitProvider.PrepareAttach.
func (g *gitProvider) PrepareAttach(ctx context.Context, directory string, target *core.Entry) (*core.Entry, []string, error) {
	// Clone the repository, determine the commit to use, then detach HEAD at
	// the commit and make the index match it, without a working tree. Unlike a
	// checkout, neither of these can fail because of the files in the working
	// tree that the repository will have, and neither writes any, which is why
	// a checkout can't be used. If the session stops before the Git directory
	// is moved into place, then nothing's changed.
	commit, warning, err := g.cloneAndResolve(ctx, directory, target, func(commit string) error {
		if _, err := git.Run(ctx, directory, "update-ref", "--no-deref", "-m", "mutagen: attach", "HEAD", commit); err != nil {
			return fmt.Errorf("unable to set commit: %w", err)
		} else if _, err := git.Run(ctx, directory, "read-tree", commit); err != nil {
			return fmt.Errorf("unable to set index: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if warning != "" {
		warnings = []string{warning}
	}

	// Create the result.
	link, err := gitLinkFor(target, commit)
	if err != nil {
		return nil, nil, err
	}
	return link, warnings, nil
}

// Refresh implements core.GitProvider.Refresh.
func (g *gitProvider) Refresh(ctx context.Context, directory string) error {
	_, err := git.Run(ctx, directory, "reset", "-q", "--mixed", "--no-recurse-submodules")
	return err
}

// gitOperations maps the entries of a Git directory that record an operation
// in progress to descriptions of the operation for use in messages. The index's
// lock file records that a Git command is writing the index, which a commit
// does for as long as its message is being edited, and when it's done, it
// moves whatever HEAD refers to at that point.
var gitOperations = []struct {
	// path is the entry's path within the Git directory.
	path string
	// description describes the operation.
	description string
}{
	{"MERGE_HEAD", "a merge is in progress"},
	{"CHERRY_PICK_HEAD", "a cherry-pick is in progress"},
	{"REVERT_HEAD", "a revert is in progress"},
	{"rebase-merge", "a rebase is in progress"},
	{"rebase-apply", "a rebase is in progress"},
	{"index.lock", "another Git command is using the index (index.lock exists), such as a commit whose message is being edited"},
}

// gitRestoreTimeout is the maximum amount of time that the Git commands that
// reattach HEAD to its branch after a failed follow can take (see Follow).
// They run even if the follow was cancelled, since they undo what the follow
// did, and they only read and write a few small files.
const gitRestoreTimeout = 10 * time.Second

// gitOperationInProgress determines whether or not an operation whose state
// isn't just HEAD and the index is in progress in the repository in a directory,
// such as a merge or a rebase that has stopped, or a commit, or whether the
// index has conflicts, and if so, it returns a description for use in messages.
func gitOperationInProgress(ctx context.Context, directory string) (string, error) {
	// Locate the entries that record operations. Git reports paths relative to
	// the directory unless they're elsewhere.
	arguments := []string{"rev-parse"}
	for _, operation := range gitOperations {
		arguments = append(arguments, "--git-path", operation.path)
	}
	output, err := git.Run(ctx, directory, arguments...)
	if err != nil {
		return "", fmt.Errorf("unable to locate Git directory: %w", err)
	}
	paths := strings.Split(output, "\n")
	if len(paths) != len(gitOperations) {
		return "", fmt.Errorf("unexpected output from Git: %q", output)
	}
	for p, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(directory, filepath.FromSlash(path))
		}
		if _, err := os.Lstat(path); err == nil {
			return gitOperations[p].description, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("unable to check for operations in progress: %w", err)
		}
	}

	// Look for conflicts in the index.
	if output, err := git.Run(ctx, directory, "ls-files", "--unmerged"); err != nil {
		return "", fmt.Errorf("unable to check index for conflicts: %w", err)
	} else if output != "" {
		return "the index has conflicts", nil
	}
	return "", nil
}

// Follow implements core.GitProvider.Follow.
func (g *gitProvider) Follow(ctx context.Context, directory string, old, target *core.Entry) error {
	// Determine what's checked out. If that's already the target commit, then
	// there's nothing to do. If it's neither that nor the expected commit, then
	// something has happened since the scan, and the next cycle has to look at
	// it.
	oldCommit, targetCommit := hex.EncodeToString(old.Digest), hex.EncodeToString(target.Digest)
	head, err := g.head(ctx, directory)
	if err != nil {
		return fmt.Errorf("HEAD didn't follow commit %s: %w", gitShortCommit(targetCommit), err)
	} else if head == targetCommit {
		g.fetcher.cancel(directory)
		return nil
	} else if head != oldCommit {
		return fmt.Errorf("HEAD didn't follow commit %s, because it changed since the scan", gitShortCommit(targetCommit))
	}

	// If the commit isn't available, then have it fetched in the background,
	// which wakes the session if it succeeds, so that the next cycle can move
	// HEAD. Fetching here would hold up the synchronization of everything
	// else, because the next cycle can't start until this transition is
	// complete, and that would repeat in every cycle until the commit is
	// pushed, which can take arbitrarily long.
	if !g.hasCommit(ctx, directory, targetCommit) {
		status := g.fetcher.request(directory, target.Url, targetCommit)
		return fmt.Errorf("HEAD didn't follow commit %s, which isn't available on this endpoint yet (%s); the submodule's files are synchronized regardless", gitShortCommit(targetCommit), status)
	}
	g.fetcher.cancel(directory)

	// Don't follow while an operation is in progress whose state is in the
	// index and the Git directory, such as a merge with conflicts, since
	// moving HEAD and resetting the index would discard it, or while another
	// command is using the index, which would make moving the index fail, and
	// which may be a commit that would then move HEAD.
	if operation, err := gitOperationInProgress(ctx, directory); err != nil {
		return fmt.Errorf("HEAD didn't follow commit %s: %w", gitShortCommit(targetCommit), err)
	} else if operation != "" {
		return fmt.Errorf("HEAD didn't follow commit %s, because %s, which moving HEAD would disrupt; finish or abort it, and HEAD will follow", gitShortCommit(targetCommit), operation)
	}

	// Determine the branch that HEAD refers to, if any, so that HEAD can be
	// reattached to it if moving the index fails. Git answers with an exit
	// code of one if HEAD is detached.
	var branch string
	if output, err := git.Run(ctx, directory, "symbolic-ref", "--quiet", "HEAD"); err == nil && strings.HasPrefix(output, "refs/") {
		branch = output
	} else if err != nil && !git.ExitedWith(err, 1) {
		return fmt.Errorf("HEAD didn't follow commit %s: unable to determine the branch: %w", gitShortCommit(targetCommit), err)
	}

	// Detach HEAD where it is, which also verifies that it's still the
	// expected commit, and then move it and the index to the target commit.
	// Neither command writes to the working tree, unlike checkout, which can
	// remove files (e.g. in a sparse checkout, those outside of its patterns),
	// and neither moves the branch that was checked out. The index isn't
	// refreshed, because that runs filters, which can fail and would keep HEAD
	// from ever following; without refreshing, the paths that differ between
	// the commits merely lack cached file information. The reset writes the
	// index before it moves HEAD, so if it's interrupted, then HEAD is detached
	// at the old commit, which is consistent, and the next cycle tries again.
	//
	// If moving the index fails, such as because another command has started
	// to use the index since we checked, then HEAD is reattached to the branch
	// that it referred to. Otherwise, a commit that was in progress would be
	// made on the detached HEAD rather than on the branch, since a commit only
	// verifies that HEAD is still at the commit where it was. Nothing changes
	// for the commit if HEAD is reattached before the commit is made, and if
	// it's made in the mean time, then HEAD is left alone, as it is if anything
	// else has moved HEAD or the branch.
	if _, err := git.Run(ctx, directory, "update-ref", "--no-deref", "-m", "mutagen: follow", "HEAD", oldCommit, oldCommit); err != nil {
		return fmt.Errorf("HEAD didn't follow commit %s: %w", gitShortCommit(targetCommit), err)
	} else if err := resetWithoutRefresh(ctx, directory, targetCommit); err != nil {
		if ctx.Err() != nil || !replaceIndex(ctx, directory, oldCommit, targetCommit) {
			if branch != "" {
				if reattachErr := reattachHead(ctx, directory, branch, oldCommit); reattachErr != nil {
					return fmt.Errorf("HEAD didn't follow commit %s: %w; HEAD is left detached at %s, because it couldn't be reattached to %s (%v)",
						gitShortCommit(targetCommit), err, gitShortCommit(oldCommit), branch, reattachErr,
					)
				}
			}
			return fmt.Errorf("HEAD didn't follow commit %s: %w", gitShortCommit(targetCommit), err)
		}
	}

	// Success.
	return nil
}

// reattachHead points HEAD back at a branch after Follow has detached it at
// the commit that the branch is at, if moving the index has failed. HEAD is
// only reattached if it's still detached at that commit, since otherwise
// something else has moved it and the result isn't Follow's to undo, and if the
// branch is still at that commit, since otherwise reattaching would change
// what's checked out. The commands run even if the context has been cancelled,
// because the follow would otherwise leave HEAD detached. There's no way to
// make the change conditional on HEAD's state with the versions of Git that
// submodules support, so the state is checked first, which leaves a window of
// a few milliseconds, as opposed to the indefinite one of leaving HEAD detached.
func reattachHead(ctx context.Context, directory, branch, commit string) error {
	// Create a context that isn't cancelled along with the follow.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitRestoreTimeout)
	defer cancel()

	// Verify that HEAD is still detached at the commit. If it isn't, then
	// something else has changed it, and it's left alone.
	if _, err := git.Run(ctx, directory, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		return nil
	} else if !git.ExitedWith(err, 1) {
		return err
	}
	if output, err := git.Run(ctx, directory, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return err
	} else if commitBytes, err := git.CommitFromOutput(output); err != nil {
		return err
	} else if hex.EncodeToString(commitBytes) != commit {
		return nil
	}

	// Verify that the branch is still at the commit.
	if output, err := git.Run(ctx, directory, "rev-parse", "--verify", "--quiet", branch+"^{commit}"); err != nil {
		return fmt.Errorf("unable to determine the commit of the branch: %w", err)
	} else if commitBytes, err := git.CommitFromOutput(output); err != nil {
		return err
	} else if hex.EncodeToString(commitBytes) != commit {
		return errors.New("the branch has moved")
	}

	// Reattach HEAD.
	_, err := git.Run(ctx, directory, "symbolic-ref", "-m", "mutagen: follow failed", "HEAD", branch)
	return err
}

// replaceIndex moves HEAD and the index of the repository in a directory from
// one commit to another by replacing the index with the target commit's tree,
// rather than by a reset, which is what Follow falls back to if the reset fails.
// Even without refreshing the index, Git compares the contents of index entries
// that are racily clean (whose files were written within the same second as
// the index) whenever it writes the index, which runs the clean filters. That's
// the state of every index right after a checkout, such as that of a clone that
// populated a submodule, and if a required clean filter fails (e.g. one that's
// configured, but whose program is missing), then the reset fails until
// something else writes the index. Reading the tree instead doesn't compare
// anything, because the new entries don't have any cached file information. That
// makes Git examine every file the next time that it compares the working tree
// with the index, which is why it's only a fallback. The index is replaced
// before HEAD moves, which only happens if HEAD is still at the old commit, so
// if this is interrupted or HEAD changes, then the next cycle sees HEAD at the
// commit where it was and tries again. It returns whether or not it succeeded,
// leaving the reset's failure to be reported otherwise.
func replaceIndex(ctx context.Context, directory, oldCommit, targetCommit string) bool {
	if _, err := git.Run(ctx, directory, "read-tree", "--no-recurse-submodules", targetCommit); err != nil {
		return false
	} else if _, err := git.Run(ctx, directory, "update-ref", "--no-deref", "-m", "mutagen: follow", "HEAD", targetCommit, oldCommit); err != nil {
		return false
	}
	return true
}

// gitUsageExitCode is the exit code with which Git reports that it's been
// invoked incorrectly, such as with an option that it doesn't know.
const gitUsageExitCode = 129

// resetWithoutRefresh moves HEAD and the index of the repository in a
// directory to a commit, without touching the working tree and without
// refreshing the index (see Follow). Git 2.36 introduced --no-refresh, and from
// then on, only that option skips the refresh. Earlier versions, including
// those from 2.29 (the oldest that submodules support, see git.Probe) to 2.35,
// don't know the option, but -q skips the refresh by itself. So if Git rejects
// the command as invalid, then we try again without the option. We recognize
// that by the exit code, rather than by the message, because Git follows the
// message with its usage, and error messages only retain the last few lines of
// output.
func resetWithoutRefresh(ctx context.Context, directory, commit string) error {
	_, err := git.Run(ctx, directory, "reset", "-q", "--no-refresh", "--mixed", "--no-recurse-submodules", commit)
	if err != nil && ctx.Err() == nil && git.ExitedWith(err, gitUsageExitCode) {
		_, err = git.Run(ctx, directory, "reset", "-q", "--mixed", "--no-recurse-submodules", commit)
	}
	return err
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

// gitEntryEventDirectory determines whether or not the watch-root-relative,
// slash-separated path of a filesystem event identifies a Git entry itself (as
// "sub/.git" does), rather than something within one or anything else, and if
// it does, returns the path of the directory that contains the entry. Git
// entries are recognized as gitModeEventRelevant recognizes them, and those in
// temporary directories don't count.
func gitEntryEventDirectory(path string) (string, bool) {
	components := strings.Split(path, "/")
	for c, component := range components {
		if strings.HasPrefix(component, filesystem.TemporaryNamePrefix) {
			return "", false
		} else if strings.EqualFold(component, ".git") {
			if c != len(components)-1 {
				return "", false
			}
			return strings.Join(components[:c], "/"), true
		}
	}
	return "", false
}

// gitEntryChanged determines whether or not an event for the Git entry of a
// directory, specified by its root-relative path, is relevant in Git mode,
// which is the case if the Git entry changed what the directory contributes to
// the rules since the last scan (see gitignore.Repositories.ChangedAt), such as
// by making the directory the top level of a nested repository's working tree.
// Most events for a Git directory itself don't: recursive watchers can report
// one whenever an entry is created or removed in it, which happens continuously
// due to lock files, so they're dropped like the events for the lock files
// themselves. Events are also dropped for directories that the last scan
// ignored, or that are beneath one, whose contents aren't scanned, and before a
// scan has succeeded, since the next scan is a full one anyway. If scans
// aren't accelerated, then each one is a full one, and all of them are dropped,
// without waiting for the scan lock, which watching doesn't otherwise need
// then. Otherwise, it acquires the scan lock.
func (e *endpoint) gitEntryChanged(directory string) bool {
	if !e.accelerationAllowed {
		return false
	}
	e.lockScanLock(context.Background())
	defer e.unlockScanLock()
	if e.gitRepositories == nil || e.snapshot == nil || isIgnoredInSnapshot(e.snapshot.Content, directory) {
		return false
	}
	return e.gitRepositories.ChangedAt(directory)
}

// isIgnoredInSnapshot determines whether or not the content of a snapshot has an
// untracked entry at a root-relative path, or at one of the path's parents.
func isIgnoredInSnapshot(content *core.Entry, path string) bool {
	entry := content
	if path != "" {
		for _, component := range strings.Split(path, "/") {
			if entry == nil || entry.Kind == core.EntryKind_Untracked {
				break
			}
			entry = entry.Contents[component]
		}
	}
	return entry != nil && entry.Kind == core.EntryKind_Untracked
}

// isDirectoryInSnapshot determines whether or not the content of a snapshot has
// a directory at a root-relative path, whose contents a scan that uses the
// snapshot as its baseline may reuse.
func isDirectoryInSnapshot(content *core.Entry, path string) bool {
	entry := content
	if path != "" {
		for _, component := range strings.Split(path, "/") {
			if entry == nil || entry.Kind != core.EntryKind_Directory {
				return false
			}
			entry = entry.Contents[component]
		}
	}
	return entry != nil && entry.Kind == core.EntryKind_Directory
}

// gitModeEventRelevant determines whether or not a filesystem event from a
// recursive watcher should wake a session in Git mode. The path is specified in
// its watch-root-relative, slash-separated form. Events for Git entries
// themselves are handled separately (see gitEntryEventDirectory).
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
// its references, its packed references, and, for repositories that store
// references in reftables, the list of tables, which is the only file that
// changes when any reference does), of ignore rules (its info/exclude file), or
// of what makes a nested repository a submodule (its config file, which holds
// its origin and line ending settings) are relevant. Nothing that the session
// runs in a repository writes its config file. The Git directories of
// submodules are nested within that of the enclosing repository (e.g.
// .git/modules/x/y/HEAD), or are found at the submodule's path, and those of
// linked worktrees within that of their repository (e.g. .git/worktrees/w/HEAD),
// and the same applies to them. Where the path can't be attributed to a single
// Git directory with certainty, it's relevant if it might be relevant (see
// gitDirectoryEventRelevant), since a missed event is worse than a spurious one.
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

	// The Git directory itself isn't relevant by itself. Events are reported
	// for it when entries are created or removed within it, which happens
	// continuously due to lock files. The caller only lets through those that
	// change something (see gitEntryChanged).
	if len(components) == 0 {
		return false
	}

	// Classify the path within the Git directory.
	return gitDirectoryEventRelevant(components)
}

// gitDirectoryEventRelevant determines whether or not an event for a path in a
// Git directory, specified by its components relative to the Git directory,
// might be relevant (see gitModeEventRelevant).
//
// A Git directory has a few fixed entries, and those of its HEAD, references,
// and reference logs, and the Git directories of submodules can be nested
// within it, at modules/<name>, as can the administrative directories of
// linked worktrees, at worktrees/<name>, each of which has its own HEAD,
// references, and reference logs. Reference logs mirror the locations of what
// they record (e.g. logs/HEAD and logs/refs/heads/main), and they're updated
// along with what they record, so we don't lose anything by excluding them.
// The name of a worktree's directory is a single component, but that of a
// submodule can have several, each of which can be anything, including logs,
// refs, modules, or HEAD. So without examining the filesystem, a path within
// modules can't always be attributed to a single Git directory: for example,
// modules/a/logs/HEAD is the reference log of the HEAD of a submodule named a,
// which isn't relevant, or the HEAD of a submodule named a/logs, which is. Such
// a path is relevant if it's relevant for any of the submodule names that it
// may contain. That makes some reference logs and a few other entries of the
// Git directories of submodules with such names relevant, but they're usually
// written along with an entry that's relevant anyway.
//
// A path whose final component ends in .lock is never relevant, however it's
// attributed, since none of the relevant entries can have such a name: Git
// refuses reference names that end in .lock, so within refs, such a file can
// only be the lock file for a reference, which says nothing by itself. Git
// takes the one for refs/remotes/<remote>/HEAD in every fetch of a remote, even
// if nothing changes, so this keeps fetches from waking the session. When a
// lock file is renamed into place, an event is reported for the entry itself.
func gitDirectoryEventRelevant(components []string) bool {
	// Determine whether or not each suffix of the components is relevant as a
	// path within a Git directory, from the shortest to the longest, since a
	// path within modules continues with a path within the Git directory of a
	// submodule. relevant[c] is set for components[c:], and anyRelevant[c]
	// records whether or not that or any shorter suffix is relevant. The empty
	// suffix is a Git directory itself, which isn't relevant.
	count := len(components)
	relevant := make([]bool, count+1)
	anyRelevant := make([]bool, count+1)
	for c := count - 1; c >= 0; c-- {
		switch remaining := components[c:]; {
		case remaining[0] == "modules":
			// The name of a submodule follows, with at least one component,
			// followed by a path within its Git directory.
			relevant[c] = c+2 <= count && anyRelevant[c+2]
		case remaining[0] == "worktrees":
			// The single-component name of a linked worktree's directory
			// follows, followed by a path within that directory.
			relevant[c] = c+2 <= count && relevant[c+2]
		case remaining[0] == "logs":
			relevant[c] = false
		case remaining[0] == "refs":
			relevant[c] = !strings.HasSuffix(components[count-1], ".lock")
		case len(remaining) == 1:
			relevant[c] = remaining[0] == "HEAD" || remaining[0] == "packed-refs" || remaining[0] == "config"
		case len(remaining) == 2:
			relevant[c] = (remaining[0] == "info" && remaining[1] == "exclude") ||
				(remaining[0] == "reftable" && remaining[1] == "tables.list")
		}
		anyRelevant[c] = relevant[c] || anyRelevant[c+1]
	}
	return relevant[0]
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
