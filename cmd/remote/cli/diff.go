package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

const (
	// diffContext is the number of unchanged lines shown around changes.
	diffContext = 3
	// maximumMinimizedEdits bounds the edits for which a minimal edit script is
	// computed. Beyond it, the differing region is replaced wholesale, which
	// bounds the quadratic memory usage of the search.
	maximumMinimizedEdits = 2000
	// binaryProbeSize is the number of leading bytes inspected for null bytes
	// to detect binary files, as Git does.
	binaryProbeSize = 8000
)

// edit is a line of an edit script.
type edit struct {
	// operation is ' ' for unchanged lines, '-' for deletions, and '+' for
	// insertions.
	operation byte
	// line is the line, including any terminating newline.
	line string
}

// splitLines splits data into lines, each including its terminating newline
// (which the final line may lack).
func splitLines(data []byte) []string {
	var lines []string
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n') + 1
		if end == 0 {
			end = len(data)
		}
		lines = append(lines, string(data[:end]))
		data = data[end:]
	}
	return lines
}

// diffLines computes an edit script transforming a into b.
func diffLines(a, b []string) []edit {
	// Unchanged leading and trailing lines are common and cheap to identify.
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	var script []edit
	for _, line := range a[:prefix] {
		script = append(script, edit{' ', line})
	}
	script = append(script, minimalEdits(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])...)
	for _, line := range a[len(a)-suffix:] {
		script = append(script, edit{' ', line})
	}
	return script
}

// minimalEdits computes a minimal edit script transforming a into b using
// Myers' algorithm, unless more than maximumMinimizedEdits edits are required.
func minimalEdits(a, b []string) []edit {
	// Search for the furthest-reaching path on each diagonal k = x - y for
	// increasing numbers of edits d, recording the endpoints for each d.
	n, m := len(a), len(b)
	limit := min(n+m, maximumMinimizedEdits)
	offset := limit + 1
	frontier := make([]int, 2*limit+3)
	var trace [][]int
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && frontier[offset+k-1] < frontier[offset+k+1]) {
				x = frontier[offset+k+1]
			} else {
				x = frontier[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			frontier[offset+k] = x
			if x >= n && y >= m {
				trace = append(trace, append([]int(nil), frontier[offset-d:offset+d+1]...))
				return backtrackEdits(a, b, trace)
			}
		}
		trace = append(trace, append([]int(nil), frontier[offset-d:offset+d+1]...))
	}

	// Replace the region wholesale if it differs too much.
	script := make([]edit, 0, n+m)
	for _, line := range a {
		script = append(script, edit{'-', line})
	}
	for _, line := range b {
		script = append(script, edit{'+', line})
	}
	return script
}

// backtrackEdits reconstructs an edit script from the endpoints recorded by
// minimalEdits, where trace[d][k+d] is the furthest x on diagonal k after d
// edits.
func backtrackEdits(a, b []string, trace [][]int) []edit {
	var reversed []edit
	x, y := len(a), len(b)
	for d := len(trace) - 1; d > 0; d-- {
		previous := trace[d-1]
		k := x - y
		var previousK int
		if k == -d || (k != d && previous[k-1+d-1] < previous[k+1+d-1]) {
			previousK = k + 1
		} else {
			previousK = k - 1
		}
		previousX := previous[previousK+d-1]
		previousY := previousX - previousK
		for x > previousX && y > previousY {
			reversed = append(reversed, edit{' ', a[x-1]})
			x, y = x-1, y-1
		}
		if previousK == k+1 {
			reversed = append(reversed, edit{'+', b[y-1]})
			y--
		} else {
			reversed = append(reversed, edit{'-', a[x-1]})
			x--
		}
	}
	for x > 0 && y > 0 {
		reversed = append(reversed, edit{' ', a[x-1]})
		x, y = x-1, y-1
	}
	script := make([]edit, len(reversed))
	for i, e := range reversed {
		script[len(reversed)-1-i] = e
	}
	return script
}

// hunkRange formats a hunk's line range in unified diff format.
func hunkRange(start, count int) string {
	if count == 0 {
		// Empty ranges refer to the line before the hunk.
		return fmt.Sprintf("%d,0", start)
	} else if count == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}

// unifiedDiff formats an edit script as a unified diff with the specified
// headers, returning an empty string if there are no changes.
func unifiedDiff(from, to string, script []edit) string {
	// Compute the line numbers in each file at each position in the script.
	aLines, bLines := make([]int, len(script)+1), make([]int, len(script)+1)
	for i, e := range script {
		aLines[i+1], bLines[i+1] = aLines[i], bLines[i]
		if e.operation != '+' {
			aLines[i+1]++
		}
		if e.operation != '-' {
			bLines[i+1]++
		}
	}

	// Group changes separated by at most twice the context into hunks.
	var output strings.Builder
	for i := 0; i < len(script); {
		if script[i].operation == ' ' {
			i++
			continue
		}
		start, end := max(0, i-diffContext), i
		for end < len(script) {
			if script[end].operation != ' ' {
				end++
				continue
			}
			unchanged := 0
			for end+unchanged < len(script) && script[end+unchanged].operation == ' ' {
				unchanged++
			}
			if end+unchanged < len(script) && unchanged <= 2*diffContext {
				end += unchanged
			} else {
				end += min(unchanged, diffContext)
				break
			}
		}
		if output.Len() == 0 {
			fmt.Fprintf(&output, "--- %s\n+++ %s\n", from, to)
		}
		fmt.Fprintf(&output, "@@ -%s +%s @@\n",
			hunkRange(aLines[start], aLines[end]-aLines[start]),
			hunkRange(bLines[start], bLines[end]-bLines[start]),
		)
		for _, e := range script[start:end] {
			output.WriteByte(e.operation)
			output.WriteString(strings.TrimSuffix(e.line, "\n"))
			output.WriteByte('\n')
			if !strings.HasSuffix(e.line, "\n") {
				output.WriteString("\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return output.String()
}

// isBinary determines whether data appears to be binary.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binaryProbeSize)], 0) >= 0
}

// describeEntry describes an entry for comparison output.
func describeEntry(entry *remote.Entry) string {
	switch entry.Kind {
	case remote.EntryFile:
		description := fmt.Sprintf("file of %d bytes", entry.Size)
		if entry.Executable {
			description = "executable " + description
		}
		return description
	case remote.EntrySymbolicLink:
		return "symbolic link to " + entry.Target
	case remote.EntryOther:
		return "special file"
	default:
		return entry.Kind
	}
}

// compareEntries describes how the remote entry at a path differs from the
// local one, returning an empty string if they're equivalent. Files are
// compared as a unified diff from the local copy to the remote one when
// possible. The path is used in output and must already be neutralized.
// Differences in executability are only reported for identical contents.
func compareEntries(path string, local, opposite *remote.Entry) string {
	// Compare file contents when both sides have (or one side lacks) a file.
	localFile, oppositeFile := local.Kind == remote.EntryFile, opposite.Kind == remote.EntryFile
	if (localFile || local.Kind == remote.EntryMissing) && (oppositeFile || opposite.Kind == remote.EntryMissing) {
		if !localFile && !oppositeFile {
			return ""
		} else if localFile && oppositeFile && local.Digest == opposite.Digest {
			if local.Executable == opposite.Executable {
				return ""
			}
			return fmt.Sprintf("%s: %s locally, %s remotely (contents are identical)\n",
				path, describeEntry(local), describeEntry(opposite))
		}
		from, to := path+" (local)", path+" (remote)"
		if !localFile {
			from = "/dev/null"
		} else if !oppositeFile {
			to = "/dev/null"
		}
		tooLarge := (localFile && local.Data == nil && local.Size > 0) ||
			(oppositeFile && opposite.Data == nil && opposite.Size > 0)
		if tooLarge {
			return fmt.Sprintf("%s: %s locally, %s remotely (too large to compare)\n",
				path, describeEntry(local), describeEntry(opposite))
		} else if isBinary(local.Data) || isBinary(opposite.Data) {
			return fmt.Sprintf("Binary files %s and %s differ\n", from, to)
		}
		// An empty file and a missing one have no lines that differ.
		if difference := unifiedDiff(from, to, diffLines(splitLines(local.Data), splitLines(opposite.Data))); difference != "" {
			return difference
		}
		return fmt.Sprintf("%s: %s locally, %s remotely\n", path, describeEntry(local), describeEntry(opposite))
	}

	// Otherwise, describe the entries.
	if local.Kind == opposite.Kind && local.Target == opposite.Target {
		return ""
	}
	return fmt.Sprintf("%s: %s locally, %s remotely\n", path, describeEntry(local), describeEntry(opposite))
}

// compare prints how the remote copies of the specified path arguments (or, if
// there are none, of the paths involved in the report's conflicts) differ from
// the local copies, and reports whether any differ. The target and report are
// those of a flush. Path arguments must exist on at least one endpoint.
func compare(ctx context.Context, directory, session string, target *remote.Target, report *remote.Report, arguments []string, output, errorOutput io.Writer) (bool, error) {
	locator := newLocator(target)
	var paths []string
	if len(arguments) == 0 {
		if paths = conflictPaths(report); len(paths) == 0 {
			fmt.Fprintln(errorOutput, "remote: there are no conflicts to compare")
		}
	}
	for _, argument := range arguments {
		path, err := locator.resolve(argument)
		if err != nil {
			return false, err
		}
		paths = append(paths, path)
	}
	var differ bool
	for _, path := range paths {
		// The root is a directory on both endpoints while synchronizing.
		if path == "" {
			continue
		}
		local, err := remote.ReadEntry(filepath.Join(target.Root, filepath.FromSlash(path)))
		if err != nil {
			return differ, fmt.Errorf("unable to read %s: %w", locator.display(path), err)
		}
		connection, _, err := remote.Dial(ctx, directory, session, remote.OperationExecute)
		if err != nil {
			return differ, err
		}
		opposite, err := remote.Fetch(ctx, connection, path)
		if err != nil {
			return differ, fmt.Errorf("unable to read remote %s: %w", locator.display(path), err)
		} else if len(arguments) > 0 && local.Kind == remote.EntryMissing && opposite.Kind == remote.EntryMissing {
			return differ, fmt.Errorf("%s doesn't exist locally or remotely", locator.display(path))
		}
		if difference := compareEntries(locator.display(path), local, opposite); difference != "" {
			differ = true
			io.WriteString(output, difference)
		}
	}
	return differ, nil
}
