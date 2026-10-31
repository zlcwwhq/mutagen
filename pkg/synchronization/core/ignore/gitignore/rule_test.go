package gitignore

import (
	"strings"
	"testing"
	"time"
)

// TestTrimTrailingSpaces tests that trimTrailingSpaces behaves as expected.
func TestTrimTrailingSpaces(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"a", "a"},
		{"a ", "a"},
		{"a   ", "a"},
		{"   ", ""},
		{" a", " a"},
		{" a ", " a"},
		{"a b ", "a b"},
		{"a\t", "a\t"},
		{"a\t ", "a\t"},
		{"a \t", "a \t"},
		{"\\", "\\"},
		{"a\\", "a\\"},
		{"a\\ ", "a\\ "},
		{"a\\  ", "a\\ "},
		{"a\\ \\ ", "a\\ \\ "},
		{"a\\\\ ", "a\\\\"},
		{"a\\\\\\ ", "a\\\\\\ "},
		{"a \\ ", "a \\ "},
		{"a/ ", "a/"},
	}
	for _, test := range tests {
		if output := trimTrailingSpaces(test.input); output != test.expected {
			t.Errorf("trimming %q: output did not match expected: %q != %q", test.input, output, test.expected)
		}
	}
}

// TestParseRule tests that parseRule produces the expected rules.
func TestParseRule(t *testing.T) {
	tests := []struct {
		line string
		// valid indicates whether or not a rule is expected. If it's false,
		// then the remaining fields are ignored.
		valid         bool
		negated       bool
		directoryOnly bool
		anchored      bool
		kind          ruleKind
		literal       string
	}{
		// Lines that don't produce rules.
		{line: ""},
		{line: "#"},
		{line: "# comment"},
		{line: "#a"},
		{line: " "},
		{line: "   "},
		{line: "/"},
		{line: "//"},
		{line: "!"},
		{line: "! "},
		{line: "!/"},
		{line: "\\"},
		{line: "a\\"},
		{line: "a/\\"},
		{line: "a\\/"},
		{line: "[a"},
		{line: "[]"},
		{line: "[!"},
		{line: "[a-"},
		{line: "a[b"},
		{line: "a*[b"},
		{line: "[[:foo:]]"},
		{line: "[[:alpha:]"},
		{line: "[[:alpha"},
		{line: "[a\\]"},
		{line: "**\\"},

		// Literals.
		{line: "a", valid: true, kind: ruleKindLiteral, literal: "a"},
		{line: "abc", valid: true, kind: ruleKindLiteral, literal: "abc"},
		{line: " a", valid: true, kind: ruleKindLiteral, literal: " a"},
		{line: "a ", valid: true, kind: ruleKindLiteral, literal: "a"},
		{line: "a\\ ", valid: true, kind: ruleKindLiteral, literal: "a "},
		{line: "a\t", valid: true, kind: ruleKindLiteral, literal: "a\t"},
		{line: "a b", valid: true, kind: ruleKindLiteral, literal: "a b"},
		{line: "\\#a", valid: true, kind: ruleKindLiteral, literal: "#a"},
		{line: "\\!a", valid: true, kind: ruleKindLiteral, literal: "!a"},
		{line: "a#", valid: true, kind: ruleKindLiteral, literal: "a#"},
		{line: "a!", valid: true, kind: ruleKindLiteral, literal: "a!"},
		{line: "\\*", valid: true, kind: ruleKindLiteral, literal: "*"},
		{line: "a\\*b", valid: true, kind: ruleKindLiteral, literal: "a*b"},
		{line: "a\\\\", valid: true, kind: ruleKindLiteral, literal: "a\\"},
		{line: "a\\\\ ", valid: true, kind: ruleKindLiteral, literal: "a\\"},
		{line: "\\a", valid: true, kind: ruleKindLiteral, literal: "a"},
		{line: "!a", valid: true, negated: true, kind: ruleKindLiteral, literal: "a"},
		{line: "!!a", valid: true, negated: true, kind: ruleKindLiteral, literal: "!a"},
		{line: "a/", valid: true, directoryOnly: true, kind: ruleKindLiteral, literal: "a"},
		{line: "!a/", valid: true, negated: true, directoryOnly: true, kind: ruleKindLiteral, literal: "a"},
		{line: "a/ ", valid: true, directoryOnly: true, kind: ruleKindLiteral, literal: "a"},
		{line: "/a", valid: true, anchored: true, kind: ruleKindLiteral, literal: "a"},
		{line: "/a/", valid: true, directoryOnly: true, anchored: true, kind: ruleKindLiteral, literal: "a"},
		{line: "!/a/", valid: true, negated: true, directoryOnly: true, anchored: true, kind: ruleKindLiteral, literal: "a"},
		{line: "a/b", valid: true, anchored: true, kind: ruleKindLiteral, literal: "a/b"},
		{line: "a/b/", valid: true, directoryOnly: true, anchored: true, kind: ruleKindLiteral, literal: "a/b"},
		{line: "//a", valid: true, anchored: true, kind: ruleKindLiteral, literal: "/a"},
		{line: "a//", valid: true, directoryOnly: true, anchored: true, kind: ruleKindLiteral, literal: "a/"},
		{line: "a\\/b", valid: true, anchored: true, kind: ruleKindLiteral, literal: "a/b"},
		{line: "\\/a", valid: true, anchored: true, kind: ruleKindLiteral, literal: "/a"},

		// Suffix rules.
		{line: "*", valid: true, kind: ruleKindSuffix, literal: ""},
		{line: "**", valid: true, kind: ruleKindSuffix, literal: ""},
		{line: "*a", valid: true, kind: ruleKindSuffix, literal: "a"},
		{line: "**a", valid: true, kind: ruleKindSuffix, literal: "a"},
		{line: "*.log", valid: true, kind: ruleKindSuffix, literal: ".log"},
		{line: "!*.log", valid: true, negated: true, kind: ruleKindSuffix, literal: ".log"},
		{line: "*.log/", valid: true, directoryOnly: true, kind: ruleKindSuffix, literal: ".log"},
		{line: "/*.log", valid: true, anchored: true, kind: ruleKindSuffix, literal: ".log"},
		{line: "*\\*", valid: true, kind: ruleKindSuffix, literal: "*"},
		{line: "*/a", valid: true, anchored: true, kind: ruleKindSuffix, literal: "/a"},

		// General rules.
		{line: "?", valid: true, kind: ruleKindGeneral},
		{line: "a*", valid: true, kind: ruleKindGeneral, literal: "a"},
		{line: "a*b", valid: true, kind: ruleKindGeneral, literal: "a"},
		{line: "*a*", valid: true, kind: ruleKindGeneral},
		{line: "[ab]", valid: true, kind: ruleKindGeneral},
		{line: "a[b]c", valid: true, kind: ruleKindGeneral, literal: "a"},
		{line: "**/a", valid: true, anchored: true, kind: ruleKindGeneral},
		{line: "a/**", valid: true, anchored: true, kind: ruleKindGeneral, literal: "a/"},
		{line: "a/**/b", valid: true, anchored: true, kind: ruleKindGeneral, literal: "a/"},
		{line: "a/*", valid: true, anchored: true, kind: ruleKindGeneral, literal: "a/"},
		{line: "a/*/", valid: true, directoryOnly: true, anchored: true, kind: ruleKindGeneral, literal: "a/"},
	}
	for _, test := range tests {
		r, ok := parseRule(test.line)
		if ok != test.valid {
			t.Errorf("parsing %q: validity did not match expected: %v != %v", test.line, ok, test.valid)
			continue
		} else if !ok {
			continue
		}
		if r.negated != test.negated {
			t.Errorf("parsing %q: negated did not match expected: %v != %v", test.line, r.negated, test.negated)
		}
		if r.directoryOnly != test.directoryOnly {
			t.Errorf("parsing %q: directoryOnly did not match expected: %v != %v", test.line, r.directoryOnly, test.directoryOnly)
		}
		if r.anchored != test.anchored {
			t.Errorf("parsing %q: anchored did not match expected: %v != %v", test.line, r.anchored, test.anchored)
		}
		if r.kind != test.kind {
			t.Errorf("parsing %q: kind did not match expected: %v != %v", test.line, r.kind, test.kind)
		}
		if r.literal != test.literal {
			t.Errorf("parsing %q: literal did not match expected: %q != %q", test.line, r.literal, test.literal)
		}
	}
}

// TestParseRules tests that parseRules handles line endings, byte order marks,
// null bytes, and blank lines correctly.
func TestParseRules(t *testing.T) {
	tests := []struct {
		contents string
		expected []string
	}{
		{"", nil},
		{"\n", nil},
		{"\n\n\n", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\n\nb\n", []string{"a", "b"}},
		{"a\r\nb\r\n", []string{"a", "b"}},
		{"a\r\r\n", []string{"a\r"}},
		{"a\r", []string{"a"}},
		{"\r\n", nil},
		{"\r", nil},
		{"a \r\n", []string{"a"}},
		{"\xef\xbb\xbfa\n", []string{"a"}},
		{"\xef\xbb\xbf#a\nb\n", []string{"b"}},
		{"\xef\xbb\xbf", nil},
		{"\xef\xbb\xbf\xef\xbb\xbfa\n", []string{"\xef\xbb\xbfa"}},
		{"a\n\xef\xbb\xbfb\n", []string{"a", "\xef\xbb\xbfb"}},
		{"a\x00b\n", []string{"a"}},
		{"\x00a\nb\n", []string{"b"}},
		{"a\x00\nb\n", []string{"a", "b"}},
		{"#a\n#b\nc\n", []string{"c"}},
		{"a\n#b\nc\n", []string{"a", "c"}},
		{" #a\n", []string{" #a"}},
	}
	for _, test := range tests {
		rules := parseRules([]byte(test.contents))
		var literals []string
		for _, r := range rules {
			if r.kind != ruleKindLiteral {
				t.Fatalf("parsing %q: unexpected non-literal rule", test.contents)
			}
			literals = append(literals, r.literal)
		}
		if strings.Join(literals, "\n") != strings.Join(test.expected, "\n") || len(literals) != len(test.expected) {
			t.Errorf("parsing %q: rules did not match expected: %q != %q", test.contents, literals, test.expected)
		}
	}
}

// matchTest is a test case for matching a rule file against a path.
type matchTest struct {
	// rules is the contents of the rule file.
	rules string
	// path is the path to test, relative to the directory with the rule file.
	path string
	// directory indicates whether or not the path is a directory.
	directory bool
	// ignored is whether or not the path is expected to be ignored.
	ignored bool
}

// matchTests are the test cases for matching rules.
var matchTests = []matchTest{
	// Names.
	{"foo", "foo", false, true},
	{"foo", "foo", true, true},
	{"foo", "a/foo", false, true},
	{"foo", "a/b/foo", true, true},
	{"foo", "foobar", false, false},
	{"foo", "barfoo", false, false},
	{"foo", "fo", false, false},
	{"foo", "Foo", false, false},
	{"foo", "a/foo/b", false, false},
	{"foo bar", "foo bar", false, true},
	{"foo\\ bar", "foo bar", false, true},
	{"foo ", "foo", false, true},
	{"foo ", "foo ", false, false},
	{"foo\\ ", "foo ", false, true},
	{"foo\\ ", "foo", false, false},
	{"foo\\  ", "foo ", false, true},
	{"foo\\\\ ", "foo\\", false, true},
	{"foo\t", "foo", false, false},
	{"foo\t", "foo\t", false, true},
	{" foo", " foo", false, true},
	{" foo", "foo", false, false},

	// Directories.
	{"foo/", "foo", true, true},
	{"foo/", "foo", false, false},
	{"foo/", "a/foo", true, true},
	{"foo/", "a/foo", false, false},
	{"foo/ ", "foo", true, true},
	{"foo//", "foo", true, false},
	{"/foo/", "foo", true, true},
	{"/foo/", "foo", false, false},
	{"/foo/", "a/foo", true, false},
	{"a/foo/", "a/foo", true, true},
	{"a/foo/", "a/foo", false, false},
	{"*/", "foo", true, true},
	{"*/", "foo", false, false},
	{"**/", "foo", true, true},
	{"**/", "a/foo", true, true},
	{"**/", "foo", false, false},

	// Anchoring.
	{"/foo", "foo", false, true},
	{"/foo", "a/foo", false, false},
	{"a/foo", "a/foo", false, true},
	{"a/foo", "foo", false, false},
	{"a/foo", "b/a/foo", false, false},
	{"/a/foo", "a/foo", false, true},
	{"//foo", "foo", false, false},
	{"\\/foo", "foo", false, false},
	{"a\\/foo", "a/foo", false, true},
	{"a\\/foo", "a\\/foo", false, false},
	{"a/foo/", "a/foo", true, true},

	// Comments and escapes.
	{"#foo", "#foo", false, false},
	{"#foo", "foo", false, false},
	{"\\#foo", "#foo", false, true},
	{" #foo", " #foo", false, true},
	{"\\!foo", "!foo", false, true},
	{"\\!foo", "foo", false, false},
	{"!foo", "foo", false, false},
	{"!foo", "!foo", false, false},
	{"foo#", "foo#", false, true},
	{"fo\\o", "foo", false, true},
	{"\\*", "*", false, true},
	{"\\*", "a", false, false},
	{"\\?", "?", false, true},
	{"\\?", "a", false, false},
	{"\\[a]", "[a]", false, true},
	{"\\[a]", "a", false, false},
	{"\\\\", "\\", false, true},

	// Wildcards.
	{"*", "foo", false, true},
	{"*", "a/foo", false, true},
	{"*", "foo", true, true},
	{"/*", "foo", false, true},
	{"/*", "a/foo", false, false},
	{"*.log", "a.log", false, true},
	{"*.log", ".log", false, true},
	{"*.log", "x/a.log", false, true},
	{"*.log", "a.logx", false, false},
	{"*.log", "alog", false, false},
	{"/*.log", "a.log", false, true},
	{"/*.log", "x/a.log", false, false},
	{"*/a", "x/a", false, true},
	{"*/a", "x/y/a", false, false},
	{"*/a", "a", false, false},
	{"a*", "a", false, true},
	{"a*", "abc", false, true},
	{"a*", "ba", false, false},
	{"a*c", "abc", false, true},
	{"a*c", "ac", false, true},
	{"a*c", "abcd", false, false},
	{"a*c*e", "abcde", false, true},
	{"a*c*e", "abce", false, true},
	{"a*c*e", "acde", false, true},
	{"a*c*e", "abde", false, false},
	{"*a*", "bab", false, true},
	{"*a*", "bbb", false, false},
	{"a?c", "abc", false, true},
	{"a?c", "ac", false, false},
	{"a?c", "abbc", false, false},
	{"?", "a", false, true},
	{"?", "ab", false, false},
	{"?", "é", false, false},
	{"??", "é", false, true},
	{"???", "é", false, false},
	{"é", "é", false, true},
	{"[é]", "é", false, false},
	{"[é][é]", "é", false, true},
	{"a?", "aé", false, false},
	{"a??", "aé", false, true},
	{"a/?", "a/b", false, true},
	{"a/?", "a/bc", false, false},
	{"a/*", "a/b", false, true},
	{"a/*", "a", false, false},
	{"a/*", "a/b/c", false, false},
	{"a/*/c", "a/b/c", false, true},
	{"a/*/c", "a/b/b/c", false, false},
	{"a/*/c", "a/c", false, false},
	{"a/b*/c", "a/bx/c", false, true},
	{"a/b*/c", "a/b/c", false, true},
	{"a/b*/c", "a/bx/y/c", false, false},
	{"a/*b", "a/xb", false, true},
	{"a/*b", "a/x/b", false, false},
	{"a?b", "a/b", false, false},
	{"a/?/b", "a/x/b", false, true},
	{"a/?/b", "a//b", false, false},
	{"a\\*b", "a*b", false, true},
	{"a\\*b", "axb", false, false},

	// Double asterisks.
	{"**/foo", "foo", false, true},
	{"**/foo", "a/foo", false, true},
	{"**/foo", "a/b/foo", false, true},
	{"**/foo", "afoo", false, false},
	{"**/foo", "a/bfoo", false, false},
	{"**/foo/bar", "foo/bar", false, true},
	{"**/foo/bar", "a/foo/bar", false, true},
	{"**/foo/bar", "a/b/foo/bar", false, true},
	{"**/foo/bar", "a/xfoo/bar", false, false},
	{"/**/foo", "foo", false, true},
	{"/**/foo", "a/foo", false, true},
	{"a/**", "a/b", false, true},
	{"a/**", "a/b/c", false, true},
	{"a/**", "a", true, false},
	{"a/**", "b/a/c", false, false},
	{"a/**/", "a/b", true, true},
	{"a/**/", "a/b", false, false},
	{"a/**/b", "a/b", false, true},
	{"a/**/b", "a/x/b", false, true},
	{"a/**/b", "a/x/y/b", false, true},
	{"a/**/b", "a/xb", false, false},
	{"a/**/b", "ab", false, false},
	{"a/**/b", "x/a/b", false, false},
	{"a/**/b/**/c", "a/b/c", false, true},
	{"a/**/b/**/c", "a/x/b/y/z/c", false, true},
	{"a/**/b/**/c", "a/x/c", false, false},
	{"a/**/**/b", "a/b", false, true},
	{"a/**/**/b", "a/x/y/b", false, true},
	{"**/**", "a", false, true},
	{"**/**", "a/b", false, true},
	{"**", "a", false, true},
	{"**", "a/b", false, true},
	{"/**", "a", false, true},
	{"/**", "a/b", false, true},
	{"**/a/**", "a/b", false, true},
	{"**/a/**", "x/a/b", false, true},
	{"**/a/**", "a", false, false},
	{"**/*.txt", "a.txt", false, true},
	{"**/*.txt", "x/y/a.txt", false, true},
	{"**/*.txt", "x/y/a.txt2", false, false},
	{"a/**/*.txt", "a/b/c.txt", false, true},
	{"a/**/*.txt", "a/c.txt", false, true},
	{"a/**/*.txt", "b/c.txt", false, false},
	{"***/a", "a", false, true},
	{"***/a", "x/y/a", false, true},
	{"a/***", "a/b/c", false, true},

	// Double asterisks that aren't path components act like single ones.
	{"a**b", "axxb", false, true},
	{"a**b", "a/b", false, false},
	{"a/**b", "a/xb", false, true},
	{"a/**b", "a/x/b", false, false},
	{"a/b**", "a/bx", false, true},
	{"a/b**", "a/bx/y", false, false},
	{"a/b**/c", "a/bx/c", false, true},
	{"a/b**/c", "a/b/c", false, true},
	{"a/b**/c", "a/bc", false, false},
	{"a/b**/c", "a/bx/y/c", false, false},
	{"a**/b", "ax/b", false, true},
	{"a**/b", "ax/y/b", false, false},
	{"a**/b", "ab", false, false},
	{"x/*/**", "x/y/z", false, true},
	{"x/*/**", "x/y", false, false},
	{"a/\\**/b", "a/*/b", false, true},
	{"a/\\**/b", "a/x/b", false, false},
	{"a/**x/b", "a/yx/b", false, true},
	{"a/**x/b", "a/y/x/b", false, false},
	{"a/**\\x/b", "a/yx/b", false, true},
	{"a/**\\x/b", "a/y/x/b", false, false},

	// A double asterisk followed by an escaped slash matches slashes but,
	// unlike one followed by a slash, it doesn't match zero directories.
	{"a/**\\/b", "a/b", false, false},
	{"a/**\\/b", "a/x/b", false, true},
	{"a/**\\/b", "a/x/y/b", false, true},
	{"a/**\\/b", "a//b", false, true},
	{"**\\/b", "x/b", false, true},
	{"**\\/b", "b", false, false},
	{"a/**\\/", "a/x", true, false},

	// Classes.
	{"[ab]", "a", false, true},
	{"[ab]", "b", false, true},
	{"[ab]", "c", false, false},
	{"[ab]", "ab", false, false},
	{"[!ab]", "c", false, true},
	{"[!ab]", "a", false, false},
	{"[^ab]", "c", false, true},
	{"[^ab]", "a", false, false},
	{"[a-c]", "b", false, true},
	{"[a-c]", "d", false, false},
	{"[a-c]", "-", false, false},
	{"[!a-c]", "d", false, true},
	{"[!a-c]", "b", false, false},
	{"[c-a]", "c", false, true},
	{"[c-a]", "b", false, false},
	{"[c-a]", "a", false, false},
	{"[a-a]", "a", false, true},
	{"[]a]", "]", false, true},
	{"[]a]", "a", false, true},
	{"[]a]", "b", false, false},
	{"[!]a]", "b", false, true},
	{"[!]a]", "]", false, false},
	{"[]]", "]", false, true},
	{"[a-]", "-", false, true},
	{"[a-]", "a", false, true},
	{"[a-]", "b", false, false},
	{"[-a]", "-", false, true},
	{"[-a]", "a", false, true},
	{"[+--]", ",", false, true},
	{"[+--]", "+", false, true},
	{"[+--]", "-", false, true},
	{"[+--]", ".", false, false},
	{"[a\\-c]", "-", false, true},
	{"[a\\-c]", "b", false, false},
	{"[a\\]]", "]", false, true},
	{"[\\]a]", "]", false, true},
	{"[\\a]", "a", false, true},
	{"[\\a]", "\\", false, false},
	{"[a-\\]]", "a", false, true},
	{"[a-\\]]", "z", false, false},
	{"a[b-d]e", "ace", false, true},
	{"a[b-d]e", "aee", false, false},
	{"a[b-d]*", "acxyz", false, true},
	{"*[b-d]", "xxc", false, true},
	{"*[b-d]", "xxe", false, false},
	{"[[]", "[", false, true},
	{"[[]a]", "[a]", false, true},
	{"[[a]", "[", false, true},
	{"[[a]", "a", false, true},
	{"[!/]x", "ax", false, true},
	{"a[/]b", "a/b", false, false},
	{"a[!x]b", "a/b", false, false},
	{"a[^x]b", "a/b", false, false},
	{"a/[!x]/b", "a/y/b", false, true},
	{"a/[!x]/b", "a//b", false, false},
	{"[a-z]*/b", "abc/b", false, true},
	{"[!a]/b", "x/b", false, true},

	// Classes with bytes of multi-byte characters.
	{"[!a]", "é", false, false},
	{"[!a][!a]", "é", false, true},
	{"[\xc3]\xa9", "é", false, true},
	{"[\x80-\xff]?", "é", false, true},

	// Named classes.
	{"[[:alpha:]]", "a", false, true},
	{"[[:alpha:]]", "Z", false, true},
	{"[[:alpha:]]", "1", false, false},
	{"[[:alpha:]]", "é", false, false},
	{"[![:alpha:]]", "1", false, true},
	{"[^[:alpha:]]", "a", false, false},
	{"[[:digit:]]", "5", false, true},
	{"[[:digit:]]", "a", false, false},
	{"[[:alnum:]]", "5", false, true},
	{"[[:alnum:]]", "_", false, false},
	{"[[:lower:]]", "a", false, true},
	{"[[:lower:]]", "A", false, false},
	{"[[:upper:]]", "A", false, true},
	{"[[:upper:]]", "a", false, false},
	{"[[:space:]]", " ", false, true},
	{"[[:space:]]", "\t", false, true},
	{"[[:space:]]", "a", false, false},
	{"[[:blank:]]", " ", false, true},
	{"[[:blank:]]", "\v", false, false},
	{"[[:punct:]]", "!", false, true},
	{"[[:punct:]]", "_", false, true},
	{"[[:punct:]]", " ", false, false},
	{"[[:punct:]]", "a", false, false},
	{"[[:xdigit:]]", "f", false, true},
	{"[[:xdigit:]]", "g", false, false},
	{"[[:cntrl:]]", "\x01", false, true},
	{"[[:cntrl:]]", "a", false, false},
	{"[[:graph:]]", "a", false, true},
	{"[[:graph:]]", " ", false, false},
	{"[[:print:]]", " ", false, true},
	{"[[:print:]]", "\x01", false, false},
	{"[[:alpha:][:digit:]]", "5", false, true},
	{"[[:alpha:]x]", "x", false, true},
	{"[[:alpha:]_]", "_", false, true},
	{"[[:alpha:]]*", "a1", false, true},

	// Invalid rules never match.
	{"[a", "a", false, false},
	{"[a", "[a", false, false},
	{"[", "[", false, false},
	{"[]", "[]", false, false},
	{"[!", "!", false, false},
	{"[a-", "a", false, false},
	{"a[", "a[", false, false},
	{"*[", "x[", false, false},
	{"[[:foo:]]", "a", false, false},
	{"[[:foo:]]", ":", false, false},
	{"[[:alpha:]", "a", false, false},
	{"\\", "\\", false, false},
	{"a\\", "a\\", false, false},
	{"a\\", "a", false, false},
	{"[a\\]", "a", false, false},
	{"[[:alpha:]\\", "a", false, false},
	{"a/**\\", "a/b", false, false},
	{"a/\\", "a/\\", false, false},

	// Negation.
	{"*.log\n!keep.log", "keep.log", false, false},
	{"*.log\n!keep.log", "other.log", false, true},
	{"!keep.log\n*.log", "keep.log", false, true},
	{"*.log\n!keep.log\nkeep.log", "keep.log", false, true},
	{"*\n!a", "a", false, false},
	{"*\n!a", "b", false, true},
	{"*\n!a/", "a", true, false},
	{"*\n!a/", "a", false, true},
	{"a\n!a\na\n!a", "a", false, false},
	{"b/*\n!b/a", "b/a", false, false},
	{"b/*\n!b/a", "b/c", false, true},
	{"!a", "a", false, false},
	{"!*", "a", false, false},
	{"!/", "a", false, false},

	// Line endings, byte order marks, and null bytes.
	{"a\r\nb\r\n", "a", false, true},
	{"a\r\nb\r\n", "b", false, true},
	{"a\r\nb\r\n", "a\r", false, false},
	{"a\r\r\n", "a", false, false},
	{"a\r\r\n", "a\r", false, true},
	{"a\r", "a", false, true},
	{"a/\r\n", "a", true, true},
	{"\xef\xbb\xbfa\n", "a", false, true},
	{"\xef\xbb\xbfa\n", "\xef\xbb\xbfa", false, false},
	{"\xef\xbb\xbf#a\n", "a", false, false},
	{"\xef\xbb\xbf#a\n", "#a", false, false},
	{"a\x00b\n", "a", false, true},
	{"a\x00b\n", "ab", false, false},
	{"a\x00b\n", "b", false, false},
	{"\x00a\nb\n", "a", false, false},
	{"\x00a\nb\n", "b", false, true},
	{"a\nb", "b", false, true},
	{"a\n\n\nb\n", "b", false, true},

	// Reasonable real-world rules.
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "node_modules", true, true},
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "src/node_modules", true, true},
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "node_modules", false, false},
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "src/a.pyc", false, true},
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "build", true, true},
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "src/build", true, false},
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "src/.env", false, true},
	{"node_modules/\n*.pyc\n__pycache__/\n/build\n.env\n", "src/main.py", false, false},
	{"/*\n!/src\n/src/*\n!/src/main.go\n", "src", true, false},
	{"/*\n!/src\n/src/*\n!/src/main.go\n", "docs", true, true},
	{"/*\n!/src\n/src/*\n!/src/main.go\n", "src/main.go", false, false},
	{"/*\n!/src\n/src/*\n!/src/main.go\n", "src/util.go", false, true},
}

// TestRuleMatching tests that rule files match paths as expected.
func TestRuleMatching(t *testing.T) {
	var m matcher
	for _, test := range matchTests {
		rules := parseRules([]byte(test.rules))
		var ignored bool
		base := test.path[strings.LastIndexByte(test.path, '/')+1:]
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].matches(base, test.path, test.directory, &m) {
				ignored = !rules[i].negated
				break
			}
		}
		if ignored != test.ignored {
			t.Errorf(
				"rules %q, path %q (directory: %v): result did not match expected: %v != %v",
				test.rules, test.path, test.directory, ignored, test.ignored,
			)
		}
	}
}

// TestMatcherLinearTime tests that matching patterns with many wildcards takes
// time that's linear in the length of the text, rather than exponential. The
// patterns end in wildcards so that they can't be rejected by comparing a
// suffix, and none of them match, so every way of matching them is explored.
func TestMatcherLinearTime(t *testing.T) {
	tests := []struct {
		pattern string
		text    string
	}{
		{strings.Repeat("*a", 40) + "b?", strings.Repeat("a", 5000)},
		{"x/" + strings.Repeat("**/a/", 30) + "b?", "x/" + strings.Repeat("a/", 5000) + "y"},
		{strings.Repeat("[ab]*", 40) + "c?", strings.Repeat("ab", 2500)},
		{strings.Repeat("*?", 40) + "c?", strings.Repeat("a", 5000)},
		{"x/" + strings.Repeat("*/", 40) + "b?", "x/" + strings.Repeat("a/", 5000) + "y"},
	}
	start := time.Now()
	for _, test := range tests {
		r, ok := parseRule(test.pattern)
		if !ok {
			t.Fatalf("unable to parse %q", test.pattern)
		}
		var m matcher
		base := test.text[strings.LastIndexByte(test.text, '/')+1:]
		if r.matches(base, test.text, false, &m) {
			t.Errorf("pattern %q unexpectedly matched", test.pattern)
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Fatalf("matching took too long: %v", elapsed)
		}
	}
}

// TestMatcherReuse tests that a matcher can be reused for patterns with
// different numbers of tokens.
func TestMatcherReuse(t *testing.T) {
	var m matcher
	long, _ := parseRule("a/" + strings.Repeat("[a-z]*", 50) + "/**/z")
	short, _ := parseRule("a/*")
	for i := 0; i < 5; i++ {
		if !short.matches("b", "a/b", false, &m) {
			t.Error("short pattern did not match")
		}
		if !long.matches("z", "a/"+strings.Repeat("a", 60)+"/q/z", false, &m) {
			t.Error("long pattern did not match")
		}
		if short.matches("c", "a/b/c", false, &m) {
			t.Error("short pattern unexpectedly matched")
		}
	}
}
