package synchronization

import (
	"fmt"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	dockerignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/docker"
	mutagenignore "github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore/mutagen"
)

// NewIgnorer creates the ignorer specified by a final (i.e. fully merged)
// session configuration, using the session version to resolve any default
// behavior. The configuration must already be valid. Ignores can't be validated
// by Configuration.EnsureValid because the ignore syntax may be specified by a
// different configuration that's merged on top, so this function is used both
// by endpoints and to validate ignores when session creation is requested.
func NewIgnorer(configuration *Configuration, version Version) (ignore.Ignorer, error) {
	// Compute the effective ignore syntax.
	ignoreSyntax := configuration.IgnoreSyntax
	if ignoreSyntax.IsDefault() {
		ignoreSyntax = version.DefaultIgnoreSyntax()
	}

	// Compute a combined ignore list and create the ignorer.
	var ignores []string
	ignores = append(ignores, configuration.DefaultIgnores...)
	ignores = append(ignores, configuration.Ignores...)
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

	// Compute the effective VCS ignore mode and add VCS ignores if necessary.
	ignoreVCSMode := configuration.IgnoreVCSMode
	if ignoreVCSMode.IsDefault() {
		ignoreVCSMode = version.DefaultIgnoreVCSMode()
	}
	if ignoreVCSMode == ignore.IgnoreVCSMode_IgnoreVCSModeIgnore {
		ignorer = ignore.IgnoreVCS(ignorer)
	}

	// Success.
	return ignorer, nil
}
