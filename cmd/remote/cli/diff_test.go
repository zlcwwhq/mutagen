package cli

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestCompareOmittedConflicts(t *testing.T) {
	var errorOutput strings.Builder
	target := &remote.Target{Root: t.TempDir(), Contained: true}
	report := &remote.Report{ExcludedConflicts: 1}
	if _, err := compare(context.Background(), target.Root, "", target, report, nil, io.Discard, &errorOutput); err == nil ||
		!strings.Contains(err.Error(), "specify paths") || strings.Contains(errorOutput.String(), "no conflicts") {
		t.Fatalf("omitted conflicts reported as identical: %v, %q", err, errorOutput.String())
	}
}

// longestCommonSubsequence computes the length of the longest common
// subsequence of two line sequences.
func longestCommonSubsequence(a, b []string) int {
	lengths := make([][]int, len(a)+1)
	for i := range lengths {
		lengths[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lengths[i][j] = lengths[i+1][j+1] + 1
			} else {
				lengths[i][j] = max(lengths[i+1][j], lengths[i][j+1])
			}
		}
	}
	return lengths[0][0]
}

// checkScript verifies that an edit script transforms a into b, returning the
// number of edits.
func checkScript(t *testing.T, a, b []string, script []edit) int {
	t.Helper()
	var from, to []string
	var edits int
	for _, e := range script {
		if e.operation != '+' {
			from = append(from, e.line)
		}
		if e.operation != '-' {
			to = append(to, e.line)
		}
		if e.operation != ' ' {
			edits++
		}
	}
	if strings.Join(from, "|") != strings.Join(a, "|") || strings.Join(to, "|") != strings.Join(b, "|") {
		t.Fatalf("script doesn't transform %q into %q: %v", a, b, script)
	}
	return edits
}

func TestDiffLinesMinimal(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	sequence := func() []string {
		lines := make([]string, random.Intn(12))
		for i := range lines {
			lines[i] = string(rune('a' + random.Intn(3)))
		}
		return lines
	}
	for i := 0; i < 2000; i++ {
		a, b := sequence(), sequence()
		edits := checkScript(t, a, b, diffLines(a, b))
		if minimum := len(a) + len(b) - 2*longestCommonSubsequence(a, b); edits != minimum {
			t.Fatalf("script for %q and %q has %d edits instead of %d", a, b, edits, minimum)
		}
	}
}

func TestDiffLinesBounded(t *testing.T) {
	// Regions that differ too much are replaced wholesale.
	var a, b []string
	for i := 0; i < maximumMinimizedEdits; i++ {
		a, b = append(a, fmt.Sprintf("a%d\n", i)), append(b, fmt.Sprintf("b%d\n", i))
	}
	a, b = append([]string{"same\n"}, a...), append([]string{"same\n"}, b...)
	if edits := checkScript(t, a, b, diffLines(a, b)); edits != 2*maximumMinimizedEdits {
		t.Fatal("incorrect number of edits:", edits)
	}
}

func TestUnifiedDiff(t *testing.T) {
	lines := func(first, last int, replacements map[int]string) string {
		var result strings.Builder
		for i := first; i <= last; i++ {
			if replacement, ok := replacements[i]; ok {
				result.WriteString(replacement + "\n")
			} else {
				fmt.Fprintf(&result, "%d\n", i)
			}
		}
		return result.String()
	}
	// Expected outputs match GNU diff -u.
	for _, test := range []struct{ name, a, b, expected string }{
		{"identical", "a\nb\n", "a\nb\n", ""},
		{"distant changes", lines(1, 20, nil), lines(1, 20, map[int]string{5: "five", 16: "sixteen"}),
			"--- L\n+++ R\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n@@ -13,7 +13,7 @@\n 13\n 14\n 15\n-16\n+sixteen\n 17\n 18\n 19\n"},
		{"nearby changes", lines(1, 10, nil), lines(1, 10, map[int]string{4: "four", 8: "eight"}),
			"--- L\n+++ R\n@@ -1,10 +1,10 @@\n 1\n 2\n 3\n-4\n+four\n 5\n 6\n 7\n-8\n+eight\n 9\n 10\n"},
		{"missing newline", "x\ny\n", "w\nx\ny\nz",
			"--- L\n+++ R\n@@ -1,2 +1,4 @@\n+w\n x\n y\n+z\n\\ No newline at end of file\n"},
		{"deletion", "a\nb\nc\n", "", "--- L\n+++ R\n@@ -1,3 +0,0 @@\n-a\n-b\n-c\n"},
		{"single lines", "a\n", "b\n", "--- L\n+++ R\n@@ -1 +1 @@\n-a\n+b\n"},
	} {
		if actual := unifiedDiff("L", "R", diffLines(splitLines([]byte(test.a)), splitLines([]byte(test.b)))); actual != test.expected {
			t.Errorf("%s: incorrect diff:\n%s\nexpected:\n%s", test.name, actual, test.expected)
		}
	}
}

func TestCompareEntries(t *testing.T) {
	file := func(contents string, executable bool) *remote.Entry {
		return &remote.Entry{Kind: remote.EntryFile, Size: int64(len(contents)), Executable: executable,
			Digest: fmt.Sprintf("digest of %q", contents), Data: []byte(contents)}
	}
	missing := &remote.Entry{Kind: remote.EntryMissing}
	directory := &remote.Entry{Kind: remote.EntryDirectory}
	large := &remote.Entry{Kind: remote.EntryFile, Size: remote.MaximumComparedFileSize + 1, Digest: "large"}
	for _, test := range []struct {
		name            string
		local, opposite *remote.Entry
		expected        string
	}{
		{"identical files", file("a\n", false), file("a\n", false), ""},
		{"modified file", file("a\n", false), file("b\n", false), "--- f (local)\n+++ f (remote)\n@@ -1 +1 @@\n-a\n+b\n"},
		{"missing locally", missing, file("b\n", false), "--- /dev/null\n+++ f (remote)\n@@ -0,0 +1 @@\n+b\n"},
		{"missing remotely", file("a\n", false), missing, "--- f (local)\n+++ /dev/null\n@@ -1 +0,0 @@\n-a\n"},
		{"missing on both", missing, missing, ""},
		{"executability", file("a\n", true), file("a\n", false), "f: executable file of 2 bytes locally, file of 2 bytes remotely (contents are identical)\n"},
		{"binary", file("a\x00", false), file("b\x00", false), "Binary files f (local) and f (remote) differ\n"},
		{"too large", large, file("b\n", false), "f: file of 1048577 bytes locally, file of 2 bytes remotely (too large to compare)\n"},
		{"empty and missing", file("", false), missing, "f: file of 0 bytes locally, missing remotely\n"},
		{"kinds", directory, file("b\n", false), "f: directory locally, file of 2 bytes remotely\n"},
		{"directories", directory, directory, ""},
		{"links", &remote.Entry{Kind: remote.EntrySymbolicLink, Target: "a"}, &remote.Entry{Kind: remote.EntrySymbolicLink, Target: "b"},
			"f: symbolic link to a locally, symbolic link to b remotely\n"},
	} {
		if actual := compareEntries("f", test.local, test.opposite); actual != test.expected {
			t.Errorf("%s: incorrect comparison:\n%q\nexpected:\n%q", test.name, actual, test.expected)
		}
	}
}
