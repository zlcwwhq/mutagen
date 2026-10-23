package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

// readScript reads the script that --script specifies, from a local file or,
// for -, from standard input, which is then consumed.
func readScript(name string, stdin io.Reader, stderr io.Writer) (*remote.Script, error) {
	input := stdin
	if name != "-" {
		file, err := os.Open(name)
		if err != nil {
			return nil, fmt.Errorf("unable to open script: %w", err)
		}
		defer file.Close()
		input = file
	}
	contents, err := io.ReadAll(io.LimitReader(input, remote.MaximumScriptSize+1))
	if err != nil {
		return nil, fmt.Errorf("unable to read script: %w", err)
	} else if len(contents) > remote.MaximumScriptSize {
		return nil, fmt.Errorf("the script exceeds the size limit of %d KiB; save it in the synchronized directory and run it with remote bash <path> instead", remote.MaximumScriptSize/1024)
	} else if bytes.Contains(contents, []byte("\r\n")) {
		fmt.Fprintln(stderr, "remote: warning: the script has Windows line endings, which its interpreter may not accept")
	}
	return &remote.Script{Name: name, Contents: contents}, nil
}
