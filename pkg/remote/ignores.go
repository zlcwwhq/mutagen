package remote

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	dockerignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/docker"
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

// excluded implements ExcludedInAnyForm and, if every is true,
// ExcludedInEveryForm.
func (i *Ignores) excluded(path string, directory, every bool) string {
	if i == nil || path == "" {
		return ""
	}
	var ignorer ignore.Ignorer
	var err error
	switch i.Syntax {
	case ignore.Syntax_SyntaxMutagen:
		ignorer, err = mutagenignore.NewIgnorer(i.Patterns)
	case ignore.Syntax_SyntaxDocker:
		ignorer, err = dockerignore.NewIgnorer(i.Patterns)
	default:
		return ""
	}
	if err != nil {
		return ""
	} else if i.VCS {
		ignorer = ignore.IgnoreVCS(ignorer)
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
		depth := excludedDepth(ignorer, strings.Split(form, "/"), directory)
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

// excludedDepth returns the number of components of the path to the entry that
// keeps the entry with the specified path components, which is a directory if
// directory is true, from being synchronized in the form in which they're
// given (see Ignores.ExcludedInAnyForm), or zero if there's none. Ignorers
// never direct traversal of entries other than directories, so those are
// excluded whenever they're ignored or masked.
func excludedDepth(ignorer ignore.Ignorer, components []string, directory bool) int {
	masked := false
	for c := range components {
		status, traverse := ignorer.Ignore(strings.Join(components[:c+1], "/"), directory || c < len(components)-1)
		switch status {
		case ignore.IgnoreStatusIgnored:
			if !traverse {
				return c + 1
			}
			masked = true
		case ignore.IgnoreStatusUnignored:
			masked = false
		default:
			if masked && !traverse {
				return c + 1
			}
		}
	}
	return 0
}
