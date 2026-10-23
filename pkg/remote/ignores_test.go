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
