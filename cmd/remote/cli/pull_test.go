package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// readTree reads the files beneath a directory, with symbolic links read as
// their targets prefixed by "-> ".
func readTree(t *testing.T, directory string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		relative, _ := filepath.Rel(directory, path)
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			result[filepath.ToSlash(relative)] = "-> " + target
			return err
		}
		data, err := os.ReadFile(path)
		result[filepath.ToSlash(relative)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// writeFiles creates files with the specified contents beneath a directory.
func writeFiles(t *testing.T, directory string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPullMerge(t *testing.T) {
	staged, destination := t.TempDir(), t.TempDir()
	writeFiles(t, staged, map[string]string{
		"same": "same\n", "changed": "new\n", "added/a": "a\n", "sub/b": "b\n", "file-to-directory/c": "c\n", "directory-to-file": "d\n",
	})
	writeFiles(t, destination, map[string]string{
		"same": "same\n", "changed": "old\n", "local-only": "kept\n", "sub/local": "kept\n",
		"file-to-directory": "file\n", "directory-to-file/e": "e\n",
	})
	// The identical file also has identical metadata.
	mtime := time.Unix(1700000000, 0)
	setPullModificationTime(t, filepath.Join(staged, "same"), mtime)
	setPullModificationTime(t, filepath.Join(destination, "same"), mtime)
	links := runtime.GOOS != "windows"
	if links {
		os.Symlink("same", filepath.Join(staged, "link"))
		os.Symlink("other", filepath.Join(destination, "link"))
	}

	// Differing files are replaced and missing ones added, while local
	// content is never deleted, and kind mismatches are reported.
	summary := &pullSummary{}
	summary.merge(staged, destination, "results")
	expected := map[string]string{
		"same": "same\n", "changed": "new\n", "added/a": "a\n", "sub/b": "b\n", "sub/local": "kept\n", "local-only": "kept\n",
		"file-to-directory": "file\n", "directory-to-file/e": "e\n",
	}
	added, replaced := 2, 1
	if links {
		expected["link"] = "-> same"
		replaced++
	}
	if tree := readTree(t, destination); len(tree) != len(expected) {
		t.Fatalf("incorrect merge: %v", tree)
	} else {
		for name, data := range expected {
			if tree[name] != data {
				t.Errorf("incorrect merge of %s: %q", name, tree[name])
			}
		}
	}
	if summary.added != added || summary.replaced != replaced || summary.unchanged != 1 {
		t.Errorf("incorrect summary: %s", summary)
	} else if problems := strings.Join(summary.problems, "\n"); !strings.Contains(problems, "results/file-to-directory exists locally but isn't a directory") ||
		!strings.Contains(problems, "results/directory-to-file exists locally as a directory") || len(summary.problems) != 2 {
		t.Errorf("incorrect problems: %q", summary.problems)
	}

	// Executability differences replace files.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(staged, "same"), 0700); err != nil {
			t.Fatal(err)
		}
		summary = &pullSummary{}
		summary.merge(filepath.Join(staged, "same"), filepath.Join(destination, "same"), "same")
		if summary.replaced != 1 {
			t.Errorf("file differing in executability not replaced: %s", summary)
		}
	}
}

func TestPullParents(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"src/a.py": "", "file": ""})
	target := &remote.Target{
		Root: root, Contained: true,
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results/", "*.log", "linked", "into-*", "caf\u00e9/"}},
	}
	locator := newLocator(target)
	prepare := func(relative string) (string, error) {
		t.Helper()
		resolvedRoot, landing, err := prepareParents(context.Background(), target, locator, relative)
		if err == nil {
			if expected, resolveErr := filepath.EvalSymlinks(root); resolveErr != nil || resolvedRoot != expected {
				t.Fatalf("incorrect resolved root for %s: %s", relative, resolvedRoot)
			}
		}
		return landing, err
	}

	// Ignored parents are created, while synchronized ones aren't, and paths
	// without symbolic links land as given.
	if landing, err := prepare("results/run1/metrics.json"); err != nil {
		t.Fatal("ignored parents not created:", err)
	} else if info, err := os.Stat(filepath.Join(root, "results", "run1")); err != nil || !info.IsDir() {
		t.Fatal("ignored parents missing:", err)
	} else if expected, _ := filepath.EvalSymlinks(filepath.Join(root, "results", "run1")); landing != filepath.Join(expected, "metrics.json") {
		t.Fatal("incorrect landing:", landing)
	} else if _, err := prepare("src/train.log"); err != nil {
		t.Fatal("existing parent rejected:", err)
	} else if _, err := prepare("missing/train.log"); err == nil || !strings.Contains(err.Error(), "synchronizes it") {
		t.Fatal("synchronized parent created:", err)
	} else if _, err := prepare("file/train.log"); err == nil || !strings.Contains(err.Error(), "isn't a directory") {
		t.Fatal("file accepted as a parent:", err)
	}

	// Parents are only created if the session ignores them in every form in
	// which scans could see them.
	if _, err := prepare("cafe\u0301/x.json"); err == nil || !strings.Contains(err.Error(), "only some Unicode normalization forms") {
		t.Fatal("partially ignored parent created:", err)
	} else if _, err := os.Lstat(filepath.Join(root, "cafe\u0301")); err == nil {
		t.Fatal("partially ignored parent exists")
	}

	// Symbolic links are only followed if they're ignored, and parents are
	// checked where they land through them, so that ignored links don't lead
	// to synchronized content.
	if runtime.GOOS == "windows" {
		return
	}
	outside := t.TempDir()
	for link, target := range map[string]string{"linked": outside, "synchronized": outside, "into-src": "src", "into-results": "results"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	resolvedOutside, _ := filepath.EvalSymlinks(outside)
	if landing, err := prepare("linked/train.log"); err != nil {
		t.Fatal("ignored symbolic link not followed:", err)
	} else if landing != filepath.Join(resolvedOutside, "train.log") {
		t.Fatal("incorrect landing through a symbolic link:", landing)
	} else if _, err := prepare("synchronized/train.log"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatal("synchronized symbolic link followed:", err)
	} else if landing, err := prepare("linked/run1/train.log"); err != nil || landing != filepath.Join(resolvedOutside, "run1", "train.log") {
		t.Fatal("parent outside the root not created:", landing, err)
	} else if landing, err := prepare("into-results/run2/metrics.json"); err != nil || landing != filepath.Join(resolvedRoot, "results", "run2", "metrics.json") {
		t.Fatal("parent landing in ignored content not created:", landing, err)
	} else if _, err := prepare("into-src/run1/metrics.json"); err == nil ||
		!strings.Contains(err.Error(), "into-src/run1 resolves to src/run1 through a symbolic link, and the session synchronizes that, so remote doesn't create it") {
		t.Fatal("parent landing in synchronized content created:", err)
	} else if _, err := os.Lstat(filepath.Join(root, "src", "run1")); err == nil {
		t.Fatal("parent created in synchronized content")
	} else if landing, err := prepare("into-src/a.py"); err != nil || landing != filepath.Join(resolvedRoot, "src", "a.py") {
		t.Fatal("incorrect landing in synchronized content:", landing, err)
	}
}

func TestPullChecks(t *testing.T) {
	// Paths must be ignored in every form in which scans could see them, so
	// literal patterns that match only one form of a name don't allow pulling
	// it in any form, while patterns that match every form do.
	composed, decomposed := "caf\u00e9.txt", "cafe\u0301.txt"
	literal := &remote.Ignores{Syntax: ignore.Syntax_SyntaxDocker, Patterns: []string{composed, "data", "!data/keep"}}
	wildcard := &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"caf*.txt"}}
	for _, test := range []struct {
		ignores   *remote.Ignores
		path      string
		directory bool
		refusal   string
	}{
		{literal, decomposed, false, "the session ignores it in only some Unicode normalization forms of its path and may synchronize it"},
		{literal, composed, false, "the session ignores it in only some Unicode normalization forms of its path and may synchronize it"},
		{wildcard, decomposed, false, ""},
		{wildcard, composed, true, ""},
		{literal, "train.py", false, "the session synchronizes it, so remote doesn't write it"},
		{literal, "data", true, "the session synchronizes some content beneath it"},
		{literal, "data", false, ""},
		{literal, "data/other", false, ""},
	} {
		if err := checkIgnored(test.ignores, test.path, test.directory); test.refusal == "" && err != nil {
			t.Errorf("%q (directory: %t) refused: %v", test.path, test.directory, err)
		} else if test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)) {
			t.Errorf("%q (directory: %t) not refused correctly: %v", test.path, test.directory, err)
		}
	}

	// Paths are checked where writes to them land, given the root, both with
	// symbolic links resolved: inside the root, the session must ignore that
	// path, and outside it, directories mustn't contain the root.
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := &remote.Target{
		Root: root, Path: "sub", Contained: true,
		Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results/", "into-*", composed}},
	}
	locator := newLocator(target)
	for _, test := range []struct {
		landing   string
		directory bool
		refusal   string
	}{
		{filepath.Join(root, "results", "x"), false, ""},
		{filepath.Join(root, "results"), true, ""},
		{filepath.Join(base, "outside", "x"), true, ""},
		{filepath.Join(root, "src", "data"), false, "it resolves to ../src/data through a symbolic link, and the session synchronizes that, so remote doesn't write it"},
		{filepath.Join(root, "results"), false, "it resolves to ../results through a symbolic link, and the session synchronizes that"},
		{filepath.Join(root, decomposed), false, "it resolves to ../" + decomposed + " through a symbolic link, and the session ignores that in only some Unicode normalization forms"},
		{root, true, "it resolves to the session's root (..) through a symbolic link"},
		{base, true, "it resolves to " + base + " through a symbolic link, which contains the session's root"},
		{base, false, ""},
	} {
		if err := checkLanding(target, locator, root, test.landing, test.directory, "it", "write it"); test.refusal == "" && err != nil {
			t.Errorf("landing at %s (directory: %t) refused: %v", test.landing, test.directory, err)
		} else if test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)) {
			t.Errorf("landing at %s (directory: %t) not refused correctly: %v", test.landing, test.directory, err)
		}
	}
}

// relaySession connects the endpoints of a session whose alpha endpoint, at
// the specified root, submits commands, and whose beta endpoint, at the other
// root, executes them, relaying submissions as the daemon would.
func relaySession(t *testing.T, session string, alpha, beta string, ignores *remote.Ignores) {
	t.Helper()
	submitting, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: session, Root: alpha, Alpha: true, Submit: true, Ignores: ignores})
	if err != nil {
		t.Fatal(err)
	}
	executing, err := remote.NewLocalChannel(nil, &remote.Endpoint{Session: session, Root: beta, Execute: true, Ignores: ignores})
	if err != nil {
		submitting.Close()
		t.Fatal(err)
	}
	var routes sync.WaitGroup
	routes.Add(1)
	go func() {
		defer routes.Done()
		for {
			submission, err := submitting.Accept()
			if err != nil {
				return
			}
			routes.Add(1)
			go func() {
				defer routes.Done()
				if header, err := remote.ReceiveSubmission(context.Background(), submission); err != nil {
					remote.Reject(submission, err)
				} else if header.Operation != remote.OperationExecute {
					remote.Reject(submission, errors.New("unsupported operation"))
				} else if execution, err := executing.Open(context.Background()); err != nil {
					remote.Reject(submission, err)
				} else {
					remote.Splice(submission, execution)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		submitting.Close()
		executing.Close()
		routes.Wait()
	})
}

// setupPullSessions creates the alpha and beta roots of a session that ignores
// results, with the specified local and remote files, relays its submissions,
// and registers its endpoints' scopes, as its endpoints would register them.
// It returns the alpha root and a function that pulls paths through the
// session from there, returning the output.
func setupPullSessions(t *testing.T, local, remoteFiles map[string]string) (string, func(...string) (string, error)) {
	t.Helper()

	// Avoid the test name and nested directory added by t.TempDir: combined
	// with macOS's long temporary root, they exceed Unix socket path limits.
	data, err := os.MkdirTemp("", "mr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alpha, beta := filepath.Join(base, "alpha"), filepath.Join(base, "beta")
	writeFiles(t, alpha, local)
	writeFiles(t, beta, remoteFiles)

	// Connect the session and register its scopes.
	ignores := &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"results"}}
	relaySession(t, "sync_outer", alpha, beta, ignores)
	for _, root := range []string{alpha, beta} {
		registerScope(t, "sync_outer", root == alpha, root, ignores)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return alpha, func(paths ...string) (string, error) {
		var stderr bytes.Buffer
		router := &router{ctx: ctx, directory: alpha, selection: &remote.Selection{}, stderr: &stderr}
		err := pull(router, paths)
		return stderr.String(), err
	}
}

// registerScope registers the scope of a synchronization endpoint for the
// duration of a test (see remote.RegisterScope).
func registerScope(t *testing.T, session string, alpha bool, root string, ignores *remote.Ignores) {
	t.Helper()
	scope, err := remote.RegisterScope(session, alpha, root, ignores)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scope.Close() })
}

func TestPullSynchronizedByOtherSession(t *testing.T) {
	alpha, pullPaths := setupPullSessions(t,
		map[string]string{"results/data": "local\n"},
		map[string]string{"results/data": "remote\n", "results/other": "other\n"},
	)

	// Content that the session ignores is pulled.
	if output, err := pullPaths("results/other"); err != nil || output != "remote: pulled results/other (1 added, 0 replaced, 0 unchanged)\n" {
		t.Fatalf("ignored content not pulled: %q, %v", output, err)
	}

	// Content that another session synchronizes isn't, even if that session
	// doesn't run remote commands, and local content is left unchanged.
	registerScope(t, "sync_results", true, filepath.Join(alpha, "results"), nil)
	if output, err := pullPaths("results/data"); err == nil ||
		output != "remote: unable to pull results/data: it's synchronized by sync_results, so remote doesn't write it\n" {
		t.Errorf("content synchronized by another session pulled: %q, %v", output, err)
	}
	if contents, err := os.ReadFile(filepath.Join(alpha, "results", "data")); err != nil || string(contents) != "local\n" {
		t.Errorf("local content changed: %q, %v", contents, err)
	}
}

func TestPullParentSynchronizedByOtherSession(t *testing.T) {
	alpha, pullPaths := setupPullSessions(t,
		map[string]string{"results/kept": "local\n"},
		map[string]string{"results/run1/metrics.json": "1\n"},
	)

	// A missing parent that the session ignores isn't created if another
	// session synchronizes it, since that session would propagate it even
	// though the pull itself is refused.
	registerScope(t, "sync_results", true, filepath.Join(alpha, "results"), nil)
	if output, err := pullPaths("results/run1/metrics.json"); err == nil ||
		output != "remote: unable to pull results/run1/metrics.json: results/run1 doesn't exist locally, and it's synchronized by sync_results, so remote doesn't create it\n" {
		t.Errorf("parent synchronized by another session not refused: %q, %v", output, err)
	}
	if _, err := os.Lstat(filepath.Join(alpha, "results", "run1")); err == nil {
		t.Error("parent created in content that another session synchronizes")
	}
}
