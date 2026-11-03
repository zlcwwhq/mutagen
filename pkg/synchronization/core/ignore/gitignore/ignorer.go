package gitignore

import (
	"bytes"
	"strings"
)

// parseRules parses the contents of a rule file.
func parseRules(contents []byte) []rule {
	// Skip any UTF-8 byte order mark.
	contents = bytes.TrimPrefix(contents, []byte("\xef\xbb\xbf"))

	// Parse each line.
	var rules []rule
	text := string(contents)
	for len(text) > 0 {
		// Extract the next line, without its line ending. Only a single
		// carriage return is considered part of the line ending.
		var line string
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			line, text = text[:newline], text[newline+1:]
		} else {
			line, text = text, ""
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		// A null byte terminates the line.
		if null := strings.IndexByte(line, 0); null >= 0 {
			line = line[:null]
		}

		// Parse the rule.
		if r, ok := parseRule(line); ok {
			rules = append(rules, r)
		}
	}
	return rules
}
