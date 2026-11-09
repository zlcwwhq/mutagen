package gitignore

import (
	"strings"
	"unsafe"
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

// rule is a single compiled line of a rule file. A rule file can contain
// millions of rules, so its fields are ordered to keep it compact, and its
// strings refer to the rule file's contents where possible.
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
	// minimumLength is the minimum length of text matched by tokens. It can't
	// overflow, since it's less than the length of a rule file.
	minimumLength int32
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

// mayProduceRule returns false if a line of a rule file can't produce a rule,
// because it's blank or a comment, as parseRule determines first. It's cheap,
// which allows the rules in a file to be counted (from above) before they're
// parsed.
func mayProduceRule(line string) bool {
	return line != "" && line[0] != '#' && trimTrailingSpaces(line) != ""
}

// parseRule parses a single line of a rule file, which must already have had
// its line ending removed. It returns false if the line doesn't produce a rule,
// either because it's blank, a comment, or malformed. A malformed line is one
// that could never match anything (e.g. due to an unterminated bracket
// expression), so omitting it is equivalent to retaining it. It also returns
// false if the rule would exceed maximumCompiledRuleSize on its own.
func parseRule(line string) (rule, bool) {
	parser := newRuleParser(maximumCompiledRuleSize)
	r, ok, _ := parser.parse(line)
	return r, ok
}

const (
	// ruleSize, tokenSize, and byteSetSize are the sizes of a rule, a token,
	// and a bracket expression's set of bytes, which make up the memory that
	// compiled rules take (see maximumCompiledRuleSize).
	ruleSize    = uint64(unsafe.Sizeof(rule{}))
	tokenSize   = uint64(unsafe.Sizeof(token{}))
	byteSetSize = uint64(unsafe.Sizeof(byteSet{}))
)

// compiledSize returns the amount of memory that a compiled rule takes, apart
// from its strings, which are no longer than its line, and which usually refer
// to the rule file's contents.
func (r *rule) compiledSize() uint64 {
	size := ruleSize + uint64(len(r.tokens))*tokenSize
	for i := range r.tokens {
		if r.tokens[i].kind == tokenClass {
			size += byteSetSize
		}
	}
	return size
}

// ruleParser parses the rules of a rule file (see parseRule), keeping their
// compiled size within a limit. It retains working storage between rules, so
// that a rule doesn't allocate more than its compiled form.
type ruleParser struct {
	// limit is the amount of memory that the rules can take in all (see
	// rule.compiledSize), which also limits the working storage.
	limit uint64
	// remaining is the amount of memory that the rules that are still to be
	// parsed can take.
	remaining uint64
	// tokens is the storage in which tokens are accumulated.
	tokens []token
}

// newRuleParser creates a parser for rules that can take the specified amount
// of memory in all.
func newRuleParser(limit uint64) *ruleParser {
	return &ruleParser{limit: limit, remaining: limit}
}

// parse implements parseRule, and it subtracts the size of the rule that it
// yields from the amount of memory that remains. If the rule would exceed that,
// then it fails with errRuleFileComplexity.
func (p *ruleParser) parse(line string) (rule, bool, error) {
	// Skip blank lines and comments, and remove trailing spaces.
	if !mayProduceRule(line) {
		return rule{}, false, nil
	}
	line = trimTrailingSpaces(line)

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
		return rule{}, false, nil
	}

	// Compile the pattern text, which can't accumulate more tokens than the
	// limit allows for in all, and account for the result.
	var ok bool
	var err error
	if p.tokens, ok, err = r.compile(line, p.tokens[:0], p.limit); err != nil || !ok {
		return rule{}, false, err
	} else if size := r.compiledSize(); size > p.remaining {
		return rule{}, false, errRuleFileComplexity
	} else {
		p.remaining -= size
	}
	return r, true, nil
}

// compile compiles the pattern text of a rule, which must be non-empty and
// stripped of negation, leading slash, and trailing slash. It sets the kind,
// literal, tokens, suffix, and minimumLength fields of the rule and returns
// false if the pattern can never match. Tokens are accumulated in the specified
// storage, which is returned for reuse, and the rule only retains a copy of
// those that it needs. If the tokens (and the sets of bytes of their bracket
// expressions) would take more memory than the specified limit, which no rule
// within the limit can have, then it fails with errRuleFileComplexity, before
// they do.
//
// The literal text that opens the pattern (i.e. everything that precedes the
// first asterisk, question mark, opening bracket, or backslash) is recorded
// separately, so that it can be compared with a single string operation before
// any wildcards are considered. It has no effect on what the pattern matches.
func (r *rule) compile(pattern string, tokens []token, limit uint64) ([]token, bool, error) {
	// Split the pattern into its leading literal text and the remainder.
	split := strings.IndexAny(pattern, "*?[\\")
	if split < 0 {
		split = len(pattern)
	}
	r.literal = pattern[:split]
	remainder := pattern[split:]

	// Convert the remainder to tokens, keeping track of how many of them are
	// bracket expressions.
	var classes uint64
	for i := 0; i < len(remainder); {
		switch current := remainder[i]; current {
		case '\\':
			// An escaped byte is a literal. A backslash at the end of the
			// pattern escapes nothing, so the pattern is malformed.
			if i+1 >= len(remainder) {
				return tokens, false, nil
			}
			tokens = append(tokens, token{kind: tokenLiteral, value: remainder[i+1]})
			i += 2
		case '?':
			tokens = append(tokens, token{kind: tokenAnyByte})
			i++
		case '[':
			class, next, ok := parseClass(remainder, i)
			if !ok {
				return tokens, false, nil
			}
			tokens = append(tokens, token{kind: tokenClass, class: class})
			classes++
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

		// Stop once the tokens take more memory than the limit allows.
		if uint64(len(tokens))*tokenSize+classes*byteSetSize > limit {
			return tokens, false, errRuleFileComplexity
		}
	}

	// If the rule consists only of literal bytes, then it can be evaluated by
	// comparing strings. Escaped bytes have to be folded into the literal text
	// for this to work, and the remainder can only consist of literal bytes if
	// it's empty or begins with an escape.
	allLiteral := true
	for i := range tokens {
		if tokens[i].kind != tokenLiteral {
			allLiteral = false
			break
		}
	}
	if allLiteral {
		r.kind = ruleKindLiteral
		if len(tokens) > 0 {
			var builder strings.Builder
			builder.Grow(len(r.literal) + len(tokens))
			builder.WriteString(r.literal)
			for i := range tokens {
				builder.WriteByte(tokens[i].value)
			}
			r.literal = builder.String()
		}
		return tokens, true, nil
	}

	// Check for a rule that consists only of an asterisk and literal bytes,
	// which is common (e.g. "*.log") and can be evaluated as a suffix
	// comparison.
	if r.literal == "" && tokens[0].kind == tokenStar {
		suffix := true
		for i := 1; i < len(tokens); i++ {
			if tokens[i].kind != tokenLiteral {
				suffix = false
				break
			}
		}
		if suffix {
			r.kind = ruleKindSuffix
			r.literal = literalText(remainder, tokens[1:])
			r.tokens = nil
			return tokens, true, nil
		}
	}

	// Record the information needed to quickly reject text that can't match
	// the tokens. The rule keeps a copy of its tokens, since their storage is
	// reused.
	r.kind = ruleKindGeneral
	r.tokens = make([]token, len(tokens))
	copy(r.tokens, tokens)
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
		r.suffix = literalText(remainder, tokens[trailing:])
	}
	return tokens, true, nil
}

// literalText returns the text that a list of literal tokens matches, which are
// the last tokens of the specified pattern text. If they correspond to the end
// of the text (i.e. none of them were escaped, or the escapes happen to have
// the same text), then that's what's returned, so that it refers to the rule
// file's contents rather than being a copy.
func literalText(pattern string, tokens []token) string {
	if len(tokens) <= len(pattern) {
		end := pattern[len(pattern)-len(tokens):]
		same := true
		for i := range tokens {
			if end[i] != tokens[i].value {
				same = false
				break
			}
		}
		if same {
			return end
		}
	}
	text := make([]byte, len(tokens))
	for i := range tokens {
		text[i] = tokens[i].value
	}
	return string(text)
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
		if len(text) < int(r.minimumLength) || !strings.HasSuffix(text, r.suffix) {
			return false
		}
		return m.matches(r.tokens, text)
	}
}
