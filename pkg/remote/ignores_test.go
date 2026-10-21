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
		if excluded := test.ignores.excluded(test.path); excluded != test.excluded {
			t.Errorf("incorrect exclusion of %q by %+v: %q", test.path, test.ignores, excluded)
		}
	}
}
