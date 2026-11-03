package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// gitMinimumRetryInterval is the time that has to pass after an operation
	// that uses the network for a submodule (such as fetching a commit) has
	// failed, before it's attempted again for the same URL and commit. It
	// doubles with each consecutive failure, up to gitMaximumRetryInterval.
	// Both are placeholders until they've been measured on real networks: they
	// determine how long after a commit is pushed a submodule's HEAD follows
	// it, and how much a commit that isn't pushed costs in network requests.
	gitMinimumRetryInterval = 10 * time.Second
	// gitMaximumRetryInterval is the maximum retry interval.
	gitMaximumRetryInterval = 5 * time.Minute
)

// gitRetryKey identifies the target of an operation that uses the network for
// a submodule: a commit, specified by its hexadecimal identifier, from a URL,
// without credentials.
type gitRetryKey struct {
	// url is the URL.
	url string
	// commit is the commit.
	commit string
}

// gitRetryState is the history of failures for a retry key.
type gitRetryState struct {
	// last is the time of the last failure.
	last time.Time
	// interval is the time after the last failure before the next attempt.
	interval time.Duration
	// err is the last failure.
	err error
}

// gitRetries throttles attempts at operations that use the network for
// submodules after they fail, per URL and commit. The throttling only depends
// on the history of the attempts themselves, and not on what else happens in
// the synchronization cycles that request them. It's safe for concurrent use.
type gitRetries struct {
	// lock serializes access to the fields below.
	lock sync.Mutex
	// minimumInterval is the retry interval after a first failure, which is
	// gitMinimumRetryInterval except in tests.
	minimumInterval time.Duration
	// maximumInterval is the maximum retry interval, which is
	// gitMaximumRetryInterval except in tests.
	maximumInterval time.Duration
	// states maps retry keys to their failure histories. Successes remove
	// their keys.
	states map[gitRetryKey]*gitRetryState
}

// newGitRetries creates a new retry throttle.
func newGitRetries() *gitRetries {
	return &gitRetries{
		minimumInterval: gitMinimumRetryInterval,
		maximumInterval: gitMaximumRetryInterval,
		states:          make(map[gitRetryKey]*gitRetryState),
	}
}

// wait returns the time until an operation can be attempted for a key, which
// is zero or less if it can be attempted now, along with the last failure, if
// there's been one.
func (r *gitRetries) wait(key gitRetryKey) (time.Duration, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	state, ok := r.states[key]
	if !ok {
		return 0, nil
	}
	return time.Until(state.last.Add(state.interval)), state.err
}

// failed records a failure for a key.
func (r *gitRetries) failed(key gitRetryKey, err error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if state, ok := r.states[key]; ok {
		state.last = time.Now()
		state.interval = min(2*state.interval, r.maximumInterval)
		state.err = err
	} else {
		r.states[key] = &gitRetryState{last: time.Now(), interval: r.minimumInterval, err: err}
	}
}

// succeeded records a success for a key.
func (r *gitRetries) succeeded(key gitRetryKey) {
	r.lock.Lock()
	defer r.lock.Unlock()
	delete(r.states, key)
}

// gitFetchRequest is a request to fetch a commit into a repository.
type gitFetchRequest struct {
	// directory is the directory of the repository.
	directory string
	// key identifies the commit and the URL that the other endpoint has for
	// the repository.
	key gitRetryKey
}

// gitFetcher fetches the commits that submodules have to follow in the
// background, so that synchronization never waits for the network (see
// gitProvider.Follow). A request is attempted as soon as the throttling allows
// (see gitRetries), and if the attempt fails, then the request is kept and
// attempted again once the throttling allows it, until it succeeds, until a
// newer request for the same repository replaces it, or until it's cancelled
// because the commit is no longer needed. The request has to be kept, because a
// commit that couldn't be fetched usually just hasn't been pushed yet, and
// pushing it changes nothing that a scan sees, so no synchronization cycle
// would request it again. The throttling limits the network traffic of an idle
// session to an attempt per maximum interval for each commit that a submodule
// has to follow. If a fetch succeeds, then the fetcher wakes the session, so
// that the next cycle moves HEAD. It's safe for concurrent use.
type gitFetcher struct {
	// provider is the provider whose operations the fetcher uses.
	provider *gitProvider
	// signal wakes the session.
	signal func()
	// retries throttles the fetches.
	retries *gitRetries
	// lock serializes access to the fields below.
	lock sync.Mutex
	// pending maps the directories of repositories to the requests that are
	// waiting to be attempted for them. Only the latest request for each
	// repository is kept, because a submodule only has to follow the latest
	// commit.
	pending map[string]gitFetchRequest
	// active is the request whose fetch is in progress, if any.
	active *gitFetchRequest
	// cancelled indicates that the request whose fetch is in progress was
	// cancelled, so that it's not attempted again if it fails.
	cancelled bool
	// wake is used to wake the fetcher's Goroutine when a request arrives.
	wake chan struct{}
}

// newGitFetcher creates a new fetcher for a provider. It doesn't fetch anything
// until run is called.
func newGitFetcher(provider *gitProvider, signal func()) *gitFetcher {
	return &gitFetcher{
		provider: provider,
		signal:   signal,
		retries:  newGitRetries(),
		pending:  make(map[string]gitFetchRequest),
		wake:     make(chan struct{}, 1),
	}
}

// request requests that a commit, specified by its hexadecimal identifier, be
// fetched into the repository in a directory, from the repository's origin or
// from the URL that the other endpoint has for it. It returns a description of
// the request's status for use in messages.
func (f *gitFetcher) request(directory, url, commit string) string {
	// Record the request.
	request := gitFetchRequest{directory: directory, key: gitRetryKey{url: url, commit: commit}}
	f.lock.Lock()
	inProgress := f.active != nil && *f.active == request
	if !inProgress {
		f.pending[directory] = request
	}
	f.lock.Unlock()

	// Wake the fetcher's Goroutine.
	select {
	case f.wake <- struct{}{}:
	default:
	}

	// Describe the status.
	if inProgress {
		return "it's being fetched"
	} else if remaining, last := f.retries.wait(request.key); last != nil {
		if remaining > 0 {
			return fmt.Sprintf("it couldn't be fetched (%v), and it will be fetched again in %s", last, remaining.Round(time.Second))
		}
		return fmt.Sprintf("it couldn't be fetched (%v), and it's being fetched again", last)
	}
	return "it's being fetched"
}

// cancel cancels any pending request for the repository in a directory, which
// is called once a submodule's HEAD no longer needs a commit to be fetched,
// such as when it's followed a commit that was already available. A request
// whose fetch is in progress isn't affected, but it's not attempted again.
func (f *gitFetcher) cancel(directory string) {
	f.lock.Lock()
	defer f.lock.Unlock()
	delete(f.pending, directory)
	if f.active != nil && f.active.directory == directory {
		f.cancelled = true
	}
}

// next removes and returns the pending request that can be attempted first,
// along with the time until it can be attempted, which is zero or less if it
// can be attempted now. If there's no pending request, then it returns false.
func (f *gitFetcher) next() (gitFetchRequest, time.Duration, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()
	var result gitFetchRequest
	var wait time.Duration
	var found bool
	for _, request := range f.pending {
		remaining, _ := f.retries.wait(request.key)
		if !found || remaining < wait {
			result, wait, found = request, remaining, true
		}
	}
	if found && wait <= 0 {
		delete(f.pending, result.directory)
		f.active = &result
		f.cancelled = false
	}
	return result, wait, found
}

// finish records the outcome of the attempt for the request whose fetch was in
// progress. If it failed, then the request is kept for another attempt, unless
// it was cancelled or a newer request for the same repository has arrived.
func (f *gitFetcher) finish(request gitFetchRequest, err error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if err != nil {
		f.retries.failed(request.key, err)
		if _, ok := f.pending[request.directory]; !ok && !f.cancelled {
			f.pending[request.directory] = request
		}
	} else {
		f.retries.succeeded(request.key)
	}
	f.active = nil
	f.cancelled = false
}

// run performs fetches until the context is cancelled. It must be called at
// most once.
func (f *gitFetcher) run(ctx context.Context) {
	// Create a timer for waiting on throttled requests.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	// Loop until cancelled.
	for {
		// If there's a request that can be attempted, then attempt it.
		request, wait, found := f.next()
		if found && wait <= 0 {
			// If the repository is gone, then there's nothing to fetch into,
			// and the request is dropped. If the commit is already there,
			// such as because the user fetched it, then there's nothing to
			// fetch either, but the session still has to be woken. Otherwise,
			// attempt the fetch.
			var err error
			if _, statErr := os.Stat(filepath.Join(request.directory, ".git")); statErr != nil {
				f.provider.logger.Debugf("Dropping request to fetch commit %s for missing repository", gitShortCommit(request.key.commit))
				f.cancel(request.directory)
				err = statErr
			} else if !f.provider.hasCommit(ctx, request.directory, request.key.commit) {
				err = f.provider.fetchMissing(ctx, request.directory, request.key.url, request.key.commit)
			}
			if ctx.Err() != nil {
				return
			}
			f.finish(request, err)
			if err != nil {
				f.provider.logger.Debugf("Unable to fetch commit %s for submodule: %v", gitShortCommit(request.key.commit), err)
			} else {
				f.signal()
			}
			continue
		}

		// Otherwise, wait for a request, for one to become available, or for
		// cancellation.
		var timeout <-chan time.Time
		if found {
			timer.Reset(wait)
			timeout = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-f.wake:
		case <-timeout:
		}
		if found && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}
