package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
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
// and then fails with the error configured for it (if any). Clone creates a
// Git directory, which is all that a repository amounts to here, while the
// other methods operate only on the provider's own fields.
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
	// cloneFiles maps slash-separated paths within a clone to the contents of
	// the files that Clone writes, in addition to a Git directory.
	cloneFiles map[string]string
	// cloneWarnings are the warnings that Clone returns.
	cloneWarnings []string
	// scanned is the entry that ScanClone returns, if non-nil. Otherwise,
	// ScanClone scans the clone with ignored as the ignorer.
	scanned *Entry
	// ignored are the paths within a clone that ScanClone ignores when it
	// scans the clone.
	ignored map[string]bool
	// errors maps method names to the errors that they return.
	errors map[string]error
	// hooks maps method names to callbacks that they invoke with their
	// directory argument, before failing (if they're configured to fail).
	hooks map[string]func(directory string)
	// calls are the calls that have been made, in order.
	calls []testingGitCall
	// recorded is the content passed to the most recent call to RecordSeed.
	recorded *Entry
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

// methods returns the names of the methods that have been called, in order.
func (p *testingGitProvider) methods() []string {
	p.lock.Lock()
	defer p.lock.Unlock()
	var result []string
	for _, call := range p.calls {
		result = append(result, call.method)
	}
	return result
}

// testingCreateGitDirectory creates a minimal Git directory in a directory.
func testingCreateGitDirectory(directory string) error {
	if err := os.Mkdir(filepath.Join(directory, GitLinkName), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, GitLinkName, "HEAD"), []byte("ref: refs/heads/main\n"), 0600)
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

// Clone implements GitProvider.Clone. If it fails, then it leaves partial
// content behind, as a real clone can.
func (p *testingGitProvider) Clone(_ context.Context, directory string, target *Entry) (*Entry, []string, error) {
	if err := p.run("Clone", directory, ""); err != nil {
		os.WriteFile(filepath.Join(directory, "partial"), []byte("partial"), 0600)
		return nil, nil, err
	}
	if err := testingCreateGitDirectory(directory); err != nil {
		return nil, nil, err
	}
	p.lock.Lock()
	files, warnings := p.cloneFiles, p.cloneWarnings
	p.lock.Unlock()
	for path, content := range files {
		path = filepath.Join(directory, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, nil, err
		} else if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return nil, nil, err
		}
	}
	link := target.Copy(EntryCopyBehaviorSlim)
	link.Seed = ""
	return link, warnings, nil
}

// ScanClone implements GitProvider.ScanClone.
func (p *testingGitProvider) ScanClone(ctx context.Context, directory, path string) (*Entry, error) {
	if err := p.run("ScanClone", directory, path); err != nil {
		return nil, err
	}
	p.lock.Lock()
	scanned, ignored := p.scanned, p.ignored
	p.lock.Unlock()
	if scanned != nil {
		return scanned, nil
	}
	snapshot, _, _, err := ScanGitMode(
		ctx,
		directory,
		nil, nil,
		newTestingHasher(), nil,
		&testingGitIgnorer{ignored: ignored}, nil,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return snapshot.Content, nil
}

// RecordSeed implements GitProvider.RecordSeed.
func (p *testingGitProvider) RecordSeed(_ context.Context, directory string, content *Entry) error {
	if err := p.run("RecordSeed", directory, ""); err != nil {
		return err
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	p.recorded = content
	return nil
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

// testingTemporaryClones returns the root-relative paths of the temporary
// directories for submodules within a directory.
func testingTemporaryClones(t *testing.T, root string) []string {
	t.Helper()
	var result []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		} else if strings.HasPrefix(entry.Name(), gitLinkTemporaryNamePrefix) {
			relative, _ := filepath.Rel(root, path)
			result = append(result, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal("unable to look for temporary directories:", err)
	}
	return result
}
