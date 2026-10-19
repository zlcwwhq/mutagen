package cli

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestMergeThreeWayAmbiguousAlignment(t *testing.T) {
	// Changes made identically on both sides must be applied once, even where
	// edit scripts could align them in several ways, such as next to equal
	// lines, or by matching a line across a delimiter, as if it had moved.
	markers := markersFor("test")
	for _, test := range []struct {
		description                 string
		base, local, remote, merged string
	}{
		{"insertion after a repeated line", "a\nb\n", "x\na\nb\nb\n", "a\nb\nb\n", "x\na\nb\nb\n"},
		{"insertion before a repeated line", "a\nb\n", "a\na\nb\nx\n", "a\na\nb\n", "a\na\nb\nx\n"},
		{"insertion into a run", "a\na\na\nb\n", "x\na\na\na\na\nb\n", "a\na\na\na\nb\n", "x\na\na\na\na\nb\n"},
		{"deletion from a run", "a\na\na\nb\n", "x\na\na\nb\n", "a\na\nb\n", "x\na\na\nb\n"},
		{"repeated block", "a\nb\n", "x\na\nb\na\nb\n", "a\nb\na\nb\n", "x\na\nb\na\nb\n"},
		{"removal of a repeated block", "a\nb\na\nb\nc\n", "a\nb\nc\ny\n", "a\nb\nc\n", "a\nb\nc\ny\n"},
		{"insertion without a final newline", "a\nb", "x\na\nb\nb", "a\nb\nb", "x\na\nb\nb"},
		{"deletion matched across a delimiter", "S1\nb\nb\nb\nS2\nb\n", "a\na\na\nb\nS1\nb\nb\nS2\nb\n", "S1\nb\nb\nS2\nb\n", "a\na\na\nb\nS1\nb\nb\nS2\nb\n"},
		{"insertion of lines that a separate change removes", "a\na\nb\nc\nS1\na\n", "a\na\na\nS1\na\nb\na\nc\na\n", "a\na\nb\nc\nS1\na\nb\na\nc\na\n", "a\na\na\nS1\na\nb\na\nc\na\n"},
		{"deletion of a line that a separate change inserts", "S1\nb\na\nb\n", "b\na\nb\nb\nS1\nb\nb\n", "S1\nb\nb\n", "b\na\nb\nb\nS1\nb\nb\n"},
		{"line moved differently by each side", "b\nc\nc\nb\nS1\nc\nS2\nb\na\nc\n", "b\nc\nc\nb\nS1\nc\nc\na\nc\nS2\n", "c\nb\nc\nc\na\nS1\nc\nS2\n", "c\nb\nc\nc\na\nS1\nc\nc\na\nc\nS2\n"},
	} {
		for _, sides := range [][2]string{{test.local, test.remote}, {test.remote, test.local}} {
			if merged, regions := mergeThreeWay([]byte(test.base), []byte(sides[0]), []byte(sides[1]), markers); merged != test.merged || regions != 0 {
				t.Errorf("%s: %q and %q merged into %q with %d regions instead of %q", test.description, sides[0], sides[1], merged, regions, test.merged)
			}
		}
	}
}

func TestMergeThreeWayProperties(t *testing.T) {
	markers := markersFor("test")
	random := rand.New(rand.NewSource(1))
	// Lines are drawn from tiny alphabets, so that alignments are ambiguous.
	lines := func(maximum int, first byte, letters int) []string {
		result := make([]string, random.Intn(maximum+1))
		for i := range result {
			result[i] = string(first+byte(random.Intn(letters))) + "\n"
		}
		return result
	}
	join := func(segments ...[]string) string {
		var result strings.Builder
		for _, segment := range segments {
			result.WriteString(strings.Join(segment, ""))
		}
		return result.String()
	}
	merge := func(base, local, remote, expected string, clean bool) {
		t.Helper()
		for _, sides := range [][2]string{{local, remote}, {remote, local}} {
			if merged, regions := mergeThreeWay([]byte(base), []byte(sides[0]), []byte(sides[1]), markers); (regions == 0 || clean) && (merged != expected || regions != 0) {
				t.Fatalf("%q, %q, and %q merged into %q with %d regions instead of %q", base, sides[0], sides[1], merged, regions, expected)
			}
		}
	}
	for i := 0; i < 10000; i++ {
		letters := 2 + i%2

		// Merges with an unchanged side take the other side, and identical
		// sides merge into themselves.
		base, other := join(lines(6, 'a', letters)), join(lines(6, 'a', letters))
		merge(base, base, other, other, true)
		merge(base, other, other, other, true)

		// Both sides make one identical change, and each side makes another
		// change in a separate segment, delimited by unique lines. A clean
		// merge must apply each change exactly once.
		segments := make([][]string, 2+random.Intn(4))
		for s := range segments {
			segments[s] = lines(5, 'a', letters)
		}
		delimited := func(segments [][]string) string {
			var result strings.Builder
			for s, segment := range segments {
				if s > 0 {
					fmt.Fprintf(&result, "S%d\n", s)
				}
				result.WriteString(join(segment))
			}
			return result.String()
		}
		order := random.Perm(len(segments))
		local, remote, expected := slicesOf(segments), slicesOf(segments), slicesOf(segments)
		change := lines(5, 'a', letters)
		local[order[0]], remote[order[0]], expected[order[0]] = change, change, change
		change = lines(5, 'a', letters)
		local[order[1]], expected[order[1]] = change, change
		if len(segments) > 2 {
			change = lines(5, 'a', letters)
			remote[order[2]], expected[order[2]] = change, change
		}
		merge(delimited(segments), delimited(local), delimited(remote), delimited(expected), false)

		// Without delimiters, an identical change made anywhere must merge
		// with lines that only one side added at the start, and only the other
		// at the end.
		original := lines(8, 'a', letters)
		start := random.Intn(len(original) + 1)
		end := start + random.Intn(len(original)-start+1)
		changed := join(original[:start], lines(4, 'a', letters), original[end:])
		prefix, suffix := join(lines(3, 'x', 1)), join(lines(3, 'y', 1))
		merge(join(original), prefix+changed, changed+suffix, prefix+changed+suffix, false)
	}
}

// slicesOf returns a shallow copy of a list of segments.
func slicesOf(segments [][]string) [][]string {
	return append([][]string(nil), segments...)
}
