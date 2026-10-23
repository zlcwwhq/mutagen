package synchronization

import (
	"fmt"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	dockerignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/docker"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

// effectiveIgnores computes the ignore syntax, the combined ignore list, and
// whether VCS directories are ignored, as specified by a final (i.e. fully
// merged) session configuration, using the session version to resolve any
// default behavior.
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
	return ignoreSyntax, ignores, ignoreVCSMode == ignore.IgnoreVCSMode_IgnoreVCSModeIgnore
}

// EffectiveIgnores returns the effective ignores specified by a final (i.e.
// fully merged) session configuration, as effectiveIgnores computes them, in
// the form that remote uses to determine the paths that the session
// synchronizes. Ignores can't be endpoint-specific, so either endpoint's
// merged configuration yields the same ones.
func EffectiveIgnores(configuration *Configuration, version Version) *remote.Ignores {
	syntax, patterns, vcs := effectiveIgnores(configuration, version)
	return &remote.Ignores{Syntax: syntax, Patterns: patterns, VCS: vcs}
}

// NewIgnorer creates the ignorer specified by a final (i.e. fully merged)
// session configuration, using the session version to resolve any default
// behavior. The configuration must already be valid. Ignores can't be validated
// by Configuration.EnsureValid because the ignore syntax may be specified by a
// different configuration that's merged on top, so this function is used both
// by endpoints and to validate ignores when session creation is requested.
func NewIgnorer(configuration *Configuration, version Version) (ignore.Ignorer, error) {
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
