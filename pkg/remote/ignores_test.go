package remote

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

func TestIgnoresExcluded(t *testing.T) {
	// Directories are excluded as scans exclude them: an ignored directory
	// excludes itself and its contents, unless content beneath it may be
	// unignored, as Docker-style ignores allow, in which case only content
	// beneath it that can't be unignored is excluded.
	mutagen := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"inner", "/build", "!build/keep"}, VCS: true}
	docker := &Ignores{Syntax: ignore.Syntax_SyntaxDocker, Patterns: []string{"data", "!data/keep"}}
	for _, test := range []struct {
		ignores        *Ignores
		path, excluded string
	}{
		{nil, "inner", ""},
		{mutagen, "", ""},
		{mutagen, "src", ""},
		{mutagen, "inner", "inner"},
		{mutagen, "inner/x/y", "inner"},
		{mutagen, "src/inner", "src/inner"},
		{mutagen, "src/build", ""},
		{mutagen, "build/keep", "build"},
		{mutagen, ".git/hooks", ".git"},
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen}, ".git", ""},
		{docker, "data", ""},
		{docker, "data/keep/x", ""},
		{docker, "data/other/x", "data/other"},
		{&Ignores{Syntax: ignore.Syntax_SyntaxDocker, Patterns: []string{"build"}}, "build/x", "build"},
		// Paths are excluded in any Unicode normalization form, and the
		// excluded directory is reported in the form given.
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"café"}}, "café/x", "café"},
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"café"}}, "café", "café"},
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"café/inner"}}, "café/inner/x", "café/inner"},
		// Unresolved syntaxes and invalid patterns exclude nothing.
		{&Ignores{Patterns: []string{"inner"}}, "inner", ""},
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"inner", ""}}, "inner", ""},
	} {
		if excluded := test.ignores.ExcludedInAnyForm(test.path, true); excluded != test.excluded {
			t.Errorf("incorrect exclusion of %q by %+v: %q", test.path, test.ignores, excluded)
		}
	}
}

func TestIgnoresExcludedFiles(t *testing.T) {
	// Entries other than directories are excluded whenever they're ignored or
	// masked by an ignored parent, since they're never traversed.
	mutagen := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"*.log", "results/", "/build", "!build/keep"}, VCS: true}
	docker := &Ignores{Syntax: ignore.Syntax_SyntaxDocker, Patterns: []string{"data", "!data/keep"}}
	for _, test := range []struct {
		ignores        *Ignores
		path, excluded string
	}{
		{mutagen, "src/a.py", ""},
		{mutagen, "train.log", "train.log"},
		{mutagen, "src/train.log", "src/train.log"},
		// A pattern for directories doesn't exclude files of the same name,
		// but excludes the files beneath directories that it matches.
		{mutagen, "results", ""},
		{mutagen, "results/run1.json", "results"},
		// Mutagen-style ignores never traverse ignored directories.
		{mutagen, "build/keep", "build"},
		// VCS ignores apply to directories, so a .git file, as used by
		// worktrees and submodules, is synchronized.
		{mutagen, ".git/index", ".git"},
		{mutagen, ".git", ""},
		// Docker-style ignores traverse ignored directories that may contain
		// unignored content: unignored files are synchronized, while other
		// files are excluded.
		{docker, "data/keep", ""},
		{docker, "data/other", "data/other"},
	} {
		if excluded := test.ignores.ExcludedInAnyForm(test.path, false); excluded != test.excluded {
			t.Errorf("incorrect exclusion of file %q by %+v: %q", test.path, test.ignores, excluded)
		}
	}
}

func TestIgnoresExcludedForms(t *testing.T) {
	// Paths are excluded in any form if they're excluded as given, or in their
	// NFC or NFD form, and in every form only if they're excluded in all three.
	// The excluded entry is reported in the form given: the first one found
	// for any form, and the shallowest one that's excluded in every form.
	composed, decomposed := "caf\u00e9", "cafe\u0301"
	literal := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{composed + ".txt", "/" + decomposed + "/inner", "*.log"}}
	docker := &Ignores{Syntax: ignore.Syntax_SyntaxDocker, Patterns: []string{composed, "!" + composed + "/keep"}}
	for _, test := range []struct {
		ignores    *Ignores
		path       string
		directory  bool
		any, every string
	}{
		// Literal patterns written in one form match that form only, so they
		// exclude names in any form, but in every form only once each form
		// is matched.
		{literal, composed + ".txt", false, composed + ".txt", ""},
		{literal, decomposed + ".txt", false, decomposed + ".txt", ""},
		{literal, "src/" + composed + ".txt", false, "src/" + composed + ".txt", ""},
		{literal, composed + "/inner/x", false, composed + "/inner", ""},
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{composed + ".txt", decomposed + ".txt"}}, decomposed + ".txt", false, decomposed + ".txt", decomposed + ".txt"},
		// Patterns that match every form, and names that normalization
		// doesn't change, are excluded in every form.
		{literal, composed + ".log", false, composed + ".log", composed + ".log"},
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"caf*"}}, decomposed + "/x", false, decomposed, decomposed},
		{literal, "train.log", false, "train.log", "train.log"},
		{literal, "train.py", false, "", ""},
		// The shallowest entry that's excluded in every form is reported even
		// if the forms are excluded at different depths.
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"/" + composed, "/caf*/inner"}}, decomposed + "/inner/x", true, decomposed + "/inner", decomposed + "/inner"},
		{&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"/" + composed, "/caf*/inner"}}, composed + "/inner/x", true, composed, composed + "/inner"},
		// Directories that may contain unignored content aren't excluded,
		// while other content beneath them is excluded in the forms in which
		// they're ignored.
		{docker, composed, true, "", ""},
		{docker, composed + "/keep", false, "", ""},
		{docker, composed + "/other", false, composed + "/other", ""},
		{docker, decomposed + "/other", false, decomposed + "/other", ""},
		// Unknown ignores exclude nothing in either sense.
		{nil, composed + ".txt", false, "", ""},
		{&Ignores{Patterns: []string{"*"}}, "train.log", false, "", ""},
	} {
		if excluded := test.ignores.ExcludedInAnyForm(test.path, test.directory); excluded != test.any {
			t.Errorf("incorrect exclusion in any form of %q (directory: %t) by %+v: %q", test.path, test.directory, test.ignores, excluded)
		}
		if excluded := test.ignores.ExcludedInEveryForm(test.path, test.directory); excluded != test.every {
			t.Errorf("incorrect exclusion in every form of %q (directory: %t) by %+v: %q", test.path, test.directory, test.ignores, excluded)
		}
	}
}

// writeTestFiles creates files, with their parent directories, beneath a root.
func writeTestFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		} else if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// gitModeFiles are the files of a tree with rules at several levels, the
// root's own repository, a nested repository whose .git is a file (as for
// worktrees and submodules), and one whose .git is a directory.
var gitModeFiles = map[string]string{
	".gitignore":         "*.log\n!keep.log\nbuild/\n/data\n.env\nprivate\nsecret\n",
	"src/.gitignore":     "!*.log\ntmp/\n",
	".git/info/exclude":  "local-only\n",
	"sub/.git":           "gitdir: ../.git/modules/sub\n",
	"sub/.gitignore":     "out/\n",
	"sub/src/main.c":     "",
	"sub/deep/.git/HEAD": "ref: refs/heads/main\n",
	"plain/a.c":          "",
	"src/a.c":            "",
}

// gitModeIgnores returns the ignores of a session in Git mode whose patterns
// unignore .env, which a rule ignores, and ignore private, which a rule
// negation would otherwise unignore, bound to a root.
func gitModeIgnores(root string) *Ignores {
	ignores := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"!.env", "/private"}, VCS: true, Git: true}
	return ignores.WithRoot(root)
}

func TestIgnoresGitMode(t *testing.T) {
	root := resolveSymbolicLinks(t.TempDir())
	writeTestFiles(t, root, gitModeFiles)
	ignores := gitModeIgnores(root)
	for _, test := range []struct {
		path       string
		directory  bool
		any, every string
	}{
		// Rules of every level apply, and nested rules override those above.
		{"app.log", false, "app.log", "app.log"},
		{"keep.log", false, "", ""},
		{"src/x.log", false, "", ""},
		{"src/tmp/y", false, "src/tmp", "src/tmp"},
		{"src/a.c", false, "", ""},
		{"plain/a.c", false, "", ""},
		{"missing/x", false, "", ""},
		// Rules for directories only match directories, and rules are case
		// sensitive on every system, so that both endpoints agree.
		{"build", true, "build", "build"},
		{"build", false, "", ""},
		{"build/out.txt", false, "build", "build"},
		{"Build", true, "", ""},
		{"data", true, "data", "data"},
		{"src/data", true, "", ""},
		{"local-only", false, "local-only", "local-only"},
		// The session's patterns take precedence over rules: they unignore
		// what a rule ignores, and a rule can't unignore what they ignore.
		{".env", false, "", ""},
		{"private/x", false, "private", "private"},
		{"secret", false, "secret", "secret"},
		// Git's own entries are never synchronized, files and directories
		// alike, whatever the case of their names and the patterns say.
		{".git", true, ".git", ".git"},
		{".git/hooks/x", false, ".git", ".git"},
		{"sub/.git", false, "sub/.git", "sub/.git"},
		{".GIT", false, ".GIT", ".GIT"},
		// A nested repository is an ordinary directory, apart from its .git.
		{"sub", true, "", ""},
		{"sub/src", true, "", ""},
		{"sub/src/main.c", false, "", ""},
		{"sub/new/deeper/file", false, "", ""},
		{"sub/deep", true, "", ""},
		{"sub/deep/x", false, "", ""},
		{"sub/deep/.git/HEAD", false, "sub/deep/.git", "sub/deep/.git"},
		// That doesn't exempt it from rules.
		{"sub/out/x", false, "sub/out", "sub/out"},
		{"sub/.git/x", false, "sub/.git", "sub/.git"},
	} {
		if excluded := ignores.ExcludedInAnyForm(test.path, test.directory); excluded != test.any {
			t.Errorf("incorrect exclusion in any form of %q (directory: %t): %q", test.path, test.directory, excluded)
		}
		if excluded := ignores.ExcludedInEveryForm(test.path, test.directory); excluded != test.every {
			t.Errorf("incorrect exclusion in every form of %q (directory: %t): %q", test.path, test.directory, excluded)
		}
	}
}

func TestIgnoresGitModeRuleChanges(t *testing.T) {
	// Rules are read from disk whenever paths are evaluated, so changes to them
	// apply without recreating the ignores.
	root := resolveSymbolicLinks(t.TempDir())
	writeTestFiles(t, root, map[string]string{".gitignore": "*.tmp\n", "a/b": ""})
	ignores := gitModeIgnores(root)
	if ignores.ExcludedInAnyForm("x.tmp", false) != "x.tmp" || ignores.ExcludedInAnyForm("a/b", false) != "" {
		t.Fatal("initial rules not applied")
	}
	writeTestFiles(t, root, map[string]string{".gitignore": "a/\n"})
	if ignores.ExcludedInAnyForm("x.tmp", false) != "" || ignores.ExcludedInAnyForm("a/b", false) != "a" {
		t.Error("changed rules not applied")
	}
	if err := os.Remove(filepath.Join(root, ".gitignore")); err != nil {
		t.Fatal(err)
	} else if ignores.ExcludedInAnyForm("a/b", false) != "" {
		t.Error("removed rules still applied")
	}
}

func TestIgnoresGitModeOtherModes(t *testing.T) {
	root := resolveSymbolicLinks(t.TempDir())
	writeTestFiles(t, root, gitModeFiles)

	// Sessions that aren't in Git mode ignore neither rule files nor nested
	// repositories, and a .git file, as used by worktrees and submodules, is
	// only excluded in Git mode, even though .git directories are ignored.
	plain := (&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, VCS: true}).WithRoot(root)
	for _, path := range []string{"app.log", "build", "local-only", "sub/src", "sub/src/main.c", "sub/.git", "sub/deep/x"} {
		if excluded := plain.ExcludedInAnyForm(path, false); excluded != "" {
			t.Errorf("%q excluded without Git mode in any form: %q", path, excluded)
		} else if excluded := plain.ExcludedInEveryForm(path, false); excluded != "" {
			t.Errorf("%q excluded without Git mode in every form: %q", path, excluded)
		}
	}
	if plain.ExcludedInAnyForm(".git", true) != ".git" || plain.ExcludedInAnyForm("sub/deep/.git", true) != "sub/deep/.git" {
		t.Error(".git directories not ignored by VCS ignores")
	}

	// Without a root, only the entries named .git are known to be excluded.
	// Files and directories alike, whatever the session's patterns say, and
	// nothing is read from the working directory.
	t.Chdir(root)
	unknown := &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"!.git", "x"}, Git: true}
	for path, excluded := range map[string]string{
		"sub/.git": "sub/.git", ".GIT/x": ".GIT", "app.log": "", "build": "", "sub/src/main.c": "", "x/y": "x",
	} {
		if actual := unknown.ExcludedInAnyForm(path, false); actual != excluded {
			t.Errorf("incorrect exclusion of %q without a root: %q", path, actual)
		} else if actual := unknown.ExcludedInEveryForm(path, false); actual != excluded {
			t.Errorf("incorrect exclusion in every form of %q without a root: %q", path, actual)
		}
	}
	var nilIgnores *Ignores
	if nilIgnores.WithRoot(root) != nil || nilIgnores.ExcludedInEveryForm("sub/.git", false) != "" {
		t.Error("nil ignores exclude something")
	}
}

func TestIgnoresGitModeWire(t *testing.T) {
	// Git mode travels with the ignores, while the root they're evaluated
	// beneath doesn't, and other sessions' ignores are serialized as before.
	data, err := json.Marshal(gitModeIgnores("/some/root"))
	if err != nil || !strings.Contains(string(data), `"Git":true`) || strings.Contains(string(data), "root") {
		t.Fatalf("incorrect serialization: %s, %v", data, err)
	}
	var received Ignores
	if err := json.Unmarshal(data, &received); err != nil || !received.Git || received.root != "" {
		t.Fatalf("incorrect deserialization: %+v, %v", received, err)
	}
	if data, err := json.Marshal(&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, VCS: true}); err != nil || strings.Contains(string(data), "Git") {
		t.Fatalf("other sessions' ignores changed: %s, %v", data, err)
	}
}

func TestIgnoresExclusionSource(t *testing.T) {
	root := resolveSymbolicLinks(t.TempDir())
	writeTestFiles(t, root, gitModeFiles)
	ignores := gitModeIgnores(root)
	for _, test := range []struct {
		path      string
		directory bool
		source    ExclusionSource
	}{
		{"app.log", false, ExcludedByGitRules},
		{"build", true, ExcludedByGitRules},
		{"local-only", false, ExcludedByGitRules},
		{"sub/out", true, ExcludedByGitRules},
		{"private", true, ExcludedBySession},
		{".git", true, ExcludedByGitEntry},
		{"sub/.git", false, ExcludedByGitEntry},
		{".GIT", false, ExcludedByGitEntry},
		{"src/a.c", false, ExcludedBySession},
	} {
		if source := ignores.ExclusionSource(test.path, test.directory); source != test.source {
			t.Errorf("incorrect source of the exclusion of %q: %d", test.path, source)
		}
	}
	plain := (&Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"x"}}).WithRoot(root)
	if source := plain.ExclusionSource("x", true); source != ExcludedBySession {
		t.Error("exclusion without Git mode not attributed to the session")
	}
}

func TestDialGitMode(t *testing.T) {
	useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	writeTestFiles(t, root, gitModeFiles)
	mkdirs(t, filepath.Join(root, "build"), filepath.Join(root, "sub", "build"), filepath.Join(root, "src", "tmp"))
	var lock sync.Mutex
	var accepted []string
	registerTest(t, &Endpoint{Session: "sync_repo", Name: "repo", Root: root, Alpha: true, Ignores: &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, VCS: true, Git: true}}, "repo", &accepted, &lock)

	// Commands run in the directories of nested repositories, like any others,
	// and the target's ignores evaluate Git mode beneath the root,
	// whichever form the mode took on the way: they're loaded from the
	// registration, which records them without it.
	for _, directory := range []string{"sub", "sub/src", "sub/deep", "src", "plain"} {
		target, err := dialTest(&Selection{Corresponding: true}, filepath.Join(root, filepath.FromSlash(directory)))
		if err != nil || target.Path != directory {
			t.Errorf("directory %s refused: %+v, %v", directory, target, err)
		} else if !target.Ignores.Git || target.Ignores.ExcludedInAnyForm("app.log", false) != "app.log" {
			t.Errorf("target's ignores don't know the root: %+v", target.Ignores)
		}
	}

	// Directories that the rules ignore aren't synchronized, wherever the rules
	// are.
	for _, directory := range []string{"build", "src/tmp", "sub/build"} {
		if _, err := dialTest(&Selection{Corresponding: true}, filepath.Join(root, filepath.FromSlash(directory))); err == nil ||
			!strings.Contains(err.Error(), directory+" is ignored by session repo") {
			t.Errorf("ignored directory %s accepted or incorrectly refused: %v", directory, err)
		}
	}

	// Rules are read each time.
	writeTestFiles(t, root, map[string]string{".gitignore": "plain/\n"})
	if _, err := dialTest(&Selection{Corresponding: true}, filepath.Join(root, "plain")); err == nil ||
		!strings.Contains(err.Error(), "plain is ignored by session repo") {
		t.Error("directory ignored by a changed rule accepted:", err)
	}
}

func TestResolvedRouteIgnoresKnowRoot(t *testing.T) {
	useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	writeTestFiles(t, root, gitModeFiles)

	// The daemon sends the ignores of the selected edge with the route, and
	// they are as the registration's, so they need the root too.
	channel, err := NewLocalChannel(nil, &Endpoint{Session: "center-b", Name: "star-b", Alpha: true, Star: "project", Node: "center", PeerNode: "b", Root: root, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			connection, err := channel.Accept()
			if err != nil {
				return
			}
			if _, err := ReceiveSubmission(context.Background(), connection); err != nil {
				connection.Close()
				continue
			}
			RespondWithRoute(connection, &ResolvedRoute{Session: "center-b", Alpha: true, SourceNode: "center", Node: "b", Star: true, Revision: "b",
				Ignores: &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, VCS: true, Git: true}})
			connection.Close()
		}
	}()
	t.Cleanup(func() { channel.Close(); handlers.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	selection := &Selection{Workspace: true, Node: "b", Resolve: true, Corresponding: true}
	connection, target, err := selection.Submit(ctx, filepath.Join(root, "sub", "src"), &Submission{Operation: OperationFlush})
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if target.Ignores == nil || !target.Ignores.Git || target.Ignores.ExcludedInAnyForm("app.log", false) != "app.log" {
		t.Errorf("resolved ignores don't know the root: %+v", target.Ignores)
	}
}

func TestSynchronizingGitMode(t *testing.T) {
	useTestRegistry(t)
	root := resolveSymbolicLinks(t.TempDir())
	writeTestFiles(t, root, gitModeFiles)
	writeTestFiles(t, root, map[string]string{"app.log": "", "other.txt": ""})
	registerScopeTest(t, "sync_repo", true, root, &Ignores{Syntax: ignore.Syntax_SyntaxMutagen, VCS: true, Git: true})

	// Scopes evaluate Git mode beneath their own roots, once loaded. Ignored
	// paths aren't synchronized by the session, whereas the content of a nested
	// repository is, apart from its .git.
	for _, test := range []struct {
		path      string
		directory bool
		sessions  []string
	}{
		{"other.txt", false, []string{"sync_repo"}},
		{"plain/a.c", false, []string{"sync_repo"}},
		{"sub", true, []string{"sync_repo"}},
		{"sub/src/main.c", false, []string{"sync_repo"}},
		{"sub/src", true, []string{"sync_repo"}},
		{"sub/.gitignore", false, []string{"sync_repo"}},
		{"app.log", false, nil},
		{"sub/.git", false, nil},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		sessions, err := Synchronizing(ctx, filepath.Join(root, filepath.FromSlash(test.path)), test.directory)
		cancel()
		if err != nil {
			t.Errorf("unable to determine the sessions synchronizing %s: %v", test.path, err)
		} else if strings.Join(sessions, ", ") != strings.Join(test.sessions, ", ") {
			t.Errorf("incorrect sessions synchronizing %s (directory: %t): %v", test.path, test.directory, sessions)
		}
	}
}
