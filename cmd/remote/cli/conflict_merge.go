package cli

import (
	"bytes"
	"fmt"
	"io"
	"maps"
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
		return fmt.Sprintf("Both sides' changes since their common ancestor couldn't be merged reliably (such as a line that each side moved elsewhere), so every region where the copies differ is marked (%d), without the base version: compare each side with Base to see what each changed, and combine both sides' changes.", regions)
	case method == mergeAncestor && regions == 0:
		return "Both copies' changes since their common ancestor were merged without conflicts, but merging line by line can misplace changes, especially repeated lines (such as blank lines or closing braces), moved lines, and changes made on both sides: compare the merged file with Local, Remote, and Base before applying it."
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

// maximumAnchoringDepth bounds the recursion of mergeSegments, which bounds
// its cost for inputs whose lines only become unique in ever smaller ranges.
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

// uniqueAnchors returns the positions in base, local, and remote of lines that
// occur exactly once in each of the three versions, as long as they appear in
// the same order in all of them, keeping a longest such sequence, as patience
// diff does for two versions. Such lines correspond unambiguously.
func uniqueAnchors(base, local, remote []string) [][3]int {
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

	// A longest sequence in the same order in all three versions is
	// approximated by a longest sequence in the same order in base and one
	// side, and of those, a longest sequence in the same order in the other
	// side too, starting with whichever side yields more. (Lines that moved
	// differently on each side may otherwise displace others, which are then
	// ordered differently in the other side.)
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
	return anchors
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
// common ancestor line by line. Lines that occur once in each version, in the
// same order in all of them (see uniqueAnchors), are unchanged on both sides,
// and divide the versions into regions, which are merged the same way, since
// other lines may occur once in them. A region without such lines is merged as
// a whole, as diff3 merges a region (see mergeSegment.resolve), so that both
// sides' changes in it are marked together unless only one side changed it, or
// both changed it identically.
//
// Lines in such a region repeat (or are missing from some version), so each
// side's changes there can be aligned with the ancestor in several ways, and
// alignments computed separately for each side can misrepresent changes in
// ways that merging them line by line wouldn't reveal: a change made
// identically on both sides can be aligned differently on each, and applied
// twice, or a replacement can be aligned as the insertion of a line and the
// deletion of another, equal one, which coincides with a deletion on the other
// side, so that the merge drops it. Lines that occur once in each version
// divide the versions into the same regions for both sides, and merging each
// region as a whole loses nothing: a marked region contains everything that
// each version has there. Both sides' changes separated only by repeated lines
// are thus marked together, as are both sides' changes in ranges that recursion
// doesn't reach (see maximumAnchoringDepth).
func mergeSegments(base, local, remote []string, depth int) []mergeSegment {
	var anchors [][3]int
	if depth < maximumAnchoringDepth {
		anchors = uniqueAnchors(base, local, remote)
	}
	if len(anchors) == 0 {
		if len(base) == 0 && len(local) == 0 && len(remote) == 0 {
			return nil
		}
		region := mergeSegment{base: base, local: local, remote: remote}
		region.resolve()
		return []mergeSegment{region}
	}
	var segments []mergeSegment
	i, j, k := 0, 0, 0
	for _, anchor := range append(anchors, [3]int{len(base), len(local), len(remote)}) {
		segments = append(segments, mergeSegments(base[i:anchor[0]], local[j:anchor[1]], remote[k:anchor[2]], depth+1)...)
		if anchor[0] < len(base) {
			line := base[anchor[0] : anchor[0]+1]
			segments = append(segments, mergeSegment{base: line, local: line, remote: line, merged: line})
		}
		i, j, k = anchor[0]+1, anchor[1]+1, anchor[2]+1
	}
	return segments
}

// moveKeys returns keys identifying the lines that a version of a segment
// removes from its base version and the lines that it adds, which the segments
// at either end of a move share (see markAmbiguousMoves): one for all of them,
// regardless of their order, which is equal for versions that remove (or add)
// the same lines as many times, and one for each of them that occurs once in
// the whole base version.
func moveKeys(base, version []string, unique map[string]bool) (removed, added []string) {
	if slices.Equal(base, version) {
		return nil, nil
	}
	counts := make(map[string]int)
	for _, line := range base {
		counts[line]++
	}
	for _, line := range version {
		counts[line]--
	}
	var removals, additions strings.Builder
	for _, line := range slices.Sorted(maps.Keys(counts)) {
		lines, keys, count := &removals, &removed, counts[line]
		if count < 0 {
			lines, keys, count = &additions, &added, -count
		}
		if count > 0 && unique[line] {
			*keys = append(*keys, "line "+line)
		}
		for ; count > 0; count-- {
			fmt.Fprintf(lines, "%d:%s", len(line), line)
		}
	}
	if removals.Len() > 0 {
		removed = append(removed, "lines "+removals.String())
	}
	if additions.Len() > 0 {
		added = append(added, "lines "+additions.String())
	}
	return removed, added
}

// markAmbiguousMoves marks the parts of a merge of text with its common
// ancestor, base, where lines that one side moved coincide with an identical
// change on the other side at only one end of the move, from one end to the
// other, as a whole, with all three versions (see mergeSegment.resolve).
//
// Regions are merged separately, so lines that one side moved, which one
// region loses and another gains, are merged as separate changes. If the other
// side made only one of them, such as adding the same lines where the first
// side moved them, or deleting them where they were, then that change is
// merged as one made identically on both sides, which conceals the other
// side's intent: it may have copied the lines, or moved them too and added
// others where they were, and it may have deleted the lines, or moved them
// elsewhere. The merge would keep the lines once, dropping the other side's
// addition, or keep the lines that it deleted, even if the other end of the
// move is in a marked region, which needn't reveal the move. Lines that one
// side moved while the other side left both ends unchanged, or made the same
// move, are merged as usual. Moves are recognized by regions removing and
// adding the same lines, or a line that occurs once in the base version.
// (Lines that repeat, such as closing braces or empty lines, are removed and
// added separately too often to recognize moves of them individually, so the
// merge can still misrepresent those.)
func markAmbiguousMoves(base []string, segments []mergeSegment) []mergeSegment {
	occurrences := make(map[string]int, len(base))
	for _, line := range base {
		occurrences[line]++
	}
	unique := make(map[string]bool, len(occurrences))
	for line, count := range occurrences {
		unique[line] = count == 1
	}

	// Find the extent of the regions that remove and that add the lines that
	// each key identifies, with separate extents for changes that one side
	// made ([0]) and that both sides made ([1]).
	type extent struct {
		first, last int
		found       bool
	}
	removing, adding := make(map[string]*[2]extent), make(map[string]*[2]extent)
	include := func(extents map[string]*[2]extent, local, remote []string, s int) {
		sides := make(map[string]int, len(local)+len(remote))
		for _, key := range local {
			sides[key]++
		}
		for _, key := range remote {
			sides[key]++
		}
		for key, count := range sides {
			if extents[key] == nil {
				extents[key] = new([2]extent)
			}
			if e := &extents[key][count-1]; e.found {
				e.last = s
			} else {
				*e = extent{s, s, true}
			}
		}
	}
	for s := range segments {
		segment := &segments[s]
		localRemoved, localAdded := moveKeys(segment.base, segment.local, unique)
		remoteRemoved, remoteAdded := moveKeys(segment.base, segment.remote, unique)
		include(removing, localRemoved, remoteRemoved, s)
		include(adding, localAdded, remoteAdded, s)
	}

	// Mark the parts from each end of such moves to the other. A part spans
	// every region that removes the lines that a key identifies and every
	// region that adds them, since each pair of them may be the ends of a move.
	var marked []extent
	for key, removals := range removing {
		if additions := adding[key]; additions != nil {
			for both := range 2 {
				if removal, addition := removals[both], additions[1-both]; removal.found && addition.found {
					marked = append(marked, extent{min(removal.first, addition.first), max(removal.last, addition.last), true})
				}
			}
		}
	}
	if len(marked) == 0 {
		return segments
	}
	slices.SortFunc(marked, func(a, b extent) int { return a.first - b.first })
	var result []mergeSegment
	next := 0
	for m := 0; m < len(marked); {
		first, last := marked[m].first, marked[m].last
		for m++; m < len(marked) && marked[m].first <= last; m++ {
			last = max(last, marked[m].last)
		}
		var part mergeSegment
		for _, segment := range segments[first : last+1] {
			part.base = append(part.base, segment.base...)
			part.local = append(part.local, segment.local...)
			part.remote = append(part.remote, segment.remote...)
		}
		part.resolve()
		result = append(append(result, segments[next:first]...), part)
		next = last + 1
	}
	return append(result, segments[next:]...)
}

// consistentRange computes the range of the occurrences of a line in a merge
// that applies each side's changes once, given its occurrences in the base,
// local, and remote versions: between as many as each side has, and as many
// as applying both sides' changes separately yields (as many as both sides
// have, minus the base's, but never fewer than none), which changes made
// identically on both sides reduce. (A conflicting region whose sides both
// delete a line that occurs elsewhere in the merge thus can't excuse those
// occurrences.)
func consistentRange(occurrences [3]int) (int, int) {
	separate := max(0, occurrences[1]+occurrences[2]-occurrences[0])
	return min(occurrences[1], occurrences[2], separate), max(occurrences[1], occurrences[2], separate)
}

// consistentCounts determines whether the number of occurrences of each line
// in a three-way merge is consistent with applying each side's changes once
// (see consistentRange). Merges that keep a line twice because each side moved
// it somewhere else violate it, but changes dropped or applied twice within
// the range aren't detected, which mergeSegments and markAmbiguousMoves must
// avoid. Conflicting
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
// common ancestor line by line (see mergeSegments and markAmbiguousMoves),
// returning the merged text, the number of marked regions, and whether the
// merge is three-way. A
// merge whose line counts are inconsistent (see consistentCounts) misrepresents
// some change, which may be anywhere, so every region where the local and
// remote versions differ is marked instead, without the base version, as
// markedMerge does.
func mergeWithBase(base, local, remote []byte, markers conflictMarkers) (string, int, bool) {
	b, l, r := splitLines(base), splitLines(local), splitLines(remote)
	if segments := markAmbiguousMoves(b, mergeSegments(b, l, r, 0)); consistentCounts(b, l, r, segments) {
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
