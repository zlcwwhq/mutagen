package cli

import (
	"bytes"
	"errors"
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

// prepareMerged writes the initial merged file of a workspace. Copies with the
// same contents (differing only in executability) are copied as is. Text is
// merged with the copies' common ancestor, if the workspace has it, and
// otherwise receives conflict markers around every differing region. Binary
// copies receive no merged file, since the result requires an explicit choice.
func prepareMerged(w *workspace) error {
	local, opposite := w.file(localName), w.file(remoteName)
	if w.Local.Digest == w.Remote.Digest {
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
			return nil
		}
	}
	markers := markersFor(w.Token)
	if merged, ok, err := mergeInMemory(inputs, markers); err != nil {
		return err
	} else if ok {
		return os.WriteFile(w.file(mergedName), []byte(merged), 0600)
	}
	return writeWholeRegion(w.file(mergedName), inputs, markers)
}

// mergeInMemory merges the local and remote copies, and the base if specified
// as a third input, if they're small enough to compare line by line and valid
// UTF-8.
func mergeInMemory(inputs []string, markers conflictMarkers) (string, bool, error) {
	contents := make([][]byte, len(inputs))
	lines := 0
	for i, path := range inputs {
		if info, err := os.Stat(path); err != nil {
			return "", false, err
		} else if info.Size() > maximumMergeInput {
			return "", false, nil
		} else if contents[i], err = os.ReadFile(path); err != nil {
			return "", false, err
		} else if !utf8.Valid(contents[i]) {
			return "", false, nil
		}
		lines += bytes.Count(contents[i], []byte{'\n'})
	}
	if lines > maximumMergeLines {
		return "", false, nil
	} else if len(contents) == 3 {
		merged, _ := mergeThreeWay(contents[2], contents[0], contents[1], markers)
		return merged, true, nil
	}
	return markedMerge(contents[0], contents[1], markers), true, nil
}

// writeWholeRegion writes a merged file consisting of a single conflicting
// region containing the complete inputs (local, remote, and optionally base),
// streaming them.
func writeWholeRegion(path string, inputs []string, markers conflictMarkers) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	sections := []struct{ marker, input string }{{markers.local, inputs[0]}}
	if len(inputs) == 3 {
		sections = append(sections, struct{ marker, input string }{markers.base, inputs[2]})
	}
	sections = append(sections, struct{ marker, input string }{markers.separator, inputs[1]})
	for _, section := range sections {
		if _, err = fmt.Fprintln(file, section.marker); err != nil {
			break
		}
		input, openErr := os.Open(section.input)
		if openErr != nil {
			err = openErr
			break
		}
		_, err = io.Copy(file, input)
		if err == nil {
			var missing bool
			if missing, err = missingFinalNewline(input); err == nil && missing {
				_, err = file.WriteString("\n")
			}
		}
		input.Close()
		if err != nil {
			break
		}
	}
	if err == nil {
		_, err = fmt.Fprintln(file, markers.remote)
	}
	return errors.Join(err, file.Close())
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

// markedMerge preserves equal runs and marks every unequal region. With no
// historical baseline, neither additions nor deletions are assumed correct.
func markedMerge(a, b []byte, markers conflictMarkers) string {
	script := diffLines(splitLines(a), splitLines(b))
	var output strings.Builder
	for i := 0; i < len(script); {
		if script[i].operation == ' ' {
			output.WriteString(script[i].line)
			i++
			continue
		}
		end := i
		for end < len(script) && script[end].operation != ' ' {
			end++
		}
		var local, remote []string
		for _, edit := range script[i:end] {
			if edit.operation == '-' {
				local = append(local, edit.line)
			} else {
				remote = append(remote, edit.line)
			}
		}
		markers.writeRegion(&output, local, nil, remote, false)
		i = end
	}
	return output.String()
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
// the script, and thus differs between the sides.)
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

// mergeThreeWay merges the local and remote versions of text with their common
// ancestor line by line, as diff3 does. Lines unchanged on both sides delimit
// regions, which take the changed side's version if only one side changed
// them, and either version if both changed them identically. Regions changed
// differently on each side are marked, showing all three versions. It returns
// the merged text and the number of marked regions.
//
// A merge is only as good as each side's alignment with the ancestor: a change
// that both sides made identically must be aligned identically, since it would
// otherwise be seen as two changes and applied twice (or, for a deletion,
// delete more than either side did) without marking a conflict. The edit
// scripts are thus anchored at the same lines (see anchoredEdits), and their
// changes are positioned canonically (see canonicalEdits).
func mergeThreeWay(base, local, remote []byte, markers conflictMarkers) (string, int) {
	b, l, r := splitLines(base), splitLines(local), splitLines(remote)
	localScript, remoteScript := anchoredEdits(b, l, r, 0)
	localMatches := matchLines(b, canonicalEdits(b, l, localScript))
	remoteMatches := matchLines(b, canonicalEdits(b, r, remoteScript))
	var output strings.Builder
	regions := 0
	i, j, k := 0, 0, 0
	for i < len(b) || j < len(l) || k < len(r) {
		if i < len(b) && localMatches[i] == j && remoteMatches[i] == k {
			output.WriteString(b[i])
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
		baseRegion, localRegion, remoteRegion := b[i:end], l[j:localEnd], r[k:remoteEnd]
		switch {
		case slices.Equal(localRegion, baseRegion):
			output.WriteString(strings.Join(remoteRegion, ""))
		case slices.Equal(remoteRegion, baseRegion) || slices.Equal(localRegion, remoteRegion):
			output.WriteString(strings.Join(localRegion, ""))
		default:
			regions++
			markers.writeRegion(&output, localRegion, baseRegion, remoteRegion, true)
		}
		i, j, k = end, localEnd, remoteEnd
	}
	return output.String(), regions
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
