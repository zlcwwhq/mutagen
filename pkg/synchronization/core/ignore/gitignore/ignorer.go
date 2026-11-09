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
	"unicode/utf8"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

const (
	// ruleFileName is the name of the rule files that are found in directories
	// throughout a tree.
	ruleFileName = ".gitignore"
	// gitEntryName is the name of Git's own entry in a directory.
	gitEntryName = ".git"
	// excludeFileSuffix is the slash-separated end of the path of a
	// repository's exclude file, which is the rule file for the repository as a
	// whole, within its Git directory.
	excludeFileSuffix = "/info/exclude"
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
// which is either a directory or a file that points to one, or if a filesystem
// can treat it as that name. Such entries are never synchronized in Git mode
// (see Ignore), and nothing at or beneath them is ever written. A name that's
// an ordinary one on one endpoint can be ".git" on another, so names are
// classified as Git classifies them when it protects ".git" from paths that
// it checks out (as it does with core.protectNTFS, which is enabled by
// default on every system, and with core.protectHFS), whatever the systems
// involved: ".git" in any case, as well as the names that NTFS and HFS+ treat
// as ".git" (see isNTFSGitEntry and isHFSGitEntry).
func IsGitEntry(name string) bool {
	return isNTFSGitEntry(name) || isHFSGitEntry(name)
}

// isNTFSGitEntry returns true if NTFS (as used by Windows) can treat a name as
// ".git", as Git's is_ntfs_dotgit determines. That's the case for ".git" in any
// case and for its short name, "git~1" (in any case), when followed by any
// number of trailing dots and spaces, which Windows strips, and optionally by
// a colon, which begins the name of a stream of the entry (such as
// ".git::$INDEX_ALLOCATION"). Windows also treats a backslash as a path
// separator, so a name containing backslashes counts if any part of it does,
// as it does for Git.
func isNTFSGitEntry(name string) bool {
	for {
		part, rest, separated := strings.Cut(name, "\\")
		var tail string
		if len(part) >= 4 && strings.EqualFold(part[:4], gitEntryName) {
			tail = part[4:]
		} else if len(part) >= 5 && strings.EqualFold(part[:3], "git") && part[3:5] == "~1" {
			tail = part[5:]
		} else if separated {
			name = rest
			continue
		} else {
			return false
		}
		for i := 0; ; i++ {
			if i == len(tail) || tail[i] == ':' {
				return true
			} else if tail[i] != '.' && tail[i] != ' ' {
				break
			}
		}
		if !separated {
			return false
		}
		name = rest
	}
}

// isHFSIgnorable returns true if HFS+ (as used by macOS) ignores a code point in
// names, as Git's next_hfs_char determines.
func isHFSIgnorable(r rune) bool {
	return (r >= 0x200c && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
		(r >= 0x206a && r <= 0x206f) || r == 0xfeff
}

// isHFSGitEntry returns true if HFS+ can treat a name as ".git", as Git's
// is_hfs_dotgit determines. That's the case for ".git" in any case with any
// code points that HFS+ ignores anywhere in it (see isHFSIgnorable). Like Git,
// we treat invalid UTF-8 as the end of the name, which can only make more
// names count.
func isHFSGitEntry(name string) bool {
	// next returns the next code point that HFS+ doesn't ignore, or zero at the
	// end of the name or at invalid UTF-8.
	next := func() rune {
		for name != "" {
			r, size := utf8.DecodeRuneInString(name)
			if r == utf8.RuneError && size <= 1 {
				name = ""
				return 0
			}
			name = name[size:]
			if !isHFSIgnorable(r) {
				return r
			}
		}
		return 0
	}

	// Compare the name with ".git", ignoring the case of ASCII letters.
	for _, expected := range gitEntryName {
		r := next()
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r != expected {
			return false
		}
	}
	return next() == 0
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
//
// Base names are compared case-insensitively, on every system. On a
// case-insensitive filesystem, ".GITIGNORE" provides the rules of its
// directory, for Git and for the ignorer alike, since both look the file up as
// ".gitignore", and an endpoint can't tell how the other endpoint's filesystem
// treats the name. On a case-sensitive filesystem, such a file merely counts
// as a rule file without providing rules, which costs at most a cycle that
// only delivers it, a full scan when it changes, and its delivery into a
// directory that the endpoint's rules exclude, like that of any rule file.
func IsDeliverableRuleFile(path string) bool {
	base := path[strings.LastIndexByte(path, '/')+1:]
	return len(base) == len(ruleFileName) && strings.EqualFold(base, ruleFileName) && ShallowestGitEntry(path) == ""
}

// AffectsIgnoreEvaluation returns true if a root-relative, slash-separated path
// identifies a file whose contents affect ignore evaluation, so that a change
// to it on disk can change what a scan ignores. This is the case for
// deliverable rule files (see IsDeliverableRuleFile) and for the exclude files
// of repositories: the root repository's ".git/info/exclude" and those of
// nested repositories, which are either in their own Git directories (such as
// "sub/.git/info/exclude") or in Git directories elsewhere that their ".git"
// files point to (such as ".git/modules/sub/info/exclude" for a submodule).
// Only the ".git" entries on disk say which Git directory belongs to which
// working tree, so any path beneath a Git entry that ends in "info/exclude"
// counts, which errs towards noticing changes. Exclude files are inside Git
// directories and belong to their endpoint, so this doesn't say anything about
// what can be written.
func AffectsIgnoreEvaluation(path string) bool {
	if IsDeliverableRuleFile(path) {
		return true
	} else if !strings.HasSuffix(path, excludeFileSuffix) {
		return false
	}
	gitEntry := ShallowestGitEntry(path)
	return gitEntry != "" && len(gitEntry) <= len(path)-len(excludeFileSuffix)
}

// ruleSet is a list of rules that apply to a subtree, along with the rules that
// apply to the tree beyond it. A rule set is a link in a chain that ends at
// the lowest-precedence rules, which are those of the exclude file of the
// repository that the subtree belongs to (see ignorer.load).
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
	// gitRoot is the synchronization root with any symbolic links in it
	// resolved, from which repositories are located (see readExcludeFile).
	gitRoot string
	// session is the session-specific ignorer, if any.
	session ignore.Ignorer
	// directories maps root-relative directory paths to the chain of rule sets
	// that apply to entries in those directories. The chain may be nil. Each
	// directory's chain is computed once, so the rule files that it reads,
	// including the exclude files of the root repository and of any nested
	// repositories, are only read once.
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
	// untraversable records the root-relative paths that a scan wouldn't reach
	// as directories. See traversable.
	untraversable map[string]bool
	// repositories records what the root and the top levels of nested
	// repositories' working trees contributed to the rules. See Repositories.
	repositories map[string]repositoryState
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
//
// The rules are those that Git applies in the repository that a path belongs
// to. A directory other than the root whose ".git" entry locates a Git
// directory that Git accepts is the top level of a nested repository's working
// tree (such as a submodule's), and the rules of the directories above it don't
// apply beneath it, where the nested repository's own exclude file takes their
// place. Those rules still decide about the directory itself, as an entry of
// its parent. A failure to read them still applies beneath it, though, and the
// session ignorer and the exclusion of ".git" entries apply everywhere. See
// load for details.
//
// Repositories are located from the physical location of the root, as Git
// locates them from that of its working directory, so that the result doesn't
// depend on whether or not the root is specified by a symbolic link (see
// readExcludeFile).
func NewIgnorer(root string, session ignore.Ignorer) ignore.DirectoryIgnorer {
	return &ignorer{
		root:          root,
		gitRoot:       physicalPath(root),
		session:       session,
		directories:   make(map[string]*ruleSet),
		untraversable: make(map[string]bool),
		repositories:  make(map[string]repositoryState),
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
//
// A directory's own rules take precedence over those of its parent, which take
// precedence over those of its parent in turn, up to the root, whose parent is
// the root repository's exclude file. That's how Git evaluates the paths of a
// repository, but Git evaluates the paths beneath a nested repository's top
// level (such as a submodule's) for that repository alone, without the rules
// of the directories above it, which belong to another repository. So the
// chain of a directory other than the root that contains a ".git" entry
// doesn't continue with its parent's: it continues with the nested
// repository's exclude file (see repositoryRules), whose rules are relative to
// the directory, and it ends there. The rules above the directory still decide
// whether the directory itself is ignored, since it's evaluated as an entry of
// its parent. Nested repositories are recognized by their ".git" entries and the
// Git directories that those locate, as Git recognizes them, but without
// involving Git (see excludeFileLocation). A ".git" entry that Git doesn't
// accept doesn't make a directory a nested repository's top level, since Git
// then treats the directory as part of the enclosing repository, whose rules
// continue to apply beneath it.
//
// A failure to read the rules of a parent (or of the root repository's
// exclude file) applies to the whole subtree, even beneath the top level of a
// nested repository, whose own rules would otherwise replace them, since the
// rules that couldn't be read might ignore it entirely. A failure to determine
// whether a directory is a nested repository's top level, or to read its
// repository's exclude file, applies to the directory's subtree, just as a
// failure to read the root repository's exclude file applies to the whole
// tree.
func (i *ignorer) load(directory string) *ruleSet {
	// Check if we've already done this work.
	if rules, ok := i.directories[directory]; ok {
		return rules
	}

	// The rules of the parent directory also apply (with lower precedence), and
	// the root's parent is the root repository's exclude file. Whether or not
	// the root contains a ".git" entry, it's the top level of whatever
	// repository it's in, since rule files above it are never read.
	var parent *ruleSet
	var parentDirectory string
	var prefixLength int
	if directory == "" {
		_, parent = i.repositoryRules(directory)
	} else {
		if slash := strings.LastIndexByte(directory, '/'); slash >= 0 {
			parentDirectory = directory[:slash]
		}
		parent = i.load(parentDirectory)
		prefixLength = len(directory) + 1
	}

	// A failure to read a parent's rules applies to the whole subtree, whatever
	// this directory's own rules say, and whether or not it's the top level of
	// a nested repository.
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
		// If the directory is the top level of a nested repository, then the
		// chain restarts with that repository's exclude file. Its rules apply
		// beneath the directory in place of the parent's, which are those of
		// another repository, so that a rule such as "*.log" in the root
		// doesn't hide a file that the nested repository tracks. If its rules
		// can't be read, then neither can those of the directory.
		base := parent
		if directory != "" {
			if repository, exclude := i.repositoryRules(directory); repository {
				base = exclude
			}
		}

		// Read the directory's own rules, which take precedence over the base.
		rules = base
		if base == nil || base.err == nil {
			path := filepath.Join(i.root, filepath.FromSlash(directory), ruleFileName)
			if contents, err := readRegularFile(path, maximumRuleFileSize); err != nil {
				rules = &ruleSet{err: fmt.Errorf("unable to read ignore rules: %w", err)}
			} else if parsed := parseRules(contents); len(parsed) > 0 {
				rules = &ruleSet{rules: parsed, prefixLength: prefixLength, parent: base}
			}
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

// repositoryRules determines whether or not a directory, specified by its
// root-relative path (which is empty for the synchronization root), is the top
// level of a repository's working tree (see excludeFileLocation), and returns
// the rule set for that repository's exclude file, whose rules are relative to
// the directory. The rule set is nil if the directory isn't a repository's top
// level, if its repository has no exclude file, or if that file contains no
// rules. If the exclude file can't be located or read, including because the
// directory's ".git" entry can't be examined, then the rule set records the
// error, and the directory counts as a repository's top level, so that the
// error replaces any rules that would otherwise apply beneath it. The caller is
// responsible for retaining the result, so that the file is only read once.
// The result is recorded for the root and for every directory that has a
// ".git" entry, whether or not Git accepts it (see Repositories).
func (i *ignorer) repositoryRules(directory string) (bool, *ruleSet) {
	// Read the exclude file and record the result.
	location, contents, err := readExcludeFile(i.gitRoot, directory)
	if directory == "" || location.present {
		i.repositories[directory] = newRepositoryState(location, contents, err)
	}
	if err != nil {
		return true, &ruleSet{err: err}
	}

	// Parse the exclude file.
	rules := parseRules(contents)
	if len(rules) == 0 {
		return location.repository, nil
	}
	var prefixLength int
	if directory != "" {
		prefixLength = len(directory) + 1
	}
	return location.repository, &ruleSet{rules: rules, prefixLength: prefixLength}
}

// readExcludeFile locates the repository of a directory, specified by its
// root-relative path (which is empty for the synchronization root), which
// determines whether or not the directory is the top level of a repository's
// working tree (see excludeFileLocation), and reads the exclude file of that
// repository, if that file's rules apply beneath it. A nested repository that
// Git doesn't accept has no rules of its own, but the root's exclude file is
// read wherever it can be located, since the root is the top level of whatever
// repository it's in, and its exclude file can only add rules. The contents are
// nil if there's no exclude file to read. If the exclude file can't be located
// or read, including because the directory's ".git" entry can't be examined,
// then the directory counts as a repository's top level, and the error is
// returned.
//
// The root has to be specified by its physical location (see physicalPath).
// Git resolves a relative location in a ".git" file against the physical
// location of the directory that contains the file, so a parent reference
// ("..") in it leads to the parent of the directory's physical location. That
// matters for a root that's specified by a symbolic link, since a submodule's
// ".git" file refers to its Git directory by a relative location that leads
// out of its working tree (such as "../.git/modules/<name>"), and so can a
// linked worktree's. Directories beneath the root that a scan reaches aren't
// symbolic links. The root's ".git" entry may be one, which is followed.
func readExcludeFile(root, directory string) (repositoryLocation, []byte, error) {
	// Locate the exclude file.
	location, err := excludeFileLocation(filepath.Join(root, filepath.FromSlash(directory)), directory == "")
	if err != nil {
		return location, nil, fmt.Errorf("unable to locate ignore rules: %w", err)
	} else if location.exclude == "" || (directory != "" && !location.repository) {
		return location, nil, nil
	}

	// Read the exclude file.
	contents, err := readRegularFile(location.exclude, maximumRuleFileSize)
	if err != nil {
		location.repository = true
		return location, nil, fmt.Errorf("unable to read ignore rules: %w", err)
	}
	return location, contents, nil
}

// readRegularFile reads a file if (and only if) it is a regular file (i.e. not
// a symbolic link or anything else) that is no larger than the specified size.
// If there's no such file (including because a parent isn't a directory, or
// because the path can't name anything on this system, such as a path from a
// pointer file that contains a line feed on Windows) or it's something other
// than a regular file, then it returns nil. If it can't be
// read or it's too large, then it returns an error, because treating that like
// an empty file would synchronize what its rules ignore.
func readRegularFile(path string, limit int64) ([]byte, error) {
	// Check the type and size of the file.
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) || isInvalidName(err) {
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

// repositoryLocation is what a directory's ".git" entry locates (see
// excludeFileLocation).
type repositoryLocation struct {
	// present indicates whether or not the directory has a ".git" entry, which
	// is assumed if the entry can't be examined.
	present bool
	// repository indicates whether or not the directory is the top level of a
	// repository's working tree.
	repository bool
	// gitDirectory is the location of the Git directory that the ".git" entry
	// points to, whether or not Git accepts it, or an empty string if it
	// doesn't point anywhere.
	gitDirectory string
	// exclude is the location of the exclude file of the Git directory's
	// repository, or an empty string if there's no Git directory.
	exclude string
}

// excludeFileLocation determines whether or not a directory is the top level of
// a repository's working tree, as Git determines it, along with the location of
// the Git directory that the directory's ".git" entry points to, and that of
// the exclude file of its repository. The ".git" entry may either be a
// directory, which is then the Git directory, or a file that points to the Git
// directory (as is the case for submodules and linked worktrees, see
// parseGitFile). If the Git directory in turn points to a common directory (as
// is the case for linked worktrees), then that is where the exclude file is
// found. The locations are empty strings if there's no ".git" entry, or if a
// ".git" file doesn't point anywhere.
//
// Like Git, we only consider the directory the top level of a repository's
// working tree if its ".git" entry locates a Git directory that Git accepts
// (see isGitDirectory), as Git's is_nonbare_repository_dir does when it
// decides whether a directory is a nested repository. Otherwise, such as for
// an empty ".git" directory, or for a ".git" file that points to a Git
// directory that doesn't exist, Git treats the directory as part of the
// enclosing repository, whose rules continue to apply in it. The location of
// an exclude file can still be returned in that case. If the ".git" entry, a
// ".git" file, or a "commondir" file can't be examined or read, then there's
// no telling whose rules apply, so the directory counts as the top level of a
// repository (as it does for Git if a ".git" file can't be read), and the
// error is returned.
//
// A symbolic link named ".git" doesn't count, even though Git follows it,
// since rule files are never read through symbolic links, and the directory
// is then treated as an ordinary one, unless follow is true, which is the case
// for the synchronization root. The root is exempt from that rule, as it is
// when it's specified by a symbolic link itself (see traversable), and there's
// nothing above it whose rules a link could replace, so its ".git" entry is
// followed, as Git follows it, and a relative location in a ".git" file that
// it leads to is resolved against the directory, as Git resolves it. A link
// that doesn't lead anywhere counts as a ".git" file that doesn't point
// anywhere. The entry is looked up by name, so whether ".GIT" counts depends
// on whether the filesystem distinguishes case, as it does for Git.
func excludeFileLocation(directory string, follow bool) (repositoryLocation, error) {
	// Check the type of the directory's ".git" entry. We deliberately don't
	// follow symbolic links, unless requested.
	dotGit := filepath.Join(directory, gitEntryName)
	info, err := os.Lstat(dotGit)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return repositoryLocation{}, nil
	} else if err != nil {
		return repositoryLocation{present: true, repository: true}, err
	} else if follow && info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(dotGit)
		if err == nil {
			info, err = os.Lstat(target)
		}
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			return repositoryLocation{present: true}, nil
		} else if err != nil {
			return repositoryLocation{present: true, repository: true}, err
		}
		dotGit = target
	}

	// Determine the location of the Git directory. If a ".git" file has been
	// replaced by something other than a regular file in the meantime, then
	// it's treated as one that doesn't point anywhere.
	location := repositoryLocation{present: true}
	if info.IsDir() {
		location.gitDirectory = dotGit
	} else if !info.Mode().IsRegular() {
		return location, nil
	} else if contents, err := readRegularFile(dotGit, maximumPointerFileSize); err != nil {
		location.repository = true
		return location, err
	} else if pointer, ok := parseGitFile(contents); !ok {
		return location, nil
	} else {
		location.gitDirectory = resolvePath(directory, pointer)
	}

	// Check whether or not the Git directory points to a common directory.
	commonDirectory := location.gitDirectory
	if contents, err := readRegularFile(joinPath(location.gitDirectory, "commondir"), maximumPointerFileSize); err != nil {
		location.repository = true
		return location, err
	} else if contents != nil {
		if pointer, ok := parsePointer(contents, ""); ok {
			commonDirectory = resolvePath(location.gitDirectory, pointer)
		}
	}

	// Determine whether or not Git accepts the Git directory.
	location.repository = isGitDirectory(location.gitDirectory, commonDirectory)
	location.exclude = joinPath(commonDirectory, "info", "exclude")
	return location, nil
}

// parseGitFile extracts the location of a Git directory from the contents of a
// ".git" file, as Git's read_gitfile_gently does: the contents have to begin
// with "gitdir: ", and the location is everything that follows, apart from any
// trailing line feeds and carriage returns. It may be relative to the
// directory that contains the file.
func parseGitFile(contents []byte) (string, bool) {
	location, ok := bytes.CutPrefix(contents, []byte("gitdir: "))
	if !ok {
		return "", false
	}
	location = bytes.TrimRight(location, "\n\r")
	return string(location), len(location) > 0
}

// isGitDirectory determines whether or not Git accepts a directory as a Git
// directory, given the location of its common directory (which is the Git
// directory itself, unless it points to another), as Git's is_git_directory
// does: its HEAD has to be valid (see isValidHead), and its common directory
// has to contain "objects" and "refs" entries that Git can search (see
// isSearchable). Like Git, we treat any failure to examine them as meaning that
// the Git directory isn't one.
func isGitDirectory(gitDirectory, commonDirectory string) bool {
	return isValidHead(joinPath(gitDirectory, "HEAD")) &&
		isSearchable(joinPath(commonDirectory, "objects")) &&
		isSearchable(joinPath(commonDirectory, "refs"))
}

// maximumHeadLength is the number of bytes of a HEAD file that Git examines to
// validate it.
const maximumHeadLength = 255

// isValidHead determines whether or not the HEAD of a Git directory is valid, as
// Git's validate_headref does: it has to be either a symbolic link whose target
// begins with "refs/", or a file that begins with "ref:", optional whitespace,
// and "refs/" (as with any symbolic reference, including that of a repository
// with the reftable format), or with an object name in hexadecimal (as with a
// detached HEAD). Only the beginning of the file is examined. Git would open
// anything else that HEAD may be, which we don't do, since opening a named pipe
// could block, and since nothing other than a symbolic link or a regular file
// can be valid anyway.
func isValidHead(path string) bool {
	// Check the type of the file.
	info, err := os.Lstat(path)
	if err != nil {
		return false
	} else if info.Mode()&os.ModeSymlink != 0 {
		// Git for Windows reports link targets with forward slashes.
		target, err := os.Readlink(path)
		return err == nil && strings.HasPrefix(filepath.ToSlash(target), "refs/")
	} else if !info.Mode().IsRegular() {
		return false
	}

	// Read the beginning of the file.
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	contents := make([]byte, maximumHeadLength)
	length, err := io.ReadFull(file, contents)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false
	}
	contents = contents[:length]

	// Check for a symbolic reference. Git's whitespace doesn't include vertical
	// tabs and form feeds.
	if reference, ok := bytes.CutPrefix(contents, []byte("ref:")); ok {
		if bytes.HasPrefix(bytes.TrimLeft(reference, " \t\n\r"), []byte("refs/")) {
			return true
		}
	}

	// Check for an object name. Git accepts names of either length, but names
	// of the longer one begin with what would be a name of the shorter one.
	const objectNameLength = 40
	if len(contents) < objectNameLength {
		return false
	}
	for _, b := range contents[:objectNameLength] {
		if !isDigit(b) && !(b >= 'a' && b <= 'f') && !(b >= 'A' && b <= 'F') {
			return false
		}
	}
	return true
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
// directory, as Git resolves the locations in ".git" files and "commondir"
// files: by appending a relative path to the base directory, without any
// lexical processing (see joinPath).
func resolvePath(base, path string) string {
	path = filepath.FromSlash(path)
	if filepath.IsAbs(path) {
		return path
	}
	return joinPath(base, path)
}

// joinPath appends path components to a path, separating them as necessary,
// without the lexical processing of filepath.Join, which would cancel a parent
// reference ("..") with the component before it. The system resolves a parent
// reference after following any symbolic link that the component before it is,
// on POSIX systems, which leads to the parent of the link's target, and that's
// what Git does too, since it passes such paths to the system as they are. On
// Windows, the system resolves parent references lexically, as Git for Windows
// does.
func joinPath(base string, components ...string) string {
	for _, component := range components {
		if base != "" && !os.IsPathSeparator(base[len(base)-1]) {
			base += string(filepath.Separator)
		}
		base += component
	}
	return base
}

// physicalPath returns a path with any symbolic links in it resolved, or the
// path as it is if they can't be resolved.
func physicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
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
