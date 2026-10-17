package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// maximumMergeInput bounds in-memory line comparison. Larger files retain
// complete bodies in a single, streamed conflict block instead of truncation.
const maximumMergeInput = 4 * 1024 * 1024

// markerLines returns the conflict markers labeled with a workspace's token,
// which distinguishes them from similar lines in the copies themselves.
func markerLines(token string) [3]string {
	return [3]string{"<<<<<<< local " + token, "======= " + token, ">>>>>>> remote " + token}
}

// prepareMerged writes the initial merged file of a workspace. Copies with the
// same contents (differing only in executability) are copied as is, and text
// receives conflict markers around differing regions. Binary copies receive no
// merged file, since the result requires an explicit choice.
func prepareMerged(w *workspace) error {
	local, opposite := w.file(localName), w.file(remoteName)
	if w.Local.Digest == w.Remote.Digest {
		return copyLocalFile(w.file(mergedName), local)
	}
	for _, path := range []string{local, opposite} {
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
	if w.Local.Size <= maximumMergeInput && w.Remote.Size <= maximumMergeInput {
		a, err := os.ReadFile(local)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(opposite)
		if err != nil {
			return err
		}
		if !utf8.Valid(a) || !utf8.Valid(b) {
			return nil
		}
		// Bound the line table as well as the byte buffers (e.g. many empty
		// lines). The streaming fallback has no line-length restriction.
		if bytes.Count(a, []byte{'\n'})+bytes.Count(b, []byte{'\n'}) <= 200000 {
			return os.WriteFile(w.file(mergedName), []byte(markedMerge(a, b, w.Token)), 0600)
		}
	}
	file, err := os.OpenFile(w.file(mergedName), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	markers := markerLines(w.Token)
	_, err = fmt.Fprintln(file, markers[0])
	for side, path := range []string{local, opposite} {
		if err != nil {
			break
		}
		input, openErr := os.Open(path)
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
		if err == nil {
			_, err = fmt.Fprintln(file, markers[side+1])
		}
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
	for _, side := range [][2]string{{localName, "local"}, {remoteName, "remote"}} {
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
func markedMerge(a, b []byte, token string) string {
	script := diffLines(splitLines(a), splitLines(b))
	markers := markerLines(token)
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
		output.WriteString(markers[0] + "\n")
		for side, operation := range []byte{'-', '+'} {
			last := ""
			for _, edit := range script[i:end] {
				if edit.operation == operation {
					output.WriteString(edit.line)
					last = edit.line
				}
			}
			if last != "" && !strings.HasSuffix(last, "\n") {
				output.WriteByte('\n')
			}
			output.WriteString(markers[side+1] + "\n")
		}
		i = end
	}
	return output.String()
}

// containsMarkers uses bounded buffers, including for arbitrarily long lines.
func containsMarkers(path, token string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	markers := markerLines(token)
	longest := 0
	for _, marker := range markers {
		longest = max(longest, len(marker))
	}
	buffer := make([]byte, 64*1024+longest)
	retained := 0
	for {
		count, err := file.Read(buffer[retained:])
		data := buffer[:retained+count]
		for _, marker := range markers {
			if bytes.Contains(data, []byte(marker)) {
				return true, nil
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		retained = min(len(data), longest-1)
		copy(buffer, data[len(data)-retained:])
	}
}
