package synchronization

import (
	"errors"
	"fmt"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	dockerignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/docker"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

// GitModeEnabled indicates whether or not a final (i.e. fully merged) session
// configuration enables Git mode, using the session version to resolve any
// default behavior. Git mode can't be endpoint-specific, so either endpoint's
// merged configuration yields the same result.
func GitModeEnabled(configuration *Configuration, version Version) bool {
	gitMode := configuration.GitMode
	if gitMode.IsDefault() {
		gitMode = version.DefaultGitMode()
	}
	return gitMode == GitMode_GitModeEnabled
}

// effectiveIgnores computes the ignore syntax, the combined ignore list, and
// whether VCS directories are ignored, as specified by a final (i.e. fully
// merged) session configuration, using the session version to resolve any
// default behavior. Git mode never synchronizes Git's own entries, so it
// implies VCS ignoring.
func effectiveIgnores(configuration *Configuration, version Version) (ignore.Syntax, []string, bool) {
	// Compute the effective ignore syntax.
	ignoreSyntax := configuration.IgnoreSyntax
	if ignoreSyntax.IsDefault() {
		ignoreSyntax = version.DefaultIgnoreSyntax()
	}

	// Compute a combined ignore list.
	var ignores []string
	ignores = append(ignores, configuration.DefaultIgnores...)
	ignores = append(ignores, configuration.Ignores...)

	// Compute the effective VCS ignore mode.
	ignoreVCSMode := configuration.IgnoreVCSMode
	if ignoreVCSMode.IsDefault() {
		ignoreVCSMode = version.DefaultIgnoreVCSMode()
	}
	ignoreVCS := ignoreVCSMode == ignore.IgnoreVCSMode_IgnoreVCSModeIgnore

	// Git mode implies VCS ignoring.
	return ignoreSyntax, ignores, ignoreVCS || GitModeEnabled(configuration, version)
}

// EffectiveIgnores returns the effective ignores specified by a final (i.e.
// fully merged) session configuration, as effectiveIgnores computes them, in
// the form that remote uses to determine the paths that the session
// synchronizes. Ignores can't be endpoint-specific, so either endpoint's
// merged configuration yields the same ones. In Git mode, the ignores only
// carry the mode: the rules are files in the synchronized tree, which remote
// reads from its system's copy of it.
func EffectiveIgnores(configuration *Configuration, version Version) *remote.Ignores {
	syntax, patterns, vcs := effectiveIgnores(configuration, version)
	return &remote.Ignores{
		Syntax:   syntax,
		Patterns: patterns,
		VCS:      vcs,
		Git:      GitModeEnabled(configuration, version),
	}
}

// NewIgnorer creates the ignorer specified by a final (i.e. fully merged)
// session configuration, using the session version to resolve any default
// behavior. The configuration must already be valid. Ignores can't be validated
// by Configuration.EnsureValid because the ignore syntax may be specified by a
// different configuration that's merged on top, so this function is used both
// by endpoints and to validate ignores when session creation is requested. It
// also rejects Git mode combined with an explicit request to propagate VCS
// directories, which Git mode contradicts by never synchronizing Git's own
// entries. This can't be checked by Configuration.EnsureValid for the same
// reason as ignores: either mode may be specified by a different configuration
// that's merged on top. A VCS ignore mode that merely defaults to propagating
// them isn't a conflict, because nothing was requested.
func NewIgnorer(configuration *Configuration, version Version) (ignore.Ignorer, error) {
	// Verify that Git mode doesn't contradict an explicit VCS ignore mode.
	if GitModeEnabled(configuration, version) &&
		configuration.IgnoreVCSMode == ignore.IgnoreVCSMode_IgnoreVCSModePropagate {
		return nil, errors.New("Git mode never synchronizes VCS directories and can't be combined with propagating them")
	}

	// Compute the effective ignores and create the ignorer.
	ignoreSyntax, ignores, ignoreVCS := effectiveIgnores(configuration, version)
	var ignorer ignore.Ignorer
	if ignoreSyntax == ignore.Syntax_SyntaxMutagen {
		if i, err := mutagenignore.NewIgnorer(ignores); err != nil {
			return nil, fmt.Errorf("unable to create Mutagen-style ignorer: %w", err)
		} else {
			ignorer = i
		}
	} else if ignoreSyntax == ignore.Syntax_SyntaxDocker {
		if i, err := dockerignore.NewIgnorer(ignores); err != nil {
			return nil, fmt.Errorf("unable to create Docker-style ignorer: %w", err)
		} else {
			ignorer = i
		}
	} else {
		panic("unhandled ignore syntax")
	}

	// Add VCS ignores if necessary.
	if ignoreVCS {
		ignorer = ignore.IgnoreVCS(ignorer)
	}

	// Success.
	return ignorer, nil
}
