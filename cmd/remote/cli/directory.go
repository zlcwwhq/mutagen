package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// printCommandError prints an execution failure and, when an explicit absolute
// directory beneath the submitting user's home does not exist on the executor,
// suggests quoting a tilde if the remote home was intended. Shell expansion
// cannot be detected, so the suggestion remains conditional.
func printCommandError(output io.Writer, err error, directory string) {
	fmt.Fprintln(output, "remote:", err)
	var failure *remote.DirectoryError
	if !errors.As(err, &failure) || failure.Kind != remote.DirectoryErrorMissing || !filepath.IsAbs(directory) {
		return
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil || !filepath.IsAbs(home) {
		return
	}
	relative, inside := within(home, directory)
	if !inside {
		return
	}
	quoted := "~"
	if relative != "" {
		quoted += "/" + relative
	}
	// Single quotes also protect spaces and shell metacharacters in the suffix.
	quoted = "'" + strings.ReplaceAll(quoted, "'", "'\"'\"'") + "'"
	fmt.Fprintf(output, "If you meant the remote home directory, quote ~: -C %s.\n", terminal.NeutralizeControlCharacters(quoted))
}
