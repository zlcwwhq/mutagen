package remote

import (
	"strings"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	dockerignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/docker"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

// Ignores describes a session's effective ignores, which are the same for both
// of its endpoints, so that submissions can determine whether the session
// synchronizes the directory corresponding to theirs (see
// Selection.Corresponding).
type Ignores struct {
	// Syntax is the ignore syntax, which is never the default.
	Syntax ignore.Syntax
	// Patterns are the ignore patterns, including any default ignores.
	Patterns []string
	// VCS indicates whether VCS directories are ignored.
	VCS bool
}

// excluded returns the slash-separated path, relative to the synchronization
// root, of the directory that keeps the directory at the specified path from
// being synchronized, which is either that directory or one of its parents, or
// an empty string if there's none. Directories are excluded as scans exclude
// them, by not traversing them: an ignored directory is traversed only if
// content beneath it may be unignored, in which case some of that content may
// be synchronized, and it isn't considered excluded, while content beneath it
// that isn't unignored is excluded once nothing beneath it can be. Ignores that
// can't be compiled exclude nothing, since sessions validate their ignores.
func (i *Ignores) excluded(path string) string {
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
	masked := false
	for c := range components {
		prefix := strings.Join(components[:c+1], "/")
		status, traverse := ignorer.Ignore(prefix, true)
		switch status {
		case ignore.IgnoreStatusIgnored:
			if !traverse {
				return prefix
			}
			masked = true
		case ignore.IgnoreStatusUnignored:
			masked = false
		default:
			if masked && !traverse {
				return prefix
			}
		}
	}
	return ""
}
