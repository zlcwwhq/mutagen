package cli

import (
	"context"
	"fmt"
	"io"
	"path"
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
	containsDirectory := false
	for _, session := range sessions {
		containsDirectory = containsDirectory || session.Contained
	}
	for _, session := range sessions {
		endpoint := "beta"
		if session.Alpha {
			endpoint = "alpha"
		}
		host := session.ExecutionHost
		if host == "" {
			host = "unknown"
		}
		line := fmt.Sprintf("  %s: local root %s (%s); runs on %s:%s; cwd %s",
			terminal.NeutralizeControlCharacters(session.Label), terminal.NeutralizeControlCharacters(session.Root), endpoint,
			terminal.NeutralizeControlCharacters(host), terminal.NeutralizeControlCharacters(executionDirectory(session.ExecutionRoot, "")),
			terminal.NeutralizeControlCharacters(executionDirectory(session.ExecutionRoot, session.Path)))
		var notes []string
		if session.Selected {
			notes = append(notes, "used by default")
		}
		if session.Excluded {
			notes = append(notes, "ignores the current directory; cannot run directly")
		}
		if !session.Contained {
			if session.Selected {
				notes = append(notes, "outside local root; uses execution root")
			} else if containsDirectory {
				notes = append(notes, "outside local root; select with -s and --cwd")
			} else {
				notes = append(notes, "outside local root; -s selects execution root")
			}
		}
		if session.Version != mutagen.Version {
			notes = append(notes, fmt.Sprintf("registered by Mutagen %s, so this remote can't use it", terminal.NeutralizeControlCharacters(session.Version)))
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

// executionDirectory displays a configured execution root, or a directory
// relative to it, without resolving the root on the submitting system. Agent
// processes start in the user's home, so relative roots are shown with ~/.
// Windows absolute roots retain their separators even on POSIX submitters.
func executionDirectory(root, relative string) string {
	if root == "" {
		return "unknown"
	}
	unc := strings.HasPrefix(root, `\\`) || strings.HasPrefix(root, "//")
	windows := len(root) >= 3 && ((root[0] >= 'A' && root[0] <= 'Z') || (root[0] >= 'a' && root[0] <= 'z')) &&
		root[1] == ':' && (root[2] == '/' || root[2] == '\\') || unc
	if windows {
		// Keep the drive or UNC host/share separate from path cleaning, so ..
		// cannot remove the volume or escape above its root. This also preserves
		// the leading double separator of UNC paths on POSIX submitters.
		backslash := strings.Contains(root, `\`)
		root = strings.ReplaceAll(root, `\`, "/")
		volumeLength := 2
		if unc {
			volumeLength = len(root)
			separators := 0
			for i := 2; i < len(root); i++ {
				if root[i] == '/' {
					separators++
					if separators == 2 {
						volumeLength = i
						break
					}
				}
			}
		}
		volume, remainder := root[:volumeLength], root[volumeLength:]
		joined := volume
		if remainder != "" || relative != "" {
			joined += path.Join("/", remainder, relative)
		}
		if backslash {
			joined = strings.ReplaceAll(joined, "/", `\`)
		}
		return joined
	}
	if strings.HasPrefix(root, "/") {
		return path.Join(root, relative)
	}
	// Keep the home marker separate from path cleaning: ~/../other refers
	// to a sibling of home, and must not collapse to the relative path other.
	if strings.HasPrefix(root, `~\`) {
		root = strings.ReplaceAll(root, `\`, "/")
	}
	home := "~"
	if strings.HasPrefix(root, "~") {
		home, root, _ = strings.Cut(root, "/")
	}
	joined := path.Join(root, relative)
	if joined == "." || joined == "" {
		return home
	}
	return home + "/" + joined
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
