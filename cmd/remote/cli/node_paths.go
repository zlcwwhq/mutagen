package cli

import (
	"path"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/url"
)

// executionDirectory displays a configured execution root, or a directory
// relative to it, without resolving the root on the submitting system. Agent
// processes start in the user's home, so relative roots are shown with ~/.
// Windows absolute roots retain their separators even on POSIX submitters.
func executionDirectory(root, relative string) string {
	if root == "" {
		return "unknown"
	}
	if volumeLength := url.WindowsVolumeLength(root); volumeLength != 0 {
		// Keep the drive or UNC host/share separate from path cleaning, so ..
		// cannot remove the volume or escape above its root. This also preserves
		// the leading double separator of UNC paths on POSIX submitters.
		backslash := strings.Contains(root, `\`)
		root = strings.ReplaceAll(root, `\`, "/")
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
