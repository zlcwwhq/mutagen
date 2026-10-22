package remote

import (
	"testing"

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
		if excluded := test.ignores.Excluded(test.path, true); excluded != test.excluded {
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
		if excluded := test.ignores.Excluded(test.path, false); excluded != test.excluded {
			t.Errorf("incorrect exclusion of file %q by %+v: %q", test.path, test.ignores, excluded)
		}
	}
}
