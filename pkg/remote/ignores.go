package remote

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	dockerignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/docker"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/gitignore"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

// Ignores describes a session's effective ignores, which are the same for both
// of its endpoints, so that submissions can determine whether the session
// synchronizes the directory corresponding to theirs (see
// Selection.Corresponding), or other paths, such as those that remote --pull
// copies (see Target.Ignores).
type Ignores struct {
	// Syntax is the ignore syntax, which is never the default.
	Syntax ignore.Syntax
	// Patterns are the ignore patterns, including any default ignores.
	Patterns []string
	// VCS indicates whether VCS directories are ignored.
	VCS bool
	// Git indicates whether the session is in Git mode, in which the rules of
	// .gitignore files and of the root repository's .git/info/exclude apply
	// beneath the patterns above and entries named .git (files as well as
	// directories) are never synchronized. Those rules aren't part of the
	// session's configuration: they're files in the synchronized tree,
	// which each endpoint reads from its own copy, so they're evaluated on the
	// system that evaluates the ignores, beneath the root that it has for the
	// session (see WithRoot). Ignores that don't know their root can only tell
	// that entries named .git are excluded.
	Git bool `json:",omitempty"`
	// root is the session's synchronization root on this system, with symbolic
	// links resolved, beneath which Git mode reads rule files. It says where the
	// ignores are evaluated rather than what the session ignores, so it isn't
	// serialized with them: loading a registration binds it (see WithRoot).
	root string
}

// WithRoot returns a copy of the ignores that evaluates Git mode beneath the
// session's synchronization root on this system, which should have its
// symbolic links resolved, since the paths that the ignores evaluate are
// relative to it. Ignores that aren't in Git mode never read the filesystem.
// It returns nil for nil ignores.
func (i *Ignores) WithRoot(root string) *Ignores {
	if i == nil {
		return nil
	}
	bound := *i
	bound.root = root
	return &bound
}

// ExcludedInAnyForm returns the slash-separated path, relative to the
// synchronization root, of the entry that keeps the entry at the specified
// path, which is a directory if directory is true and another kind of entry
// otherwise, from being synchronized in at least one of the forms in which
// scans could see the path (see below). That's either the entry itself or one
// of its parents, in the form given, or an empty string if there's none.
// Entries are excluded as scans exclude them, with directories excluded by not
// traversing them: an ignored directory is traversed only if content beneath
// it may be unignored, in which case some of that content may be
// synchronized, and it isn't considered excluded, while content beneath it
// that isn't unignored is excluded once nothing beneath it can be. Ignores
// that can't be compiled exclude nothing, since sessions validate their
// ignores.
//
// In Git mode, the rules of .gitignore files and .git/info/exclude apply as
// scans apply them (see gitignore.NewIgnorer), and are read from disk each
// time, so changes to them take effect immediately.
//
// Scans needn't see a path in the form given: they recompose names on
// filesystems that decompose them, and filesystems that look up names
// regardless of their Unicode normalization form let a path be named in a form
// other than the stored one (as in $PWD, which os.Getwd returns). A path may
// thus be seen as given, or in its NFC or NFD form, while on filesystems that
// store names as given, the other forms name distinct entries, which ignores
// can treat differently. Exclusion in any form errs towards reporting paths as
// excluded, which suits refusing operations that require the session to
// synchronize a path, such as running commands in the directory corresponding
// to the current one (see Selection.Corresponding), but not permitting writes
// that require the session to ignore it, which ExcludedInEveryForm suits.
func (i *Ignores) ExcludedInAnyForm(path string, directory bool) string {
	return i.excluded(path, directory, false)
}

// ExcludedInEveryForm is like ExcludedInAnyForm, but only reports the entry at
// the specified path as excluded if it's excluded in every form in which scans
// could see the path, and errs towards reporting paths as synchronized, which
// suits permitting writes that would race with synchronization if the session
// synchronized the path, such as those of remote --pull, and determining the
// sessions that may synchronize a path (see Synchronizing). The entry that
// keeps it from being synchronized is then the shallowest one that's excluded
// in every form. Ignores that match a name in only some forms, such as literal
// patterns containing characters that normalization changes, thus don't
// exclude it, even on filesystems on which the other forms name distinct
// entries.
func (i *Ignores) ExcludedInEveryForm(path string, directory bool) string {
	return i.excluded(path, directory, true)
}

// ExclusionSource identifies what excludes an entry from being synchronized
// in Git mode, so that diagnostics can say where an exclusion comes from.
type ExclusionSource uint8

const (
	// ExcludedBySession indicates the session's own patterns and VCS
	// directories. It's also the source of every exclusion outside Git mode.
	ExcludedBySession ExclusionSource = iota
	// ExcludedByGitRules indicates a rule of a .gitignore file or of the root
	// repository's .git/info/exclude.
	ExcludedByGitRules
	// ExcludedByGitEntry indicates an entry named .git, which Git mode never
	// synchronizes, whether it's a file or a directory.
	ExcludedByGitEntry
)

// ExclusionSource determines what keeps the entry at the specified path, which
// is a directory if directory is true and another kind of entry otherwise,
// from being synchronized, given that ExcludedInEveryForm or ExcludedInAnyForm
// reported it as the excluded entry. Where several things exclude the entry,
// .git entries take precedence, since nothing can unignore them, then the
// session's own ignores, then rules, so that the source is the one that would
// still exclude the entry if the sources that follow it were removed.
func (i *Ignores) ExclusionSource(path string, directory bool) ExclusionSource {
	if i == nil || !i.Git {
		return ExcludedBySession
	}
	for _, component := range strings.Split(path, "/") {
		if gitignore.IsGitEntry(component) {
			return ExcludedByGitEntry
		}
	}
	session := &Ignores{Syntax: i.Syntax, Patterns: i.Patterns, VCS: i.VCS}
	if session.ExcludedInAnyForm(path, directory) != "" {
		return ExcludedBySession
	} else if i.excluded(path, directory, false) != "" {
		return ExcludedByGitRules
	}
	return ExcludedBySession
}

// gitEntryIgnorer is the ignorer of Git mode when the root is unknown, so that
// no rule files can be read: it only ignores entries named .git, like the
// ignorer of Git mode does before anything else, and defers to the session's
// ignorer for everything else.
type gitEntryIgnorer struct {
	// session is the session's ignorer.
	session ignore.Ignorer
}

// Ignore implements ignore.Ignorer.Ignore.
func (i gitEntryIgnorer) Ignore(path string, directory bool) (ignore.IgnoreStatus, bool) {
	if gitignore.IsGitEntry(path[strings.LastIndexByte(path, '/')+1:]) {
		return ignore.IgnoreStatusIgnored, false
	}
	return i.session.Ignore(path, directory)
}

// ignorer creates the ignorer that scans use for the ignores, as far as the
// ignores determine it, or returns nil if they can't be compiled or their
// syntax is unresolved. In Git mode, the ignorer reads rule files as it
// evaluates paths and retains what it reads, so it's created for each use,
// rather than kept, since it wouldn't observe later changes to rule files.
func (i *Ignores) ignorer() ignore.Ignorer {
	var ignorer ignore.Ignorer
	var err error
	switch i.Syntax {
	case ignore.Syntax_SyntaxMutagen:
		ignorer, err = mutagenignore.NewIgnorer(i.Patterns)
	case ignore.Syntax_SyntaxDocker:
		ignorer, err = dockerignore.NewIgnorer(i.Patterns)
	default:
		return nil
	}
	if err != nil {
		return nil
	} else if i.VCS {
		ignorer = ignore.IgnoreVCS(ignorer)
	}

	// The VCS ignorer only matches directories, whereas Git mode also never
	// synchronizes the .git files of worktrees and submodules. Without a root,
	// there are no rule files to read, and only those entries are known.
	if i.Git && i.root != "" {
		ignorer = gitignore.NewIgnorer(i.root, ignorer)
	} else if i.Git {
		ignorer = gitEntryIgnorer{ignorer}
	}
	return ignorer
}

// excluded implements ExcludedInAnyForm and, if every is true,
// ExcludedInEveryForm.
func (i *Ignores) excluded(path string, directory, every bool) string {
	if i == nil || path == "" {
		return ""
	}
	ignorer := i.ignorer()
	if ignorer == nil {
		return ""
	}

	// Determine the depth at which each form is excluded. Normalization
	// doesn't affect separators, so the forms have the same number of
	// components, and the excluded entry is reported in the form given. If
	// every form must be excluded, then the deepest of these depths is the
	// shallowest one at which every form is excluded, since content beneath an
	// excluded entry is excluded too.
	components := strings.Split(path, "/")
	result := 0
	for _, form := range []string{path, norm.NFC.String(path), norm.NFD.String(path)} {
		excluded, err := ignore.Excluded(ignorer, form, directory)
		if err != nil {
			// Rules that can't be read don't exclude anything, which errs
			// towards reporting the path as synchronized, and thus towards
			// refusing writes. Scans report the failure for the subtree.
			return ""
		}
		depth := 0
		if excluded != "" {
			depth = strings.Count(excluded, "/") + 1
		}
		if !every && depth > 0 {
			result = depth
			break
		} else if every && depth == 0 {
			return ""
		}
		result = max(result, depth)
	}
	if result == 0 {
		return ""
	}
	return strings.Join(components[:result], "/")
}
