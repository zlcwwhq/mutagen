package cli

import (
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestMergeThreeWayAmbiguousAlignment(t *testing.T) {
	// Changes made identically on both sides must be applied once, even where
	// edit scripts could align them in several ways, such as next to equal
	// lines, or by matching a line across a delimiter, as if it had moved.
	// Where the result depends on the alignment, which merging in both
	// directions reveals (see unambiguousSegments), the part of the versions
	// involved is marked instead, with all three versions, which loses
	// nothing: each region contains everything that each version has there.
	markers := markersFor("test")
	region := func(local, base, remote string) string {
		return markers.local + "\n" + local + markers.base + "\n" + base + markers.separator + "\n" + remote + markers.remote + "\n"
	}
	for _, test := range []struct {
		description                 string
		base, local, remote, merged string
	}{
		{"insertion after a repeated line", "a\nb\n", "x\na\nb\nb\n", "a\nb\nb\n", "x\na\nb\nb\n"},
		{"insertion before a repeated line", "a\nb\n", "a\na\nb\nx\n", "a\na\nb\n", "a\na\nb\nx\n"},
		// Positioned as early as possible, the a that both sides inserted is
		// next to x, and the sides' insertions differ there, so the run is
		// marked.
		{"insertion into a run", "a\na\na\nb\n", "x\na\na\na\na\nb\n", "a\na\na\na\nb\n",
			region("x\na\na\na\na\n", "a\na\na\n", "a\na\na\na\n") + "b\n"},
		// Likewise, the a that both sides deleted is next to x.
		{"deletion from a run", "a\na\na\nb\n", "x\na\na\nb\n", "a\na\nb\n",
			region("x\na\na\n", "a\na\na\n", "a\na\n") + "b\n"},
		// Likewise, the repeated block, which both sides inserted, is next to x
		// if positioned before the original.
		{"repeated block", "a\nb\n", "x\na\nb\na\nb\n", "a\nb\na\nb\n",
			region("x\na\nb\na\nb\n", "a\nb\n", "a\nb\na\nb\n")},
		{"removal of a repeated block", "a\nb\na\nb\nc\n", "a\nb\nc\ny\n", "a\nb\nc\n", "a\nb\nc\ny\n"},
		{"insertion without a final newline", "a\nb", "x\na\nb\nb", "a\nb\nb", "x\na\nb\nb"},
		{"deletion matched across a delimiter", "S1\nb\nb\nb\nS2\nb\n", "a\na\na\nb\nS1\nb\nb\nS2\nb\n", "S1\nb\nb\nS2\nb\n", "a\na\na\nb\nS1\nb\nb\nS2\nb\n"},
		{"insertion of lines that a separate change removes", "a\na\nb\nc\nS1\na\n", "a\na\na\nS1\na\nb\na\nc\na\n", "a\na\nb\nc\nS1\na\nb\na\nc\na\n", "a\na\na\nS1\na\nb\na\nc\na\n"},
		{"deletion of a line that a separate change inserts", "S1\nb\na\nb\n", "b\na\nb\nb\nS1\nb\nb\n", "S1\nb\nb\n", "b\na\nb\nb\nS1\nb\nb\n"},
	} {
		for _, swap := range []bool{false, true} {
			local, remote, expected := test.local, test.remote, test.merged
			if swap {
				local, remote, expected = remote, local, swapSides(expected, markers)
			}
			if merged, _, threeWay := mergeWithBase([]byte(test.base), []byte(local), []byte(remote), markers); merged != expected || !threeWay {
				t.Errorf("%s: %q and %q merged into %q (three-way: %t) instead of %q", test.description, local, remote, merged, threeWay, expected)
			}
		}
	}

	// Each side moved a (with other lines) to a different segment, and merging
	// would keep it twice, which no version has, so the merge's line counts
	// are inconsistent (see consistentCounts), and every region where the
	// sides differ is marked instead, without the base version, which the
	// base still has.
	base, local, remote := "b\nc\nc\nb\nS1\nc\nS2\nb\na\nc\n", "b\nc\nc\nb\nS1\nc\nc\na\nc\nS2\n", "c\nb\nc\nc\na\nS1\nc\nS2\n"
	for _, sides := range [][2]string{{local, remote}, {remote, local}} {
		if merged, _, threeWay := mergeWithBase([]byte(base), []byte(sides[0]), []byte(sides[1]), markers); threeWay ||
			merged != markedMerge([]byte(sides[0]), []byte(sides[1]), markers) {
			t.Errorf("line moved differently by each side: %q and %q merged into %q (three-way: %t)", sides[0], sides[1], merged, threeWay)
		}
	}
}

func TestMergeMisalignedChanges(t *testing.T) {
	// Changes among repeated lines can be aligned with the base in ways that
	// misrepresent them, so that a merge drops or duplicates a change without
	// marking a conflict. Such changes must be marked instead, with all three
	// versions.
	markers := markersFor("test")
	region := func(local, base, remote string) string {
		return markers.local + "\n" + local + markers.base + "\n" + base + markers.separator + "\n" + remote + markers.remote + "\n"
	}
	for _, test := range []struct {
		description                 string
		base, local, remote, merged string
	}{
		// Local modified the first of three equal lines, and remote deleted
		// one. Positioned as late as possible, local's modification became an
		// insertion before the lines and a deletion of the last one, which
		// coincided with remote's deletion, so that the merge kept all three
		// lines (and git merges them into "  - 5" and "  - 0").
		{"modification next to a deletion", "values:\n  - 0\n  - 0\n  - 0\nend\n", "values:\n  - 5\n  - 0\n  - 0\nend\n", "values:\n  - 0\n  - 0\nend\n",
			"values:\n" + region("  - 5\n  - 0\n  - 0\n", "  - 0\n  - 0\n  - 0\n", "  - 0\n  - 0\n") + "end\n"},
		{"modification next to a deletion of a statement", "func f() {\n\twg.Add(1)\n\twg.Add(1)\n\twg.Add(1)\n}\n", "func f() {\n\twg.Add(2)\n\twg.Add(1)\n\twg.Add(1)\n}\n", "func f() {\n\twg.Add(1)\n\twg.Add(1)\n}\n",
			"func f() {\n" + region("\twg.Add(2)\n\twg.Add(1)\n\twg.Add(1)\n", "\twg.Add(1)\n\twg.Add(1)\n\twg.Add(1)\n", "\twg.Add(1)\n\twg.Add(1)\n") + "}\n"},
		// Local deleted one of two equal lines, and remote modified the first,
		// which the merge took for remote's version alone, dropping local's
		// deletion (git marks it).
		{"deletion next to a modification", "def test():\n    assert x == 1\n    assert x == 1\n    done()\n", "def test():\n    assert x == 1\n    done()\n", "def test():\n    assert x == 2\n    assert x == 1\n    done()\n",
			"def test():\n" + region("    assert x == 1\n", "    assert x == 1\n    assert x == 1\n", "    assert x == 2\n    assert x == 1\n") + "    done()\n"},
		// Local replaced a with c, and remote added c, which the merge dropped
		// (git merges them into three lines of c).
		{"replacement with an equal line", "a\nc\n", "c\nc\n", "a\nc\nc\n", region("c\nc\n", "a\nc\n", "a\nc\nc\n")},
		// Both sides moved d to the start and replaced the second b with a,
		// and remote also added e after the first b. The merge applied the
		// identical changes twice (git merges them into remote's version).
		{"identical changes next to an insertion", "a\nb\nc\nd\nb\ne\n", "d\na\nb\nc\na\ne\n", "d\na\nb\ne\nc\na\ne\n",
			region("d\na\nb\nc\na\ne\n", "a\nb\nc\nd\nb\ne\n", "d\na\nb\ne\nc\na\ne\n")},
		// Both sides added an empty line before the last brace, and local also
		// added two lines at the start. The merge added the empty line twice
		// (git merges them into local's version).
		{"identical insertion among equal lines", "}\n}\n}\nu3\nu4\nu5\nu6\na\n", "a\n}\n}\n}\n\n}\nu3\nu4\nu5\nu6\na\n", "}\n}\n\n}\nu3\nu4\nu5\nu6\na\n",
			region("a\n}\n}\n}\n\n}\n", "}\n}\n}\n", "}\n}\n\n}\n") + "u3\nu4\nu5\nu6\na\n"},
		// A region marked elsewhere doesn't excuse a lossy merge of the rest,
		// which an agent resolving the marked region wouldn't review.
		{"modification next to a deletion, and a conflict", "values:\n  - 0\n  - 0\n  - 0\nend\nX\n", "values:\n  - 5\n  - 0\n  - 0\nend\nL\n", "values:\n  - 0\n  - 0\nend\nR\n",
			"values:\n" + region("  - 5\n  - 0\n  - 0\n", "  - 0\n  - 0\n  - 0\n", "  - 0\n  - 0\n") + "end\n" + region("L\n", "X\n", "R\n")},
	} {
		for _, swap := range []bool{false, true} {
			local, remote, expected := test.local, test.remote, test.merged
			if swap {
				local, remote, expected = remote, local, swapSides(expected, markers)
			}
			if merged, _, threeWay := mergeWithBase([]byte(test.base), []byte(local), []byte(remote), markers); merged != expected || !threeWay {
				t.Errorf("%s: %q and %q merged into %q (three-way: %t) instead of %q", test.description, local, remote, merged, threeWay, expected)
			}
		}
	}

	// Each side moved the same line to a different place, and merging kept it
	// twice (git marks it), so every region where the copies differ is marked
	// instead, without the base version.
	base, local, remote := "import \"a\"\nimport \"b\"\nimport \"c\"\n", "import \"a\"\nimport \"c\"\nimport \"b\"\n", "import \"c\"\nimport \"a\"\nimport \"b\"\n"
	for _, sides := range [][2]string{{local, remote}, {remote, local}} {
		if merged, regions, threeWay := mergeWithBase([]byte(base), []byte(sides[0]), []byte(sides[1]), markers); threeWay ||
			regions != 2 || merged != markedMerge([]byte(sides[0]), []byte(sides[1]), markers) {
			t.Errorf("line moved differently by each side: %q and %q merged into %q (three-way: %t)", sides[0], sides[1], merged, threeWay)
		}
	}
}

func TestMergeThreeWayLossless(t *testing.T) {
	// Both sides make the same changes to the base, which may move, duplicate,
	// delete, or replace lines drawn from tiny alphabets, so that alignments
	// are ambiguous, and then each side makes another change, which unique
	// lines distinguish. A clean merge must be a correct result, and a merge
	// with conflicting regions must be resolvable into one: a correct result
	// must consist of its clean parts, in order, with something in place of
	// each region. Merges that mark every difference, without the base
	// version, lose nothing.
	markers := markersFor("test")
	random := rand.New(rand.NewSource(1))
	vocabularies := [][]string{{"a\n"}, {"a\n", "b\n"}, {"\n", "}\n", "a\n"}}
	unchanged := func(a, b []string) int {
		count := 0
		for _, e := range diffLines(a, b) {
			if e.operation == ' ' {
				count++
			}
		}
		return count
	}
	for i := 0; i < 20000; i++ {
		vocabulary := vocabularies[i%len(vocabularies)]
		repeated := func(maximum int) []string {
			lines := make([]string, random.Intn(maximum+1))
			for l := range lines {
				lines[l] = vocabulary[random.Intn(len(vocabulary))]
			}
			return lines
		}
		base := make([]string, random.Intn(9))
		for l := range base {
			if random.Intn(2) == 0 {
				base[l] = fmt.Sprintf("u%d\n", l)
			} else {
				base[l] = vocabulary[random.Intn(len(vocabulary))]
			}
		}
		shared := slices.Clone(base)
		for n := 1 + random.Intn(3); n > 0; n-- {
			start := random.Intn(len(shared) + 1)
			end := start + random.Intn(min(3, len(shared)-start)+1)
			block, rest := slices.Clone(shared[start:end]), slices.Concat(shared[:start], shared[end:])
			switch random.Intn(4) {
			case 0:
				at := random.Intn(len(rest) + 1)
				shared = slices.Concat(rest[:at], block, rest[at:])
			case 1:
				shared = rest
			case 2:
				at := random.Intn(len(shared) + 1)
				shared = slices.Concat(shared[:at], block, shared[at:])
			case 3:
				shared = slices.Concat(shared[:start], repeated(2), shared[end:])
			}
		}

		// Make each side's own change, and determine the correct results.
		var local, remote []string
		var correct [][]string
		switch i % 3 {
		case 0:
			// Each side inserts a unique line. A correct result yields each
			// side once the other side's line is removed from it.
			l, r := random.Intn(len(shared)+1), random.Intn(len(shared)+1)
			local = slices.Concat(shared[:l], []string{"LOCAL\n"}, shared[l:])
			remote = slices.Concat(shared[:r], []string{"REMOTE\n"}, shared[r:])
			for k := 0; k <= len(local); k++ {
				candidate := slices.Concat(local[:k], []string{"REMOTE\n"}, local[k:])
				withoutLocal := slices.DeleteFunc(slices.Clone(candidate), func(line string) bool { return line == "LOCAL\n" })
				if slices.Equal(withoutLocal, remote) {
					correct = append(correct, candidate)
				}
			}
		case 1:
			// Each side replaces a different line that occurs once.
			occurrences := make(map[string]int)
			for _, line := range shared {
				occurrences[line]++
			}
			var once []int
			for l, line := range shared {
				if occurrences[line] == 1 {
					once = append(once, l)
				}
			}
			if len(once) < 2 {
				continue
			}
			random.Shuffle(len(once), func(a, b int) { once[a], once[b] = once[b], once[a] })
			local, remote = slices.Clone(shared), slices.Clone(shared)
			local[once[0]], remote[once[1]] = "LOCAL\n", "REMOTE\n"
			result := slices.Clone(local)
			result[once[1]] = "REMOTE\n"
			correct = append(correct, result)
		case 2:
			// One side inserts more repeated lines, which don't undo any of the
			// shared changes, and the result is that side.
			at := random.Intn(len(shared) + 1)
			further := slices.Concat(shared[:at], repeated(2), shared[at:])
			if len(further) == len(shared) || unchanged(base, further) != unchanged(base, shared) {
				continue
			}
			local, remote = further, shared
			if random.Intn(2) == 0 {
				local, remote = remote, local
			}
			correct = append(correct, further)
		}
		merged, _, threeWay := mergeWithBase([]byte(strings.Join(base, "")), []byte(strings.Join(local, "")), []byte(strings.Join(remote, "")), markers)
		if !threeWay {
			continue
		}
		parts := cleanParts(merged, markers)
		if !slices.ContainsFunc(correct, func(result []string) bool { return resolvable(parts, result) }) {
			t.Fatalf("%q, %q, and %q merged into %q, which doesn't resolve into any of %q",
				strings.Join(base, ""), strings.Join(local, ""), strings.Join(remote, ""), merged, correct)
		}
	}
}

func TestConsistentCounts(t *testing.T) {
	// A merge that keeps a moved line twice has inconsistent line counts, even
	// with a conflicting region elsewhere, unless resolving the region could
	// make them consistent. Some misaligned changes leave the counts
	// consistent, however, which merging in both directions detects instead.
	clean := func(merged string) []mergeSegment {
		return []mergeSegment{{merged: splitLines([]byte(merged))}}
	}
	conflict := mergeSegment{local: []string{"L\n"}, base: []string{"X\n"}, remote: []string{"R\n"}, conflict: true}
	for _, test := range []struct {
		description         string
		base, local, remote string
		segments            []mergeSegment
		consistent          bool
	}{
		{"separate insertions", "a\n", "x\na\n", "a\nx\n", clean("x\na\nx\n"), true},
		{"identical insertions applied once", "a\n", "a\nx\n", "a\nx\n", clean("a\nx\n"), true},
		{"line moved differently by each side", "a\nb\nc\n", "b\na\nc\n", "b\nc\na\n", clean("b\na\nc\na\n"), false},
		{"line moved differently by each side, and a conflict", "a\nb\nc\nX\n", "b\na\nc\nL\n", "b\nc\na\nR\n",
			append(clean("b\na\nc\na\n"), conflict), false},
		{"conflict resolvable into consistent counts", "a\nb\nX\n", "b\na\nL\n", "b\na\nR\n",
			append(clean("b\na\n"), conflict), true},
		// These merges are wrong, but their counts are consistent.
		{"identical insertions applied twice", "a\n", "a\nx\n", "a\nx\n", clean("a\nx\nx\n"), true},
		{"dropped deletion", "0\n0\n0\n", "5\n0\n0\n", "0\n0\n", clean("5\n0\n0\n"), true},
	} {
		b, l, r := splitLines([]byte(test.base)), splitLines([]byte(test.local)), splitLines([]byte(test.remote))
		if consistent := consistentCounts(b, l, r, test.segments); consistent != test.consistent {
			t.Errorf("%s: consistency %t", test.description, consistent)
		}
	}
}

func TestMergedFileNotes(t *testing.T) {
	// Checkout must say how the merged file was prepared, so that regions
	// containing the complete copies, or marked without the base version,
	// aren't taken for regions that each side changed differently, which might
	// be resolved by keeping one side's version.
	markers := markersFor("token")
	large := strings.Repeat("x", maximumMergeInput) + "\n"
	lines := strings.Repeat("x\n", maximumMergeLines/3+1)
	for _, test := range []struct {
		description         string
		local, remote, base string
		note                string
		regions             int
		// whole indicates a single region with the complete copies, and
		// withBase that regions show the base version.
		whole, withBase bool
	}{
		{"clean merge", "1\nlocal\n3\n4\n", "1\n2\n3\nremote\n", "1\n2\n3\n4\n",
			"merged without conflicts, but merging line by line can misplace changes: compare the merged file with Local, Remote, and Base before applying it", 0, false, false},
		{"conflicting region", "local\n", "remote\n", "base\n",
			"1 region changed differently on each side since their common ancestor is marked with the local, base, and remote versions", 1, false, true},
		{"inconsistent merge", "import \"a\"\nimport \"c\"\nimport \"b\"\n", "import \"c\"\nimport \"a\"\nimport \"b\"\n", "import \"a\"\nimport \"b\"\nimport \"c\"\n",
			"couldn't be merged reliably (such as a line that each side moved elsewhere, or changes among repeated lines), so every region where the copies differ is marked (2), without the base version: compare each side with Base", 2, false, false},
		{"invalid UTF-8", "caf\xe9\nlocal\n", "caf\xe9\nremote\n", "caf\xe9\n",
			"The copies couldn't be merged line by line, since the local copy isn't valid UTF-8 text, so the merged file is a single region with the complete local, base, and remote versions: compare each side with Base", 1, true, true},
		{"large copy", "remote\n", large, "base\n",
			"couldn't be merged line by line, since the remote copy is larger than 4 MiB", 1, true, true},
		{"many lines", lines + "local\n", lines + "remote\n", lines,
			"couldn't be merged line by line, since the copies and the base have more than 200000 lines in total", 1, true, true},
		{"invalid UTF-8 without an ancestor", "local\n", "caf\xe9\n", "",
			"No common ancestor was found, and the copies couldn't be compared line by line, since the remote copy isn't valid UTF-8 text", 1, true, false},
		{"no ancestor", "local\n", "remote\n", "",
			"No common ancestor was found, so every region where the copies differ is marked (1)", 1, false, false},
	} {
		w := newTestWorkspaceWithBase(t, &workspace{Path: "file", Token: "token", directory: t.TempDir()}, test.local, test.remote, test.base)
		result, err := describe(w, "file")
		if err != nil {
			t.Fatalf("%s: %v", test.description, err)
		} else if output := strings.Join(result.lines, "\n"); !strings.Contains(output, test.note) || result.Regions != test.regions {
			t.Errorf("%s: described with %d regions as %q", test.description, result.Regions, output)
		}
		data, err := os.ReadFile(w.file(mergedName))
		if err != nil {
			t.Fatalf("%s: %v", test.description, err)
		}
		if test.whole {
			expected := markers.local + "\n" + test.local
			if test.withBase {
				expected += markers.base + "\n" + test.base
			}
			if expected += markers.separator + "\n" + test.remote + markers.remote + "\n"; string(data) != expected {
				t.Errorf("%s: the merged file isn't a single region with the complete copies", test.description)
			}
		} else if withBase := strings.Contains(string(data), markers.base); withBase != test.withBase {
			t.Errorf("%s: base versions shown: %t: %q", test.description, withBase, data)
		}
	}
}

// cleanParts returns the parts of a merged file outside its conflicting
// regions, as lines, with an empty part before or after regions that start or
// end the file or that are adjacent.
func cleanParts(merged string, markers conflictMarkers) [][]string {
	parts := [][]string{nil}
	inside := false
	for _, line := range splitLines([]byte(merged)) {
		switch strings.TrimSuffix(line, "\n") {
		case markers.local:
			inside = true
		case markers.remote:
			inside = false
			parts = append(parts, nil)
		default:
			if !inside {
				parts[len(parts)-1] = append(parts[len(parts)-1], line)
			}
		}
	}
	return parts
}

// resolvable determines whether a result consists of the parts of a merged
// file outside its conflicting regions (see cleanParts), in order, with any
// lines in place of each region.
func resolvable(parts [][]string, result []string) bool {
	if len(parts) == 1 {
		return slices.Equal(parts[0], result)
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(first)+len(last) > len(result) || !slices.Equal(result[:len(first)], first) ||
		!slices.Equal(result[len(result)-len(last):], last) {
		return false
	}
	position, end := len(first), len(result)-len(last)
	for _, part := range parts[1 : len(parts)-1] {
		found := false
		for ; position+len(part) <= end; position++ {
			if slices.Equal(result[position:position+len(part)], part) {
				position, found = position+len(part), true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// swapSides swaps the local and remote versions of the conflicting regions in
// a merged file, which yields the result of merging with the sides swapped.
func swapSides(merged string, markers conflictMarkers) string {
	var output strings.Builder
	var versions [3]strings.Builder
	version, withBase := -1, false
	for _, line := range splitLines([]byte(merged)) {
		switch strings.TrimSuffix(line, "\n") {
		case markers.local:
			version, withBase = 0, false
			for v := range versions {
				versions[v].Reset()
			}
		case markers.base:
			version, withBase = 1, true
		case markers.separator:
			version = 2
		case markers.remote:
			output.WriteString(markers.local + "\n" + versions[2].String())
			if withBase {
				output.WriteString(markers.base + "\n" + versions[1].String())
			}
			output.WriteString(markers.separator + "\n" + versions[0].String() + markers.remote + "\n")
			version = -1
		default:
			if version < 0 {
				output.WriteString(line)
			} else {
				versions[version].WriteString(line)
			}
		}
	}
	return output.String()
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

// mergeThreeWay merges text with its common ancestor as checkout does (see
// mergeWithBase), returning the merged text and the number of marked regions.
func mergeThreeWay(base, local, remote []byte, markers conflictMarkers) (string, int) {
	merged, regions, _ := mergeWithBase(base, local, remote, markers)
	return merged, regions
}
