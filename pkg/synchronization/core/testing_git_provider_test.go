package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/fastpath"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// tGitURL is the URL of the repository of the GitLink entries for testing.
const tGitURL = "https://example.com/submodule.git"

// tGL0 is a GitLink entry for testing.
var tGL0 = &Entry{Kind: EntryKind_GitLink, Digest: []byte{0xc0, 0x00}, Url: tGitURL}

// tGL1 is an alternative GitLink entry (with a different commit) for testing.
var tGL1 = &Entry{Kind: EntryKind_GitLink, Digest: []byte{0xc1, 0x01}, Url: tGitURL}

// tGL2 is an alternative GitLink entry (with a different commit) for testing.
var tGL2 = &Entry{Kind: EntryKind_GitLink, Digest: []byte{0xc2, 0x02}, Url: tGitURL}

// testingSubmodule creates a submodule for testing: a directory whose Git entry
// is the specified GitLink entry and whose other contents are as specified.
func testingSubmodule(link *Entry, contents map[string]*Entry) *Entry {
	result := &Entry{Contents: map[string]*Entry{GitLinkName: link}}
	for name, entry := range contents {
		result.Contents[name] = entry
	}
	return result
}

// testingSeeded creates a copy of a GitLink entry that carries a seed.
func testingSeeded(link *Entry, seed string) *Entry {
	result := link.Copy(EntryCopyBehaviorSlim)
	result.Seed = seed
	return result
}

// testingGitIgnorer is an ignorer for tests in Git mode. Like the ignorer that
// sessions in Git mode use, it always ignores Git entries, whatever their case.
type testingGitIgnorer struct {
	// ignored are the paths that are ignored, without traversal.
	ignored map[string]bool
	// masked are the paths that are ignored, but traversed in case something
	// beneath them is unignored, which makes them ignore masked.
	masked map[string]bool
}

// Ignore implements ignore.Ignorer.Ignore.
func (i *testingGitIgnorer) Ignore(path string, _ bool) (ignore.IgnoreStatus, bool) {
	if strings.EqualFold(fastpath.Base(path), GitLinkName) || i.ignored[path] {
		return ignore.IgnoreStatusIgnored, false
	} else if i.masked[path] {
		return ignore.IgnoreStatusIgnored, true
	}
	return ignore.IgnoreStatusNominal, false
}

// testingGitCall records a call to a method of testingGitProvider.
type testingGitCall struct {
	// method is the name of the method.
	method string
	// directory is the directory argument of the method.
	directory string
	// path is the path argument of the method, if it has one.
	path string
}

// testingGitProvider is a fake implementation of GitProvider for tests. Each
// method records its call, then invokes the hook configured for it (if any),
// and then fails with the error configured for it (if any).
type testingGitProvider struct {
	// lock serializes access to the provider's fields, since scans and
	// transitions may run concurrently with tests that inspect them.
	lock sync.Mutex
	// links maps the root-relative paths of submodules to the GitLink entries
	// that Probe returns for them. Probe returns nil for any other path.
	links map[string]*Entry
	// probeErrors maps root-relative paths to the errors that Probe returns
	// for them, in addition to any error configured for all of its calls.
	probeErrors map[string]error
	// errors maps method names to the errors that they return.
	errors map[string]error
	// hooks maps method names to callbacks that they invoke with their
	// directory argument, before failing (if they're configured to fail).
	hooks map[string]func(directory string)
	// calls are the calls that have been made, in order.
	calls []testingGitCall
}

// begin records a call and returns the hook and the error configured for its
// method. The hook has to be called without the lock held, since it may
// inspect the provider.
func (p *testingGitProvider) begin(method, directory, path string) (func(string), error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.calls = append(p.calls, testingGitCall{method, directory, path})
	return p.hooks[method], p.errors[method]
}

// run records a call to a method, invokes its hook, and returns its error.
func (p *testingGitProvider) run(method, directory, path string) error {
	hook, err := p.begin(method, directory, path)
	if hook != nil {
		hook(directory)
	}
	return err
}

// callsTo returns the calls that have been made to a method, in order.
func (p *testingGitProvider) callsTo(method string) []testingGitCall {
	p.lock.Lock()
	defer p.lock.Unlock()
	var result []testingGitCall
	for _, call := range p.calls {
		if call.method == method {
			result = append(result, call)
		}
	}
	return result
}

// Probe implements GitProvider.Probe.
func (p *testingGitProvider) Probe(_ context.Context, directory, path string) (*Entry, error) {
	if err := p.run("Probe", directory, path); err != nil {
		return nil, err
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	if err := p.probeErrors[path]; err != nil {
		return nil, err
	}
	return p.links[path], nil
}

// testingWriteContent creates files and directories within a directory. The
// keys of the map are slash-separated paths, and a key that ends with a slash
// is a directory, whose value is ignored. Parent directories are created as
// necessary. The files are backdated so that their digests aren't racy (see
// testingContentManager.generate).
func testingWriteContent(t *testing.T, directory string, content map[string]string) {
	t.Helper()
	for path, data := range content {
		full := filepath.Join(directory, filepath.FromSlash(strings.TrimSuffix(path, "/")))
		if strings.HasSuffix(path, "/") {
			if err := os.MkdirAll(full, 0700); err != nil {
				t.Fatal("unable to create directory:", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal("unable to create parent directory:", err)
		} else if err := os.WriteFile(full, []byte(data), 0600); err != nil {
			t.Fatal("unable to write file:", err)
		}
		modificationTime := time.Now().Add(-time.Hour)
		if err := os.Chtimes(full, modificationTime, modificationTime); err != nil {
			t.Fatal("unable to backdate file:", err)
		}
	}
}
