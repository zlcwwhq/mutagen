package gitignore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

const (
	// ruleFileName is the name of the rule files that are found in directories
	// throughout a tree.
	ruleFileName = ".gitignore"
	// gitEntryName is the name of Git's own entry in a directory.
	gitEntryName = ".git"
	// excludeFilePath is the slash-separated, root-relative path of the rule
	// file for the root repository.
	excludeFilePath = ".git/info/exclude"
	// maximumRuleFileSize is the maximum size of a rule file that will be read.
	// Larger files cause an error, preventing synchronization without their
	// rules while bounding memory consumption.
	maximumRuleFileSize = 100 * 1024 * 1024
	// maximumPointerFileSize is the maximum size of the files that point to the
	// location of a Git directory (a ".git" file or a "commondir" file) that
	// will be read.
	maximumPointerFileSize = 64 * 1024
)

// IsGitEntry returns true if a name is that of Git's own entry in a directory,
// which is either a directory or a file that points to one. Such entries are
// never synchronized in Git mode (see Ignore). Names are compared
// case-insensitively, since filesystems can treat them as the same entry.
func IsGitEntry(name string) bool {
	return len(name) == len(gitEntryName) && strings.EqualFold(name, gitEntryName)
}

// ShallowestGitEntry returns the shallowest part of a root-relative,
// slash-separated path that identifies a Git entry (see IsGitEntry), which may
// be all of it, or an empty string if the path isn't at or beneath a Git entry.
func ShallowestGitEntry(path string) string {
	components := strings.Split(path, "/")
	for c, component := range components {
		if IsGitEntry(component) {
			return strings.Join(components[:c+1], "/")
		}
	}
	return ""
}

// IsDeliverableRuleFile returns true if a root-relative, slash-separated path
// identifies a rule file that synchronization may deliver to an endpoint, even
// if that endpoint's rules exclude it. This is the case for any file whose base
// name is ".gitignore", unless it's beneath a Git entry, where such a file
// isn't a rule file, and where nothing may be written. This is what to use to
// decide what can be delivered or written.
func IsDeliverableRuleFile(path string) bool {
	return path[strings.LastIndexByte(path, '/')+1:] == ruleFileName && ShallowestGitEntry(path) == ""
}

// AffectsIgnoreEvaluation returns true if a root-relative, slash-separated path
// identifies a file whose contents affect ignore evaluation, so that a change
// to it on disk can change what a scan ignores. This is the case for
// deliverable rule files (see IsDeliverableRuleFile) and for the root
// repository's ".git/info/exclude" file. The latter is inside a Git directory
// and belongs to its endpoint, so this doesn't say anything about what can be
// written.
func AffectsIgnoreEvaluation(path string) bool {
	return path == excludeFilePath || IsDeliverableRuleFile(path)
}

// ruleSet is a list of rules that apply to a subtree, along with the rules that
// apply to the tree beyond it. A rule set is a link in a chain that ends at
// the lowest-precedence rules.
type ruleSet struct {
	// err is the failure to read the rules of this set or of a parent set, in
	// which case there are no rules to evaluate.
	err error
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
	// untraversable records the root-relative paths that a scan wouldn't reach
	// as directories. See traversable.
	untraversable map[string]bool
	// matcher is used to match rules.
	matcher matcher
}

// NewIgnorer returns an ignorer for a single scan of the specified
// synchronization root. The ignorer reads rule files lazily, once each, and
// retains what it reads, so it must not be reused for a scan that needs to
// observe changes to those files. The session ignorer, which may be nil,
// provides ignore rules that take precedence over the rule files and that
// directly control traversal. The returned ignorer is not safe for concurrent
// use by multiple Goroutines. Callers have to use EnsureDirectory before
// processing a directory's contents, since Ignore can't report that the rules
// for them couldn't be read. Rule files are only read from directories that a
// scan would traverse, so a path beneath anything else, including a symbolic
// link, is only subject to the rules of its parents.
func NewIgnorer(root string, session ignore.Ignorer) ignore.DirectoryIgnorer {
	return &ignorer{
		root:          root,
		session:       session,
		directories:   make(map[string]*ruleSet),
		untraversable: make(map[string]bool),
	}
}

// EnsureDirectory implements ignore.DirectoryIgnorer.EnsureDirectory.
func (i *ignorer) EnsureDirectory(path string) error {
	if rules := i.rulesFor(path); rules != nil {
		return rules.err
	}
	return nil
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
	if IsGitEntry(base) {
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
	var parentDirectory string
	var prefixLength int
	if directory == "" {
		parent = i.excludeRules()
	} else {
		if slash := strings.LastIndexByte(directory, '/'); slash >= 0 {
			parentDirectory = directory[:slash]
		}
		parent = i.load(parentDirectory)
		prefixLength = len(directory) + 1
	}

	// A failure to read a parent's rules applies to the whole subtree, whatever
	// this directory's own rules say.
	if parent != nil && parent.err != nil {
		i.directories[directory] = parent
		return parent
	}

	// Read the directory's own rules, if it's one that can have any. If it
	// doesn't have any, then the chain is simply the parent's.
	rules := parent
	if traversable, err := i.traversable(directory, parentDirectory); err != nil {
		rules = &ruleSet{err: fmt.Errorf("unable to read ignore rules: %w", err)}
	} else if traversable {
		path := filepath.Join(i.root, filepath.FromSlash(directory), ruleFileName)
		if contents, err := readRegularFile(path, maximumRuleFileSize); err != nil {
			rules = &ruleSet{err: fmt.Errorf("unable to read ignore rules: %w", err)}
		} else if parsed := parseRules(contents); len(parsed) > 0 {
			rules = &ruleSet{rules: parsed, prefixLength: prefixLength, parent: parent}
		}
	}

	// Remember the result.
	i.directories[directory] = rules
	return rules
}

// traversable determines whether or not a scan would reach a path as a
// directory, given the path of its parent, which must have been checked before
// it. Paths are also evaluated when something else is in the way of a directory
// that's yet to be created, such as a symbolic link that's about to be replaced
// by one. A scan doesn't follow symbolic links, so what's at a link's target,
// which can be anything, including a cycle or another directory's rules, has
// nothing to do with the paths beneath the link, and rule files mustn't be read
// through it. The root itself is always traversed, since a symbolic link there
// is how the user has chosen to specify it. A scan also only reaches directories
// by the names that it finds in their parents, so a name that refers to a
// directory itself or to its parent, or that spans several directories, isn't
// traversable. Paths that aren't from a scan can have such names, and rule files
// outside of the root have no say about anything.
func (i *ignorer) traversable(directory, parent string) (bool, error) {
	if directory == "" {
		return true, nil
	} else if i.untraversable[parent] || !isEntryName(directory[strings.LastIndexByte(directory, '/')+1:]) {
		i.untraversable[directory] = true
		return false, nil
	}
	info, err := os.Lstat(filepath.Join(i.root, filepath.FromSlash(directory)))
	if err == nil && info.IsDir() {
		return true, nil
	} else if err == nil || os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		i.untraversable[directory] = true
		return false, nil
	}
	return false, err
}

// isEntryName determines whether or not a path component can be the name of an
// entry in a directory, as opposed to a reference to the directory itself or to
// its parent, or several components according to the system's path separator.
func isEntryName(component string) bool {
	return component != "" && component != "." && component != ".." &&
		!strings.ContainsRune(component, filepath.Separator)
}

// excludeRules returns the rule set for the root repository's exclude file, or
// nil if there is no such file or it contains no rules.
func (i *ignorer) excludeRules() *ruleSet {
	if !i.excludeLoaded {
		i.excludeLoaded = true
		if path, err := excludeFileLocation(i.root); err != nil {
			i.exclude = &ruleSet{err: fmt.Errorf("unable to locate ignore rules: %w", err)}
		} else if path != "" {
			if contents, err := readRegularFile(path, maximumRuleFileSize); err != nil {
				i.exclude = &ruleSet{err: fmt.Errorf("unable to read ignore rules: %w", err)}
			} else if rules := parseRules(contents); len(rules) > 0 {
				i.exclude = &ruleSet{rules: rules}
			}
		}
	}
	return i.exclude
}

// readRegularFile reads a file if (and only if) it is a regular file (i.e. not
// a symbolic link or anything else) that is no larger than the specified size.
// If there's no such file (including because a parent isn't a directory) or
// it's something other than a regular file, then it returns nil. If it can't be
// read or it's too large, then it returns an error, because treating that like
// an empty file would synchronize what its rules ignore.
func readRegularFile(path string, limit int64) ([]byte, error) {
	// Check the type and size of the file.
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, nil
	} else if info.Size() > limit {
		return nil, fmt.Errorf("%s exceeds the rule file size limit", path)
	}

	// Open the file and read it, taking care not to read more than expected if
	// the file grows.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	} else if int64(len(contents)) > limit {
		return nil, fmt.Errorf("%s exceeds the rule file size limit", path)
	}
	return contents, nil
}

// excludeFileLocation determines the location of the exclude file for the
// repository at a synchronization root. It returns an empty string if the root
// doesn't appear to be the root of a repository. The root's ".git" entry
// may either be a directory, in which case it contains the exclude file, or a
// file that points to the Git directory (as is the case for submodules and
// linked worktrees). If the Git directory in turn points to a common directory
// (as is the case for linked worktrees), then that is where the exclude file
// is found.
func excludeFileLocation(root string) (string, error) {
	// Check the type of the root's ".git" entry. We deliberately don't follow
	// symbolic links.
	dotGit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotGit)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Join(dotGit, "info", "exclude"), nil
	} else if !info.Mode().IsRegular() {
		return "", nil
	}

	// Read the location of the Git directory from the file.
	contents, err := readRegularFile(dotGit, maximumPointerFileSize)
	if err != nil {
		return "", err
	} else if contents == nil {
		return "", nil
	}
	pointer, ok := parsePointer(contents, "gitdir:")
	if !ok {
		return "", nil
	}
	gitDirectory := resolvePath(root, pointer)

	// Check whether or not the Git directory points to a common directory.
	if contents, err := readRegularFile(filepath.Join(gitDirectory, "commondir"), maximumPointerFileSize); err != nil {
		return "", err
	} else if contents != nil {
		if pointer, ok := parsePointer(contents, ""); ok {
			return filepath.Join(resolvePath(gitDirectory, pointer), "info", "exclude"), nil
		}
	}
	return filepath.Join(gitDirectory, "info", "exclude"), nil
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
