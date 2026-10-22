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

// Excluded returns the slash-separated path, relative to the synchronization
// root, of the entry that keeps the entry at the specified path, which is a
// directory if directory is true and another kind of entry otherwise, from
// being synchronized. That's either the entry itself or one of its parents, or
// an empty string if there's none. Entries are excluded as scans exclude them,
// with directories excluded by not traversing them: an ignored directory is
// traversed only if content beneath it may be unignored, in which case some of
// that content may be synchronized, and it isn't considered excluded, while
// content beneath it that isn't unignored is excluded once nothing beneath it
// can be. Ignores that can't be compiled exclude nothing, since sessions
// validate their ignores.
//
// The path is excluded if it's excluded in any Unicode normalization form,
// since scans needn't see it in the form given: they recompose names on
// filesystems that decompose them, and filesystems that look up names
// regardless of their form let the current directory be named in a form other
// than the stored one (as in $PWD, which os.Getwd returns).
func (i *Ignores) Excluded(path string, directory bool) string {
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
	components := strings.Split(path, "/")
	for _, form := range []string{path, norm.NFC.String(path), norm.NFD.String(path)} {
		if depth := excludedDepth(ignorer, strings.Split(form, "/"), directory); depth > 0 {
			return strings.Join(components[:depth], "/")
		}
	}
	return ""
}

// excludedDepth returns the number of components of the path to the entry that
// keeps the entry with the specified path components, which is a directory if
// directory is true, from being synchronized (see Ignores.Excluded), or zero if
// there's none. Ignorers never direct traversal of entries other than
// directories, so those are excluded whenever they're ignored or masked.
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
