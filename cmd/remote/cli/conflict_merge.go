package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// maximumMergeInput bounds in-memory line comparison. Larger files retain
	// complete bodies in a single, streamed conflict block instead of
	// truncation.
	maximumMergeInput = 4 * 1024 * 1024
	// maximumMergeLines bounds the line tables used for comparison (e.g. for
	// many empty lines). The streaming fallback has no line-length restriction.
	maximumMergeLines = 200000
)

// conflictMarkers are the lines that delimit the versions in a conflicting
// region of a merged file, labeled with a workspace's token, which
// distinguishes them from similar lines in the copies themselves.
type conflictMarkers struct {
	// local starts the region and its local version.
	local string
	// base starts the base version, if any.
	base string
	// separator starts the remote version.
	separator string
	// remote ends the region.
	remote string
}

// markersFor returns the conflict markers labeled with a token.
func markersFor(token string) conflictMarkers {
	return conflictMarkers{
		local:     "<<<<<<< local " + token,
		base:      "||||||| base " + token,
		separator: "======= " + token,
		remote:    ">>>>>>> remote " + token,
	}
}

// all returns all of the markers.
func (m conflictMarkers) all() []string {
	return []string{m.local, m.base, m.separator, m.remote}
}

// writeRegion writes a conflicting region with the local version, the base
// version (if withBase is set), and the remote version. Markers must start
// lines, so a version whose last line lacks a newline receives one.
func (m conflictMarkers) writeRegion(output *strings.Builder, local, base, remote []string, withBase bool) {
	write := func(marker string, lines []string) {
		output.WriteString(marker + "\n")
		for _, line := range lines {
			output.WriteString(line)
		}
		if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
			output.WriteByte('\n')
		}
	}
	write(m.local, local)
	if withBase {
		write(m.base, base)
	}
	write(m.separator, remote)
	output.WriteString(m.remote + "\n")
}

// mergeMethod identifies how a workspace's merged file was prepared.
type mergeMethod int

const (
	// mergeUnknown indicates that the merged file wasn't prepared by this
	// process.
	mergeUnknown mergeMethod = iota
	// mergeCopied indicates copies with the same contents (differing only in
	// executability), which the merged file copies.
	mergeCopied
	// mergeBinary indicates binary copies, which receive no merged file.
	mergeBinary
	// mergeMarked indicates that every region where the copies differ is
	// marked, since their common ancestor wasn't found.
	mergeMarked
	// mergeAncestor indicates a three-way merge with the copies' common
	// ancestor (see mergeWithBase).
	mergeAncestor
	// mergeInconsistent indicates that every region where the copies differ
	// is marked, without the base version, since the three-way merge with
	// their common ancestor was inconsistent (see mergeWithBase).
	mergeInconsistent
	// mergeWhole indicates a single region containing the complete copies
	// (and their common ancestor, if found), which couldn't be compared line
	// by line.
	mergeWhole
)

// mergeOutcome describes how prepareMerged prepared a workspace's merged file,
// which checkout describes.
type mergeOutcome struct {
	// method is how the merged file was prepared.
	method mergeMethod
	// reason explains why the copies couldn't be compared line by line, for
	// mergeWhole.
	reason string
}

// prepareMerged writes the initial merged file of a workspace and records how
// it was prepared in the workspace (see mergeOutcome). Copies with the same
// contents (differing only in executability) are copied as is. Text is merged
// with the copies' common ancestor, if the workspace has it, and otherwise
// receives conflict markers around every differing region. Binary copies
// receive no merged file, since the result requires an explicit choice. The
// merged file is flushed, as the copies are (see writeLocalFile), since a
// merged file that a crash left empty or truncated would lack the markers that
// keep apply from accepting it.
func prepareMerged(w *workspace) error {
	local, opposite := w.file(localName), w.file(remoteName)
	if w.Local.Digest == w.Remote.Digest {
		w.merge = mergeOutcome{method: mergeCopied}
		return copyLocalFile(w.file(mergedName), local)
	}
	inputs := []string{local, opposite}
	if w.BaseSource != "" {
		inputs = append(inputs, w.file(baseName))
	}
	for _, path := range inputs {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		probe, err := io.ReadAll(io.LimitReader(file, binaryProbeSize))
		file.Close()
		if err != nil {
			return err
		} else if isBinary(probe) {
			w.merge = mergeOutcome{method: mergeBinary}
			return nil
		}
	}
	markers := markersFor(w.Token)
	merged, outcome, err := mergeInMemory(inputs, markers)
	if err != nil {
		return err
	} else if outcome.method == mergeWhole {
		err = writeWholeRegion(w.file(mergedName), inputs, markers)
	} else {
		err = writeLocalFile(w.file(mergedName), func(output io.Writer) error {
			_, err := io.WriteString(output, merged)
			return err
		})
	}
	if err == nil {
		w.merge = outcome
	}
	return err
}

// mergeInMemory merges the local and remote copies, and the base if specified
// as a third input, if they're valid UTF-8 and small enough to compare line by
// line. Otherwise, it returns no merged file, and an outcome with the reason
// (see mergeWhole).
func mergeInMemory(inputs []string, markers conflictMarkers) (string, mergeOutcome, error) {
	names := []string{"the local copy", "the remote copy", "the base"}
	contents := make([][]byte, len(inputs))
	lines := 0
	for i, path := range inputs {
		if info, err := os.Stat(path); err != nil {
			return "", mergeOutcome{}, err
		} else if info.Size() > maximumMergeInput {
			reason := fmt.Sprintf("%s is larger than %d MiB", names[i], maximumMergeInput>>20)
			return "", mergeOutcome{method: mergeWhole, reason: reason}, nil
		} else if contents[i], err = os.ReadFile(path); err != nil {
			return "", mergeOutcome{}, err
		} else if !utf8.Valid(contents[i]) {
			return "", mergeOutcome{method: mergeWhole, reason: names[i] + " isn't valid UTF-8 text"}, nil
		}
		lines += bytes.Count(contents[i], []byte{'\n'})
	}
	if lines > maximumMergeLines {
		subject := "the copies have"
		if len(inputs) == 3 {
			subject = "the copies and the base have"
		}
		reason := fmt.Sprintf("%s more than %d lines in total", subject, maximumMergeLines)
		return "", mergeOutcome{method: mergeWhole, reason: reason}, nil
	} else if len(contents) == 2 {
		return markedMerge(contents[0], contents[1], markers), mergeOutcome{method: mergeMarked}, nil
	} else if merged, _, threeWay := mergeWithBase(contents[2], contents[0], contents[1], markers); threeWay {
		return merged, mergeOutcome{method: mergeAncestor}, nil
	} else {
		return merged, mergeOutcome{method: mergeInconsistent}, nil
	}
}

// writeWholeRegion writes a merged file consisting of a single conflicting
// region containing the complete inputs (local, remote, and optionally base),
// streaming them, and flushes it (see writeLocalFile).
func writeWholeRegion(path string, inputs []string, markers conflictMarkers) error {
	sections := []struct{ marker, input string }{{markers.local, inputs[0]}}
	if len(inputs) == 3 {
		sections = append(sections, struct{ marker, input string }{markers.base, inputs[2]})
	}
	sections = append(sections, struct{ marker, input string }{markers.separator, inputs[1]})
	copyInput := func(output io.Writer, path string) error {
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		if _, err := io.Copy(output, input); err != nil {
			return err
		} else if missing, err := missingFinalNewline(input); err != nil || !missing {
			return err
		}
		_, err = io.WriteString(output, "\n")
		return err
	}
	return writeLocalFile(path, func(output io.Writer) error {
		for _, section := range sections {
			if _, err := fmt.Fprintln(output, section.marker); err != nil {
				return err
			} else if err := copyInput(output, section.input); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintln(output, markers.remote)
		return err
	})
}

// mergeNote describes the merged file of a workspace whose copies differ, with
// the specified number of marked regions, for checkout. Notes on merges with
// the common ancestor ask for the whole result to be reviewed, since merging
// line by line can misrepresent changes even where it marks nothing.
func mergeNote(w *workspace, regions int) string {
	method := w.merge.method
	if method == mergeUnknown && w.BaseSource != "" {
		method = mergeAncestor
	}
	switch {
	case method == mergeWhole && w.BaseSource != "":
		return fmt.Sprintf("The copies couldn't be merged line by line, since %s, so the merged file is a single region with the complete local, base, and remote versions: compare each side with Base to see what each changed, and combine both sides' changes (keeping either version whole would drop the other side's changes).", w.merge.reason)
	case method == mergeWhole:
		return fmt.Sprintf("No common ancestor was found, and the copies couldn't be compared line by line, since %s, so the merged file is a single region with both complete copies: compare them; neither side's additions or deletions are known to be newer.", w.merge.reason)
	case method == mergeInconsistent:
		return fmt.Sprintf("Both sides' changes since their common ancestor couldn't be merged reliably (such as a line that each side moved elsewhere, or changes among repeated lines), so every region where the copies differ is marked (%d), without the base version: compare each side with Base to see what each changed, and combine both sides' changes.", regions)
	case method == mergeAncestor && regions == 0:
		return "Both copies' changes since their common ancestor were merged without conflicts, but merging line by line can misplace changes: compare the merged file with Local, Remote, and Base before applying it."
	case method == mergeAncestor:
		subject := fmt.Sprintf("%d regions changed differently on each side since their common ancestor are", regions)
		if regions == 1 {
			subject = "1 region changed differently on each side since their common ancestor is"
		}
		return subject + " marked with the local, base, and remote versions: combine both sides' changes in each region, and review the rest of the merged file against Local, Remote, and Base too."
	}
	return fmt.Sprintf("No common ancestor was found, so every region where the copies differ is marked (%d), and neither side's additions or deletions are known to be newer.", regions)
}

// missingFinalNewline determines whether a file is non-empty and doesn't end
// with a newline.
func missingFinalNewline(file *os.File) (bool, error) {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return false, err
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// unterminatedNote describes the copies of a workspace that don't end with a
// newline, if any. Conflict markers must start lines, so marking a region at
// the end of such a copy adds one, which the result may need removed. Copies
// that can't be read are skipped, since the note is only a hint.
func unterminatedNote(w *workspace) string {
	var sides []string
	for _, side := range [][2]string{{localName, sideLocal}, {remoteName, sideRemote}} {
		file, err := os.Open(w.file(side[0]))
		if err != nil {
			continue
		}
		missing, err := missingFinalNewline(file)
		file.Close()
		if err == nil && missing {
			sides = append(sides, side[1])
		}
	}
	subject := "Neither copy ends"
	switch len(sides) {
	case 0:
		return ""
	case 1:
		subject = "The " + sides[0] + " copy doesn't end"
	}
	return subject + " with a newline, but conflict markers may have added one to the merged file: remove it if the result shouldn't end with one."
}

// markedMerge preserves equal runs and marks every unequal region (see
// markedSegments).
func markedMerge(a, b []byte, markers conflictMarkers) string {
	merged, _ := renderMerge(markedSegments(splitLines(a), splitLines(b)), markers, false)
	return merged
}

// maximumAnchoringDepth bounds the recursion of anchoredEdits, which bounds its
// cost for inputs whose lines only become unique in ever smaller ranges.
const maximumAnchoringDepth = 16

// longestIncreasing returns the indices of a longest strictly increasing
// subsequence of values, using patience sorting: piles[p] is the index of the
// value ending the best subsequence of length p+1 found so far, which is the
// smallest such value.
func longestIncreasing(values []int) []int {
	var piles []int
	previous := make([]int, len(values))
	for v, value := range values {
		p, _ := slices.BinarySearchFunc(piles, value, func(pile, value int) int {
			return values[pile] - value
		})
		previous[v] = -1
		if p > 0 {
			previous[v] = piles[p-1]
		}
		if p == len(piles) {
			piles = append(piles, v)
		} else {
			piles[p] = v
		}
	}
	result := make([]int, len(piles))
	if len(piles) > 0 {
		for v, p := piles[len(piles)-1], len(piles)-1; v >= 0; v, p = previous[v], p-1 {
			result[p] = v
		}
	}
	return result
}

// anchoredEdits computes edit scripts transforming base into local and into
// remote for a three-way merge. Both leave the lines that occur exactly once in
// each of the three versions unchanged, as long as they appear in the same
// order in all of them (keeping a longest such sequence), as patience diff
// does for two versions, and recurse into the lines between them, where other
// lines may occur once. Where there are no such lines, they're minimal (see
// diffLines). They aren't necessarily minimal overall, which merges don't
// require, since they compare the regions between unchanged lines by their
// contents.
//
// Minimal scripts are often ambiguous: one may keep a line unchanged by
// matching it with an equal line across a unique one, such as a delimiter,
// deleting and inserting that one instead, as if the line had moved, while
// another, equally short, keeps the delimiter. Scripts computed separately for
// each side may resolve such ambiguities differently, depending on the changes
// elsewhere, and a merge would then see a change made identically on both
// sides as different changes, and apply it twice, or apply a deletion to a
// line that one side inserted. Lines that are unique in all three versions are
// matched the same way in both scripts, regardless of the changes around them,
// and they divide the versions into the same ranges for both sides. (Lines
// unique in base and in one side only, such as a line that the other side
// duplicated, would be matched differently in the other side's script.)
func anchoredEdits(base, local, remote []string, depth int) ([]edit, []edit) {
	if depth >= maximumAnchoringDepth {
		return diffLines(base, local), diffLines(base, remote)
	}
	type occurrences struct {
		base, local, remote int
		// at holds the positions in local and remote.
		at [2]int
	}
	counts := make(map[string]*occurrences, len(base))
	for _, line := range base {
		if c := counts[line]; c != nil {
			c.base++
		} else {
			counts[line] = &occurrences{base: 1}
		}
	}
	for side, lines := range [][]string{local, remote} {
		for j, line := range lines {
			if c := counts[line]; c != nil {
				if side == 0 {
					c.local++
				} else {
					c.remote++
				}
				c.at[side] = j
			}
		}
	}

	// Find unique lines in the same order in all three versions. A longest
	// such sequence is approximated by a longest sequence in the same order in
	// base and one side, and of those, a longest sequence in the same order in
	// the other side too, starting with whichever side yields more. (Lines
	// that moved differently on each side may otherwise displace others, which
	// are then ordered differently in the other side.)
	var candidates [][3]int
	for i, line := range base {
		if c := counts[line]; c.base == 1 && c.local == 1 && c.remote == 1 {
			candidates = append(candidates, [3]int{i, c.at[0], c.at[1]})
		}
	}
	ordered := func(candidates [][3]int, side int) [][3]int {
		positions := make([]int, len(candidates))
		for c, candidate := range candidates {
			positions[c] = candidate[side]
		}
		var result [][3]int
		for _, c := range longestIncreasing(positions) {
			result = append(result, candidates[c])
		}
		return result
	}
	anchors := ordered(ordered(candidates, 1), 2)
	if alternative := ordered(ordered(candidates, 2), 1); len(alternative) > len(anchors) {
		anchors = alternative
	}
	if len(anchors) == 0 {
		return diffLines(base, local), diffLines(base, remote)
	}

	// Compute the scripts between the anchors.
	var localScript, remoteScript []edit
	i, j, k := 0, 0, 0
	for _, anchor := range append(anchors, [3]int{len(base), len(local), len(remote)}) {
		localGap, remoteGap := anchoredEdits(base[i:anchor[0]], local[j:anchor[1]], remote[k:anchor[2]], depth+1)
		localScript, remoteScript = append(localScript, localGap...), append(remoteScript, remoteGap...)
		if anchor[0] < len(base) {
			localScript = append(localScript, edit{' ', base[anchor[0]]})
			remoteScript = append(remoteScript, edit{' ', base[anchor[0]]})
		}
		i, j, k = anchor[0]+1, anchor[1]+1, anchor[2]+1
	}
	return localScript, remoteScript
}

// embedLeftmost determines which lines of a sequence are changed if a
// subsequence of it, its unchanged lines, is matched as early as possible.
func embedLeftmost(lines, unchanged []string) []bool {
	changed := make([]bool, len(lines))
	k := 0
	for i, line := range lines {
		if k < len(unchanged) && line == unchanged[k] {
			k++
		} else {
			changed[i] = true
		}
	}
	return changed
}

// canonicalEdits returns an edit script transforming base into other that
// leaves lines with the same contents unchanged as a script does, but
// positions every changed line canonically. A change adjacent to equal lines,
// such as a line inserted next to an equal line, has several equivalent
// positions, and scripts computed for different versions may choose different
// ones, depending on the changes elsewhere (diffLines, for example, keeps
// unchanged leading and trailing lines first). A three-way merge compares two
// scripts' positions, so it would see an identical change made on both sides
// at different positions as two changes, and apply it twice, without marking a
// conflict. The unchanged lines are thus matched as early as possible in both
// base and other, which moves each changed line as late as possible and
// depends only on the lines around it, while leaving the unchanged lines'
// contents, and thus their correspondence, as they were. (Git moves runs of
// changes as late as possible too, but additionally prefers positions where
// deletions and insertions are adjacent, which depends on the other changes in
// the script, and thus differs between the sides.) Deletions and insertions
// thus move separately, which can separate a replacement next to equal lines
// into an insertion and the deletion of another line, misrepresenting it
// (see unambiguousSegments).
func canonicalEdits(base, other []string, script []edit) []edit {
	var unchanged []string
	for _, e := range script {
		if e.operation == ' ' {
			unchanged = append(unchanged, e.line)
		}
	}
	baseChanged, otherChanged := embedLeftmost(base, unchanged), embedLeftmost(other, unchanged)

	// Rebuild the script, pairing the unchanged lines in order.
	script = make([]edit, 0, len(script))
	for i, j := 0, 0; i < len(base) || j < len(other); {
		if i < len(base) && baseChanged[i] {
			script = append(script, edit{'-', base[i]})
			i++
		} else if j < len(other) && otherChanged[j] {
			script = append(script, edit{'+', other[j]})
			j++
		} else {
			script = append(script, edit{' ', base[i]})
			i, j = i+1, j+1
		}
	}
	return script
}

// matchLines maps each line of base to the line of the other version that an
// edit script transforming base into it leaves the line unchanged as, or to -1
// if it's changed. Matches increase monotonically.
func matchLines(base []string, script []edit) []int {
	matches := make([]int, len(base))
	i, j := 0, 0
	for _, edit := range script {
		switch edit.operation {
		case ' ':
			matches[i] = j
			i, j = i+1, j+1
		case '-':
			matches[i] = -1
			i++
		case '+':
			j++
		}
	}
	return matches
}

// mergeSegment is a part of a merge, which consists of consecutive lines of
// each version, such as a line unchanged on both sides of a three-way merge,
// or a region between such lines.
type mergeSegment struct {
	// base, local, and remote are the segment's lines in each version.
	base, local, remote []string
	// merged are the segment's merged lines, unless it conflicts.
	merged []string
	// conflict indicates that the segment must be resolved manually.
	conflict bool
}

// resolve merges a segment of a three-way merge as diff3 merges a region: it
// takes the changed side's version if only one side changed it, and either
// version if both changed it identically. Segments changed differently on
// each side conflict.
func (s *mergeSegment) resolve() {
	switch {
	case slices.Equal(s.local, s.base):
		s.merged = s.remote
	case slices.Equal(s.remote, s.base) || slices.Equal(s.local, s.remote):
		s.merged = s.local
	default:
		s.conflict = true
	}
}

// mergeSegments merges the local and remote versions of text with their
// common ancestor line by line, as diff3 does: lines unchanged on both sides
// delimit regions, which are resolved separately (see mergeSegment.resolve).
//
// A merge is only as good as each side's alignment with the ancestor: a change
// that both sides made identically must be aligned identically, since it would
// otherwise be seen as two changes and applied twice (or, for a deletion,
// delete more than either side did) without marking a conflict. The edit
// scripts are thus anchored at the same lines (see anchoredEdits), and their
// changes are positioned canonically (see canonicalEdits). Alignments can
// still misrepresent changes where lines repeat, however, which
// unambiguousSegments detects.
func mergeSegments(b, l, r []string) []mergeSegment {
	localScript, remoteScript := anchoredEdits(b, l, r, 0)
	localMatches := matchLines(b, canonicalEdits(b, l, localScript))
	remoteMatches := matchLines(b, canonicalEdits(b, r, remoteScript))
	var segments []mergeSegment
	i, j, k := 0, 0, 0
	for i < len(b) || j < len(l) || k < len(r) {
		if i < len(b) && localMatches[i] == j && remoteMatches[i] == k {
			line := b[i : i+1]
			segments = append(segments, mergeSegment{base: line, local: line, remote: line, merged: line})
			i, j, k = i+1, j+1, k+1
			continue
		}

		// The region ends at the next base line that's unchanged on both sides,
		// or at the end of every version.
		end := i
		for end < len(b) && (localMatches[end] < 0 || remoteMatches[end] < 0) {
			end++
		}
		localEnd, remoteEnd := len(l), len(r)
		if end < len(b) {
			localEnd, remoteEnd = localMatches[end], remoteMatches[end]
		}
		segment := mergeSegment{base: b[i:end], local: l[j:localEnd], remote: r[k:remoteEnd]}
		segment.resolve()
		segments = append(segments, segment)
		i, j, k = end, localEnd, remoteEnd
	}
	return segments
}

// reversedLines returns a copy of lines in reverse order.
func reversedLines(lines []string) []string {
	result := make([]string, len(lines))
	for i, line := range lines {
		result[len(lines)-1-i] = line
	}
	return result
}

// mirroredSegments merges the versions with their lines in reverse order (see
// mergeSegments) and returns the segments in the original order, with their
// lines in the original order. Changes are thus positioned canonically in the
// opposite direction, as early as possible rather than as late as possible
// (see canonicalEdits), and where several sequences of unique lines could
// anchor the edit scripts, another may be chosen (see anchoredEdits).
func mirroredSegments(b, l, r []string) []mergeSegment {
	segments := mergeSegments(reversedLines(b), reversedLines(l), reversedLines(r))
	slices.Reverse(segments)
	for s := range segments {
		segment := &segments[s]
		segment.base, segment.local = reversedLines(segment.base), reversedLines(segment.local)
		segment.remote, segment.merged = reversedLines(segment.remote), reversedLines(segment.merged)
	}
	return segments
}

// sameMerge determines whether two sequences of merge segments have the same
// result: the same merged lines, and the same conflicting regions in the same
// places.
func sameMerge(a, b []mergeSegment) bool {
	var aLines, bLines []string
	for i, j := 0, 0; ; i, j = i+1, j+1 {
		aLines, bLines = aLines[:0], bLines[:0]
		for ; i < len(a) && !a[i].conflict; i++ {
			aLines = append(aLines, a[i].merged...)
		}
		for ; j < len(b) && !b[j].conflict; j++ {
			bLines = append(bLines, b[j].merged...)
		}
		if !slices.Equal(aLines, bLines) {
			return false
		} else if i == len(a) || j == len(b) {
			return i == len(a) && j == len(b)
		} else if !slices.Equal(a[i].local, b[j].local) || !slices.Equal(a[i].base, b[j].base) ||
			!slices.Equal(a[i].remote, b[j].remote) {
			return false
		}
	}
}

// mergePosition is a position in each version of a three-way merge.
type mergePosition struct {
	base, local, remote int
}

// after returns the position following a segment that starts at a position.
func (p mergePosition) after(segment mergeSegment) mergePosition {
	return mergePosition{p.base + len(segment.base), p.local + len(segment.local), p.remote + len(segment.remote)}
}

// unambiguousSegments merges the local and remote versions of text with their
// common ancestor line by line, as mergeSegments does, except where the result
// depends on how the changes are aligned with the ancestor, which it marks.
//
// Where lines repeat, changes have several equivalent positions, and the
// positions that mergeSegments chooses for each side can misrepresent a change
// as another, such as a replacement as an insertion and the deletion of an
// equal line elsewhere, which coincides with a deletion on the other side, so
// that the merge drops the other side's deletion. The versions are thus also
// merged in the opposite direction (see mirroredSegments), which positions
// changes differently wherever their positions are ambiguous. Positions where
// both merges have segment boundaries, which both merges align the same way,
// divide the versions into parts. Each part takes the forward merge's segments
// if both merges agree on its result, and is otherwise merged as a whole, as
// diff3 merges a region (see mergeSegment.resolve), which usually marks it,
// with all three versions, so that no change is lost.
func unambiguousSegments(b, l, r []string) []mergeSegment {
	// Index the positions of the forward merge's segment boundaries.
	forward, mirrored := mergeSegments(b, l, r), mirroredSegments(b, l, r)
	boundaries := make(map[mergePosition]int, len(forward)+1)
	var position mergePosition
	boundaries[position] = 0
	for s, segment := range forward {
		position = position.after(segment)
		boundaries[position] = s + 1
	}

	// Compare the merges' results between the boundaries that they share,
	// which occur in the same order in both, including the end of every
	// version.
	var segments []mergeSegment
	var start, end mergePosition
	f, m := 0, 0
	for s, segment := range mirrored {
		end = end.after(segment)
		next, shared := boundaries[end]
		if !shared {
			continue
		} else if sameMerge(forward[f:next], mirrored[m:s+1]) {
			segments = append(segments, forward[f:next]...)
		} else {
			part := mergeSegment{
				base:   b[start.base:end.base],
				local:  l[start.local:end.local],
				remote: r[start.remote:end.remote],
			}
			part.resolve()
			segments = append(segments, part)
		}
		start, f, m = end, next, s+1
	}
	return segments
}

// consistentRange computes the range of the occurrences of a line in a merge
// that applies each side's changes once, given its occurrences in the base,
// local, and remote versions: between as many as each side has, and as many
// as applying both sides' changes separately yields (as many as both sides
// have, minus the base's), which changes made identically on both sides
// reduce.
func consistentRange(occurrences [3]int) (int, int) {
	separate := occurrences[1] + occurrences[2] - occurrences[0]
	return min(occurrences[1], occurrences[2], separate), max(occurrences[1], occurrences[2], separate)
}

// consistentCounts determines whether the number of occurrences of each line
// in a three-way merge is consistent with applying each side's changes once
// (see consistentRange). Merges that keep a line twice because each side moved
// it somewhere else violate it, as do some misaligned changes that merging in
// both directions agrees on, but changes dropped or applied twice within the
// range aren't detected, which unambiguousSegments must do. Conflicting
// regions have yet to be resolved, so each can add as many occurrences as
// resolving it by applying both sides' changes in it once does, and the total
// only needs to be consistent for some such resolution.
func consistentCounts(b, l, r []string, segments []mergeSegment) bool {
	type occurrences struct {
		// versions are the occurrences in the base, local, and remote
		// versions.
		versions [3]int
		// minimum and maximum bound the occurrences in the merge.
		minimum, maximum int
	}
	lines := make(map[string]*occurrences)
	count := func(line string) *occurrences {
		o := lines[line]
		if o == nil {
			o = &occurrences{}
			lines[line] = o
		}
		return o
	}
	for v, version := range [][]string{b, l, r} {
		for _, line := range version {
			count(line).versions[v]++
		}
	}
	region := make(map[string]*[3]int)
	for _, segment := range segments {
		if !segment.conflict {
			for _, line := range segment.merged {
				o := count(line)
				o.minimum, o.maximum = o.minimum+1, o.maximum+1
			}
			continue
		}
		clear(region)
		for v, version := range [][]string{segment.base, segment.local, segment.remote} {
			for _, line := range version {
				if region[line] == nil {
					region[line] = new([3]int)
				}
				region[line][v]++
			}
		}
		for line, versions := range region {
			o := count(line)
			minimum, maximum := consistentRange(*versions)
			o.minimum, o.maximum = o.minimum+minimum, o.maximum+maximum
		}
	}
	for _, o := range lines {
		if minimum, maximum := consistentRange(o.versions); o.maximum < minimum || o.minimum > maximum {
			return false
		}
	}
	return true
}

// markedSegments merges two versions without a common ancestor, preserving
// equal runs and marking every unequal region, since neither additions nor
// deletions are known to be newer.
func markedSegments(a, b []string) []mergeSegment {
	script := diffLines(a, b)
	var segments []mergeSegment
	for i := 0; i < len(script); {
		var segment mergeSegment
		if script[i].operation == ' ' {
			for ; i < len(script) && script[i].operation == ' '; i++ {
				segment.merged = append(segment.merged, script[i].line)
			}
		} else {
			segment.conflict = true
			for ; i < len(script) && script[i].operation != ' '; i++ {
				if script[i].operation == '-' {
					segment.local = append(segment.local, script[i].line)
				} else {
					segment.remote = append(segment.remote, script[i].line)
				}
			}
		}
		segments = append(segments, segment)
	}
	return segments
}

// renderMerge formats merge segments as text, marking conflicting segments
// with the local version, the base version (if withBase is set), and the
// remote version, and returns the text and the number of marked regions.
func renderMerge(segments []mergeSegment, markers conflictMarkers, withBase bool) (string, int) {
	var output strings.Builder
	regions := 0
	for _, segment := range segments {
		if segment.conflict {
			regions++
			markers.writeRegion(&output, segment.local, segment.base, segment.remote, withBase)
			continue
		}
		for _, line := range segment.merged {
			output.WriteString(line)
		}
	}
	return output.String(), regions
}

// mergeWithBase merges the local and remote versions of text with their
// common ancestor line by line (see unambiguousSegments), returning the merged
// text, the number of marked regions, and whether the merge is three-way. A
// merge whose line counts are inconsistent (see consistentCounts) misrepresents
// some change, which may be anywhere, so every region where the local and
// remote versions differ is marked instead, without the base version, as
// markedMerge does.
func mergeWithBase(base, local, remote []byte, markers conflictMarkers) (string, int, bool) {
	b, l, r := splitLines(base), splitLines(local), splitLines(remote)
	if segments := unambiguousSegments(b, l, r); consistentCounts(b, l, r, segments) {
		merged, regions := renderMerge(segments, markers, true)
		return merged, regions, true
	}
	merged, regions := renderMerge(markedSegments(l, r), markers, false)
	return merged, regions, false
}

// countMarkers counts the occurrences of a marker in a file using bounded
// buffers, including for arbitrarily long lines.
func countMarkers(path, marker string) (int, error) {
	counts, err := scanMarkers(path, []string{marker})
	if err != nil {
		return 0, err
	}
	return counts[0], nil
}

// containsMarkers determines whether a file contains any of a workspace's
// conflict markers.
func containsMarkers(path, token string) (bool, error) {
	counts, err := scanMarkers(path, markersFor(token).all())
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(counts, func(count int) bool { return count > 0 }), nil
}

// scanMarkers counts the occurrences of markers in a file using bounded
// buffers. Each read retains the end of the previous one, so that markers
// crossing reads are found, and occurrences are counted once, when they end in
// newly read data.
func scanMarkers(path string, markers []string) ([]int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	longest := 0
	for _, marker := range markers {
		longest = max(longest, len(marker))
	}
	counts := make([]int, len(markers))
	buffer := make([]byte, 64*1024+longest)
	retained := 0
	for {
		count, err := file.Read(buffer[retained:])
		data := buffer[:retained+count]
		for m, marker := range markers {
			for offset := 0; ; {
				index := bytes.Index(data[offset:], []byte(marker))
				if index < 0 {
					break
				} else if offset+index+len(marker) > retained {
					counts[m]++
				}
				offset += index + 1
			}
		}
		if err == io.EOF {
			return counts, nil
		} else if err != nil {
			return nil, err
		}
		retained = min(len(data), longest-1)
		copy(buffer, data[len(data)-retained:])
	}
}
