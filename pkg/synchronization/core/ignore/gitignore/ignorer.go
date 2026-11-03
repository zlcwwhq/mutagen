package gitignore

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

const (
	// ruleFileName is the name of the rule files that are found in directories
	// throughout a tree.
	ruleFileName = ".gitignore"
	// excludeFilePath is the slash-separated, root-relative path of the rule
	// file for the root repository.
	excludeFilePath = ".git/info/exclude"
	// maximumRuleFileSize is the maximum size of a rule file that will be read.
	// A larger file is treated as though it were empty. This is simply a
	// safeguard against unreasonable memory consumption.
	maximumRuleFileSize = 100 * 1024 * 1024
	// maximumPointerFileSize is the maximum size of the files that point to the
	// location of a Git directory (a ".git" file or a "commondir" file) that
	// will be read.
	maximumPointerFileSize = 64 * 1024
)

// IsRulePath returns true if a root-relative, slash-separated path identifies a
// file whose contents affect ignore evaluation. This is the case for any file
// whose base name is ".gitignore" and for the root repository's
// ".git/info/exclude" file.
func IsRulePath(path string) bool {
	if path == excludeFilePath {
		return true
	}
	return path[strings.LastIndexByte(path, '/')+1:] == ruleFileName
}

// ruleSet is a list of rules that apply to a subtree, along with the rules that
// apply to the tree beyond it. A rule set is a link in a chain that ends at
// the lowest-precedence rules.
type ruleSet struct {
	// rules are the rules, in the order they appeared in their file.
	rules []rule
	// prefixLength is the length of the root-relative path of the directory
	// that contains the rules' file, including a trailing slash (and thus zero
	// for rules that are relative to the root).
	prefixLength int
	// parent is the rule set with the next-highest precedence, or nil if there
	// is none.
	parent *ruleSet
}

// ignorer implements ignore.Ignorer for gitignore-style rules found on disk.
type ignorer struct {
	// root is the synchronization root.
	root string
	// session is the session-specific ignorer, if any.
	session ignore.Ignorer
	// directories maps root-relative directory paths to the chain of rule sets
	// that apply to entries in those directories. The chain may be nil.
	directories map[string]*ruleSet
	// lastDirectory is the most recent directory that was looked up in
	// directories. Scans tend to visit the entries in a directory in sequence,
	// so remembering it saves a map lookup. It's only valid if lastValid is
	// true.
	lastDirectory string
	// lastRules is the chain of rule sets for lastDirectory.
	lastRules *ruleSet
	// lastValid indicates whether or not lastDirectory and lastRules are
	// populated.
	lastValid bool
	// excludeLoaded indicates whether or not exclude has been populated.
	excludeLoaded bool
	// exclude is the rule set for the root repository's exclude file.
	exclude *ruleSet
	// matcher is used to match rules.
	matcher matcher
}

// NewIgnorer returns an ignorer for a single scan of the specified
// synchronization root. The ignorer reads rule files lazily, once each, and
// retains what it reads, so it must not be reused for a scan that needs to
// observe changes to those files. The session ignorer, which may be nil,
// provides ignore rules that take precedence over the rule files and that
// directly control traversal. The returned ignorer is not safe for concurrent
// use by multiple Goroutines.
func NewIgnorer(root string, session ignore.Ignorer) ignore.Ignorer {
	return &ignorer{
		root:        root,
		session:     session,
		directories: make(map[string]*ruleSet),
	}
}

// Ignore implements ignore.Ignorer.Ignore.
func (i *ignorer) Ignore(path string, directory bool) (ignore.IgnoreStatus, bool) {
	// Split the path into its parent directory and base name.
	var parent string
	base := path
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		parent = path[:slash]
		base = path[slash+1:]
	}

	// Never allow Git metadata to be synchronized, regardless of any rules.
	// This is checked before the session ignorer, so it can't be overridden.
	if len(base) == len(".git") && strings.EqualFold(base, ".git") {
		return ignore.IgnoreStatusIgnored, false
	}

	// Give the session ignorer precedence. If it has an opinion, then it
	// decides, otherwise we only retain its traversal directive.
	var continueTraversal bool
	if i.session != nil {
		var status ignore.IgnoreStatus
		status, continueTraversal = i.session.Ignore(path, directory)
		if status != ignore.IgnoreStatusNominal {
			return status, continueTraversal
		}
	}

	// The synchronization root itself is never ignored.
	if path == "" {
		return ignore.IgnoreStatusNominal, continueTraversal
	}

	// Evaluate the rules, from highest to lowest precedence. The first rule set
	// with a matching rule decides, and the last matching rule within it wins.
	// A negated rule doesn't result in an unignored status, because a rule
	// file can only re-include content that its own rules excluded, and the
	// session ignorer has already had its say.
	for set := i.rulesFor(parent); set != nil; set = set.parent {
		for index := len(set.rules) - 1; index >= 0; index-- {
			if set.rules[index].matches(base, path[set.prefixLength:], directory, &i.matcher) {
				if set.rules[index].negated {
					return ignore.IgnoreStatusNominal, continueTraversal
				}
				return ignore.IgnoreStatusIgnored, continueTraversal
			}
		}
	}
	return ignore.IgnoreStatusNominal, continueTraversal
}

// EvaluatesNonUTF8 implements ignore.NonUTF8Ignorer.EvaluatesNonUTF8. Rules
// match bytes, as Git matches them, so they apply to names that aren't valid
// UTF-8 just as they apply to any other. The session ignorer, which takes
// precedence, is given such names as well.
func (i *ignorer) EvaluatesNonUTF8() bool {
	return true
}

// rulesFor returns the chain of rule sets that apply to the entries in a
// directory, specified by its root-relative path (which is empty for the
// synchronization root).
func (i *ignorer) rulesFor(directory string) *ruleSet {
	if i.lastValid && i.lastDirectory == directory {
		return i.lastRules
	}
	rules := i.load(directory)
	i.lastDirectory = directory
	i.lastRules = rules
	i.lastValid = true
	return rules
}

// load returns the chain of rule sets that apply to the entries in a directory,
// reading rule files as necessary.
func (i *ignorer) load(directory string) *ruleSet {
	// Check if we've already done this work.
	if rules, ok := i.directories[directory]; ok {
		return rules
	}

	// The rules of the parent directory also apply (with lower precedence), and
	// the root's parent is the root repository's exclude file.
	var parent *ruleSet
	var prefixLength int
	if directory == "" {
		parent = i.excludeRules()
	} else {
		parentDirectory := ""
		if slash := strings.LastIndexByte(directory, '/'); slash >= 0 {
			parentDirectory = directory[:slash]
		}
		parent = i.load(parentDirectory)
		prefixLength = len(directory) + 1
	}

	// Read the directory's own rules. If it doesn't have any, then the chain is
	// simply the parent's.
	rules := parent
	path := filepath.Join(i.root, filepath.FromSlash(directory), ruleFileName)
	if parsed := parseRules(readRegularFile(path, maximumRuleFileSize)); len(parsed) > 0 {
		rules = &ruleSet{rules: parsed, prefixLength: prefixLength, parent: parent}
	}

	// Remember the result.
	i.directories[directory] = rules
	return rules
}

// excludeRules returns the rule set for the root repository's exclude file, or
// nil if there is no such file or it contains no rules.
func (i *ignorer) excludeRules() *ruleSet {
	if !i.excludeLoaded {
		i.excludeLoaded = true
		if path := excludeFileLocation(i.root); path != "" {
			if rules := parseRules(readRegularFile(path, maximumRuleFileSize)); len(rules) > 0 {
				i.exclude = &ruleSet{rules: rules}
			}
		}
	}
	return i.exclude
}

// readRegularFile reads a file if (and only if) it is a regular file (i.e. not
// a symbolic link or anything else) that is no larger than the specified size.
// Otherwise it returns nil.
func readRegularFile(path string, limit int64) []byte {
	// Check the type and size of the file.
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil
	}

	// Open the file and read it, taking care not to read more than expected if
	// the file grows.
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(contents)) > limit {
		return nil
	}
	return contents
}

// excludeFileLocation determines the location of the exclude file for the
// repository at a synchronization root. It returns an empty string if the root
// doesn't appear to be the root of a repository. The root's ".git" entry
// may either be a directory, in which case it contains the exclude file, or a
// file that points to the Git directory (as is the case for submodules and
// linked worktrees). If the Git directory in turn points to a common directory
// (as is the case for linked worktrees), then that is where the exclude file
// is found.
func excludeFileLocation(root string) string {
	// Check the type of the root's ".git" entry. We deliberately don't follow
	// symbolic links.
	dotGit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return filepath.Join(dotGit, "info", "exclude")
	} else if !info.Mode().IsRegular() {
		return ""
	}

	// Read the location of the Git directory from the file.
	contents := readRegularFile(dotGit, maximumPointerFileSize)
	if contents == nil {
		return ""
	}
	pointer, ok := parsePointer(contents, "gitdir:")
	if !ok {
		return ""
	}
	gitDirectory := resolvePath(root, pointer)

	// Check whether or not the Git directory points to a common directory.
	if contents := readRegularFile(filepath.Join(gitDirectory, "commondir"), maximumPointerFileSize); contents != nil {
		if pointer, ok := parsePointer(contents, ""); ok {
			return filepath.Join(resolvePath(gitDirectory, pointer), "info", "exclude")
		}
	}
	return filepath.Join(gitDirectory, "info", "exclude")
}

// parsePointer extracts a path from the first line of a file that points to
// another location. The line must begin with the specified prefix, which may be
// empty, and the path may be preceded by whitespace that's not part of it.
func parsePointer(contents []byte, prefix string) (string, bool) {
	// Extract the first line.
	if newline := bytes.IndexByte(contents, '\n'); newline >= 0 {
		contents = contents[:newline]
	}
	line := strings.TrimSpace(string(contents))

	// Extract the path.
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	path := strings.TrimSpace(line[len(prefix):])
	return path, path != ""
}

// resolvePath resolves a slash-separated path that may be relative to a base
// directory.
func resolvePath(base, path string) string {
	path = filepath.FromSlash(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

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
