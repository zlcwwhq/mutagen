package remote

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// maximumMessageSize bounds command metadata independently of streamed I/O.
const maximumMessageSize = 1024 * 1024

// Request describes a single command. Arguments are passed directly to the
// executable, without shell interpretation. Environment entries override the
// executing process's inherited environment, with later entries taking
// precedence.
type Request struct {
	// Arguments contains the executable name followed by its arguments.
	Arguments []string
	// Environment contains explicit overrides of the remote environment.
	Environment []string
	// Path is the slash-separated working directory relative to the target
	// synchronization root. An empty path refers to the root itself.
	Path string
	// Directory optionally overrides the working directory. Absolute and
	// home-relative values are interpreted on the target system, while other
	// values are resolved against the directory specified by Path.
	Directory string
}

// EnsureValid checks that command metadata can be represented losslessly by
// the protocol and passed to the operating system.
func (r *Request) EnsureValid() error {
	if r == nil || len(r.Arguments) == 0 || r.Arguments[0] == "" {
		return errors.New("no command specified")
	}
	for _, value := range append(append([]string{r.Path, r.Directory}, r.Arguments...), r.Environment...) {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return errors.New("command metadata contains invalid UTF-8 or a null byte")
		}
	}
	for _, entry := range r.Environment {
		if key, _, ok := strings.Cut(entry, "="); !ok || key == "" {
			return errors.New("environment entries must have the form NAME=VALUE")
		}
	}
	// The relative path is computed by the submitting endpoint and must stay
	// inside the target root. Explicit directory overrides are not restricted.
	if r.Path != "" && (path.IsAbs(r.Path) || path.Clean(r.Path) != r.Path ||
		r.Path == ".." || strings.HasPrefix(r.Path, "../") || strings.Contains(r.Path, "\\")) {
		return errors.New("working directory path must be relative to the synchronization root")
	}
	return nil
}

// status reports whether a command route reached an executing endpoint. It is
// written by the executing endpoint on success or by any intermediate hop that
// is unable to establish the route.
type status struct {
	// Error is the routing error, if any.
	Error string
}

// result distinguishes command exit status from execution infrastructure errors.
type result struct {
	// ExitCode is the command exit code.
	ExitCode int
	// Error is the execution infrastructure error, if any.
	Error string
}

// writeMessage writes bounded, length-prefixed command metadata.
func writeMessage(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	} else if len(data) > maximumMessageSize {
		return errors.New("command metadata exceeds size limit")
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

// readMessage reads bounded, length-prefixed command metadata.
func readMessage(reader io.Reader, value any) error {
	var size uint32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return err
	} else if size > maximumMessageSize {
		return errors.New("command metadata exceeds size limit")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return err
	} else if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("invalid command metadata: %w", err)
	}
	return nil
}
