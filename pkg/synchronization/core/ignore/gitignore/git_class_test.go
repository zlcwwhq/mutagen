package gitignore

import (
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// probeNames returns the names of single-character files that are created to
// probe the behavior of bracket expressions and wildcards. Characters that
// can't appear in file names are omitted. The control characters that are
// included are those that classes such as "space" and "cntrl" treat
// differently.
func probeNames() []string {
	names := []string{"\t", "\n", "\v", "\f", "\r", "\x01"}
	for b := byte(' '); b < 127; b++ {
		if b == '/' || b == '.' {
			continue
		}
		names = append(names, string([]byte{b}))
	}
	return append(names, "é")
}

// randomClass generates a random pattern containing a bracket expression.
func randomClass(r *rand.Rand) string {
	fragments := []string{
		"a", "b", "z", "A", "0", "9", "-", "-", "]", "]", "!", "^", "\\",
		"[:alpha:]", "[:digit:]", "[:upper:]", "[:lower:]", "[:space:]",
		"[:punct:]", "[:foo:]", "[:", ":]", ":", "[", "[[:alpha:]", "a-z",
		"0-9", "z-a", "\\]", "\\-", "\\a", "*", "?", "é", "[:alpha",
		"[=a=]", "[.a.]", "/", " ", "_",
	}
	pattern := "["
	for i, count := 0, r.Intn(7); i < count; i++ {
		pattern += fragments[r.Intn(len(fragments))]
	}
	if r.Intn(8) != 0 {
		pattern += "]"
	}
	switch r.Intn(6) {
	case 0:
		pattern += "?"
	case 1:
		pattern = "?" + pattern
	case 2:
		pattern += "*"
	case 3:
		pattern = "*" + pattern
	}
	return pattern
}

// TestGitClassDifferential compares the results of bracket expressions and
// single-character wildcards with those of git for every single-byte file name.
func TestGitClassDifferential(t *testing.T) {
	// Single-character file names aren't portable.
	if runtime.GOOS == "windows" {
		t.Skip("single-character file names are not portable to Windows")
	}

	// Create a harness and a repository.
	h := newGitHarness(t)
	root := t.TempDir()
	h.initialize(root)

	// Generate the patterns to test.
	patterns := []string{
		"[ab]", "[!a]", "[^a]", "[a-c]", "[c-a]", "[]a]", "[!]a]", "[a-]",
		"[-a]", "[a", "[", "[]", "[!", "[]]", "[[:alpha:]]", "[[:foo:]]",
		"[[:alpha:]", "[\\]a]", "[a-\\]]", "?", "*", "[[:alpha:]-z]",
		"[a-[:alpha:]]", "[[:alpha:]x]", "[[:]", "[[:]]", "[[::]]", "[[a]",
		"[[]", "[[]]", "[:alpha:]", "[[:alpha]]", "[a-a]", "[--/]", "[+--]",
		"[!-]", "[\\a-c]", "[a-\\c]", "[a\\-c]", "[ !]", "[ -/]", "[!-/]",
		"[[:punct:]]", "[[:space:]]", "[[:blank:]]", "[[:cntrl:]]",
		"[[:graph:]]", "[[:print:]]", "[[:xdigit:]]", "[[:upper:]]",
		"[[:lower:]]", "[[:alnum:]]", "[[:digit:]]", "[^[:alpha:]]",
		"[![:alpha:]]", "[a-z-9]", "[a-z-]", "[%--]", "[--0]",
	}
	seed, count := fuzzParameters(t, 7, 500)
	r := rand.New(rand.NewSource(seed))
	for i := 0; i < count; i++ {
		patterns = append(patterns, randomClass(r))
	}

	// Create a scenario for each pattern, containing every probe file.
	names := probeNames()
	scenarios := make([]scenario, len(patterns))
	for i, pattern := range patterns {
		scenarios[i] = scenario{
			name:  "c" + strconv.Itoa(i),
			files: map[string]string{".gitignore": pattern + "\n"},
		}
		directory := filepath.Join(root, scenarios[i].name)
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal("unable to create scenario directory:", err)
		}
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(directory, name), nil, 0o644); err != nil {
				t.Fatal("unable to create probe file:", err)
			}
		}
		if err := os.WriteFile(filepath.Join(directory, ".gitignore"), []byte(pattern+"\n"), 0o644); err != nil {
			t.Fatal("unable to write rule file:", err)
		}
	}

	// Compare.
	if compare(t, h, root, NewIgnorer(root, nil), describeScenarios(scenarios)) == 0 {
		t.Error("git did not ignore anything")
	}
}
