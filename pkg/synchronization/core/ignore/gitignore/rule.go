package gitignore

import (
	"strings"
)

// ruleKind identifies the strategy used to evaluate a rule.
type ruleKind uint8

const (
	// ruleKindLiteral rules match text that is equal to a literal string.
	ruleKindLiteral ruleKind = iota
	// ruleKindSuffix rules match text that ends with a literal string and
	// whose remainder contains no slashes (i.e. rules of the form "*suffix").
	ruleKindSuffix
	// ruleKindGeneral rules require running tokens through a matcher.
	ruleKindGeneral
)

// rule is a single compiled line of a rule file.
type rule struct {
	// negated indicates that the rule re-includes paths that it matches.
	negated bool
	// directoryOnly indicates that the rule matches only directories.
	directoryOnly bool
	// anchored indicates that the rule is matched against the path relative to
	// the directory containing the rule file, rather than against base names.
	anchored bool
	// kind is the evaluation strategy.
	kind ruleKind
	// literal is, depending on kind, the entire text to match (for literal
	// rules), the suffix to match (for suffix rules), or the literal text that
	// precedes the first wildcard (for general rules).
	literal string
	// tokens are the tokens that match the text following literal. They are
	// only used by general rules.
	tokens []token
	// suffix is literal text that every match must end with. It's used by
	// general rules to quickly reject text that can't match.
	suffix string
	// minimumLength is the minimum length of text matched by tokens.
	minimumLength int
}

// trimTrailingSpaces removes trailing spaces from a line, except for those that
// are escaped with a backslash. Other kinds of whitespace aren't removed.
func trimTrailingSpaces(line string) string {
	// In the common case there is nothing to trim.
	if len(line) == 0 || line[len(line)-1] != ' ' {
		return line
	}

	// Find the end of the last byte that isn't an unescaped space. We have to
	// scan from the front, because whether or not a backslash escapes the byte
	// that follows it depends on the backslashes that precede it.
	var end int
	for i := 0; i < len(line); {
		switch line[i] {
		case '\\':
			if i+1 < len(line) {
				i += 2
			} else {
				i++
			}
			end = i
		case ' ':
			i++
		default:
			i++
			end = i
		}
	}
	return line[:end]
}

// parseRule parses a single line of a rule file, which must already have had
// its line ending removed. It returns false if the line doesn't produce a rule,
// either because it's blank, a comment, or malformed. A malformed line is one
// that could never match anything (e.g. due to an unterminated bracket
// expression), so omitting it is equivalent to retaining it.
func parseRule(line string) (rule, bool) {
	// Skip blank lines and comments.
	if line == "" || line[0] == '#' {
		return rule{}, false
	}

	// Remove trailing spaces and check whether or not there's anything left.
	line = trimTrailingSpaces(line)
	if line == "" {
		return rule{}, false
	}

	// Handle negation.
	var r rule
	if line[0] == '!' {
		r.negated = true
		line = line[1:]
	}

	// Handle directory-only rules.
	if line != "" && line[len(line)-1] == '/' {
		r.directoryOnly = true
		line = line[:len(line)-1]
	}

	// A rule that contains a slash at this point is anchored to the directory
	// containing the rule file. Any single leading slash is only an anchor, so
	// it isn't part of the text to match.
	r.anchored = strings.IndexByte(line, '/') >= 0
	if line != "" && line[0] == '/' {
		line = line[1:]
	}

	// An empty pattern can't match anything.
	if line == "" {
		return rule{}, false
	}

	// Compile the pattern text.
	if !r.compile(line) {
		return rule{}, false
	}
	return r, true
}

// compile compiles the pattern text of a rule, which must be non-empty and
// stripped of negation, leading slash, and trailing slash. It sets the kind,
// literal, tokens, suffix, and minimumLength fields of the rule and returns
// false if the pattern can never match.
//
// The literal text that opens the pattern (i.e. everything that precedes the
// first asterisk, question mark, opening bracket, or backslash) is recorded
// separately, so that it can be compared with a single string operation before
// any wildcards are considered. It has no effect on what the pattern matches.
func (r *rule) compile(pattern string) bool {
	// Split the pattern into its leading literal text and the remainder.
	split := strings.IndexAny(pattern, "*?[\\")
	if split < 0 {
		split = len(pattern)
	}
	r.literal = pattern[:split]
	remainder := pattern[split:]

	// Convert the remainder to tokens.
	var tokens []token
	for i := 0; i < len(remainder); {
		switch current := remainder[i]; current {
		case '\\':
			// An escaped byte is a literal. A backslash at the end of the
			// pattern escapes nothing, so the pattern is malformed.
			if i+1 >= len(remainder) {
				return false
			}
			tokens = append(tokens, token{kind: tokenLiteral, value: remainder[i+1]})
			i += 2
		case '?':
			tokens = append(tokens, token{kind: tokenAnyByte})
			i++
		case '[':
			class, next, ok := parseClass(remainder, i)
			if !ok {
				return false
			}
			tokens = append(tokens, token{kind: tokenClass, class: class})
			i = next
		case '*':
			// Find the end of the run of asterisks.
			end := i + 1
			for end < len(remainder) && remainder[end] == '*' {
				end++
			}

			// Two or more asterisks have special meaning in an anchored
			// pattern, provided that they form an entire path component. That
			// means they must be preceded by a slash (or be at the start of
			// the pattern) and followed by a slash, an escaped slash, or the
			// end of the pattern. In all other cases, they match like a single
			// asterisk. When the asterisks are followed by an unescaped slash,
			// then the directories that they match may be omitted altogether
			// along with the slash. Otherwise they match any text, including
			// slashes, and any escaped slash that follows is matched
			// normally.
			//
			// Consecutive runs that match any directories are equivalent to a
			// single one, since matching any number of directories twice
			// matches any number of directories, and they're also redundant
			// before a run that matches any text. So they're collapsed, which
			// doesn't change what the pattern matches, but which bounds the
			// work that the matcher does without consuming input (see
			// matcher.add), which would otherwise be proportional to the
			// length of the run, and so would the depth of its recursion,
			// which could exhaust the stack for a long enough line.
			special := r.anchored && end-i >= 2 && (split+i == 0 || pattern[split+i-1] == '/')
			switch {
			case special && end < len(remainder) && remainder[end] == '/':
				if !endsWithAnyDirectories(tokens) {
					tokens = append(tokens, token{kind: tokenAnyDirectories}, token{kind: tokenAnyDirectoriesLoop})
				}
				end++
			case special && (end == len(remainder) || (remainder[end] == '\\' && end+1 < len(remainder) && remainder[end+1] == '/')):
				if endsWithAnyDirectories(tokens) {
					tokens = tokens[:len(tokens)-2]
				}
				tokens = append(tokens, token{kind: tokenAnyPath})
			default:
				tokens = append(tokens, token{kind: tokenStar})
			}
			i = end
		default:
			tokens = append(tokens, token{kind: tokenLiteral, value: current})
			i++
		}
	}

	// If the rule consists only of literal bytes, then it can be evaluated by
	// comparing strings. Escaped bytes have to be folded into the literal text
	// for this to work.
	allLiteral := true
	for i := range tokens {
		if tokens[i].kind != tokenLiteral {
			allLiteral = false
			break
		}
	}
	if allLiteral {
		var builder strings.Builder
		builder.WriteString(r.literal)
		for i := range tokens {
			builder.WriteByte(tokens[i].value)
		}
		r.kind = ruleKindLiteral
		r.literal = builder.String()
		return true
	}

	// Check for a rule that consists only of an asterisk and literal bytes,
	// which is common (e.g. "*.log") and can be evaluated as a suffix
	// comparison.
	if r.literal == "" && tokens[0].kind == tokenStar {
		suffix := true
		var builder strings.Builder
		for i := 1; i < len(tokens); i++ {
			if tokens[i].kind != tokenLiteral {
				suffix = false
				break
			}
			builder.WriteByte(tokens[i].value)
		}
		if suffix {
			r.kind = ruleKindSuffix
			r.literal = builder.String()
			r.tokens = nil
			return true
		}
	}

	// Record the information needed to quickly reject text that can't match
	// the tokens.
	r.kind = ruleKindGeneral
	r.tokens = tokens
	for i := range tokens {
		switch tokens[i].kind {
		case tokenLiteral, tokenAnyByte, tokenClass:
			r.minimumLength++
		}
	}
	trailing := len(tokens)
	for trailing > 0 && tokens[trailing-1].kind == tokenLiteral {
		trailing--
	}
	if trailing < len(tokens) {
		suffix := make([]byte, 0, len(tokens)-trailing)
		for _, t := range tokens[trailing:] {
			suffix = append(suffix, t.value)
		}
		r.suffix = string(suffix)
	}
	return true
}

// endsWithAnyDirectories returns true if a list of tokens ends with the pair of
// tokens that matches any number of directories.
func endsWithAnyDirectories(tokens []token) bool {
	return len(tokens) >= 2 &&
		tokens[len(tokens)-2].kind == tokenAnyDirectories &&
		tokens[len(tokens)-1].kind == tokenAnyDirectoriesLoop
}

// matches returns true if the rule matches an entry. The base name of the
// entry and its path relative to the directory containing the rule file are
// both provided (the former is a suffix of the latter).
func (r *rule) matches(base, relative string, directory bool, m *matcher) bool {
	// Directory-only rules never match anything else.
	if r.directoryOnly && !directory {
		return false
	}

	// Select the text to match.
	text := base
	if r.anchored {
		text = relative
	}

	// Evaluate the rule.
	switch r.kind {
	case ruleKindLiteral:
		return text == r.literal
	case ruleKindSuffix:
		if !strings.HasSuffix(text, r.literal) {
			return false
		}
		return !r.anchored || strings.IndexByte(text[:len(text)-len(r.literal)], '/') < 0
	default:
		if !strings.HasPrefix(text, r.literal) {
			return false
		}
		text = text[len(r.literal):]
		if len(text) < r.minimumLength || !strings.HasSuffix(text, r.suffix) {
			return false
		}
		return m.matches(r.tokens, text)
	}
}
