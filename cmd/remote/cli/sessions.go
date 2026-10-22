package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/platform/terminal"
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// printSessions lists the session endpoints on this system through which
// operations can be submitted, separating those whose roots contain the
// working directory, and marks the one that's used by default, given the
// default session (see remote.Selection.Default).
func printSessions(ctx context.Context, directory, defaultSession string, output io.Writer) error {
	sessions, err := (&remote.Selection{Default: defaultSession}).Sessions(ctx, directory)
	if err != nil {
		return err
	} else if len(sessions) == 0 {
		_, err := fmt.Fprintln(output, "No connected session has remote commands enabled on this system.")
		return err
	}
	var containing, others []string
	for _, session := range sessions {
		endpoint := "beta"
		if session.Alpha {
			endpoint = "alpha"
		}
		line := fmt.Sprintf("  %s: %s root %s", session.Label, endpoint, terminal.NeutralizeControlCharacters(session.Root))
		var notes []string
		if session.Selected {
			notes = append(notes, "used by default")
		}
		if session.Excluded {
			notes = append(notes, "ignores the current directory")
		}
		if session.Version != mutagen.Version {
			notes = append(notes, fmt.Sprintf("registered by Mutagen %s, so this remote can't use it", session.Version))
		}
		if len(notes) > 0 {
			line += " (" + strings.Join(notes, "; ") + ")"
		}
		if session.Contained {
			containing = append(containing, line)
		} else {
			others = append(others, line)
		}
	}
	for _, section := range []struct {
		heading string
		lines   []string
	}{
		{"Sessions whose roots contain the current directory:", containing},
		{"Sessions whose roots don't contain the current directory:", others},
	} {
		if len(section.lines) == 0 {
			continue
		}
		fmt.Fprintln(output, section.heading)
		for _, line := range section.lines {
			if _, err := fmt.Fprintln(output, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// warnOverlapping warns that several sessions synchronize the working
// directory if the target was located without selecting a session, since the
// operation may have been meant for another one (see
// remote.Target.Overlapping).
func warnOverlapping(stderr io.Writer, target *remote.Target) {
	sessions := target.Overlapping
	if len(sessions) < 2 {
		return
	}
	list := sessions[0] + " and " + sessions[1] + " both"
	if len(sessions) > 2 {
		list = strings.Join(sessions[:len(sessions)-1], ", ") + ", and " + sessions[len(sessions)-1] + " all"
	}
	fmt.Fprintf(stderr, "remote: warning: sessions %s synchronize the current directory; using %s (select one with -s)\n", list, sessions[0])
}
