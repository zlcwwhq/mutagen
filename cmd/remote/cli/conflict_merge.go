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

// matchLines maps each line of base to the line of other that a minimal edit
// script leaves it unchanged as, or to -1 if it's changed. Matches increase
// monotonically.
func matchLines(base, other []string) []int {
	matches := make([]int, len(base))
	i, j := 0, 0
	for _, edit := range diffLines(base, other) {
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
func mergeThreeWay(base, local, remote []byte, markers conflictMarkers) (string, int) {
	b, l, r := splitLines(base), splitLines(local), splitLines(remote)
	localMatches, remoteMatches := matchLines(b, l), matchLines(b, r)
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
