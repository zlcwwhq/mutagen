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

// Request describes a single command, or a request for a file. Arguments are
// passed directly to the executable, without shell interpretation. Environment
// entries override the executing process's inherited environment, with later
// entries taking precedence.
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
	// Prologue indicates whether the executing endpoint's prologue, if any,
	// should be sourced before the command. Submitters request it when the
	// submission directory lies inside the submitting root (and thus Path
	// identifies the corresponding directory), regardless of Directory.
	Prologue bool
	// File is the slash-separated path, relative to the target root, of an
	// entry to read (see ReadEntry) instead of running a command. Requests for
	// files specify no arguments.
	File string
}

// validRelativePath determines whether a slash-separated path lies inside the
// root that it's relative to.
func validRelativePath(value string) bool {
	return !path.IsAbs(value) && path.Clean(value) == value && value != ".." &&
		!strings.HasPrefix(value, "../") && !strings.Contains(value, "\\")
}

// EnsureValid checks that command metadata can be represented losslessly by
// the protocol and passed to the operating system.
func (r *Request) EnsureValid() error {
	if r != nil && r.File != "" {
		if len(r.Arguments) > 0 {
			return errors.New("file requests must not specify a command")
		} else if !utf8.ValidString(r.File) || strings.IndexByte(r.File, 0) >= 0 || r.File == "." || !validRelativePath(r.File) {
			return errors.New("requested file path must be relative to the synchronization root")
		}
		return nil
	} else if r == nil || len(r.Arguments) == 0 || r.Arguments[0] == "" {
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
	if r.Path != "" && !validRelativePath(r.Path) {
		return errors.New("working directory path must be relative to the synchronization root")
	}
	return nil
}

// Operation identifies what a submission requests from the daemon.
type Operation uint8

const (
	// OperationExecute requests a route to the opposite endpoint's executor.
	OperationExecute Operation = iota
	// OperationFlush requests a synchronization cycle, whose completion the
	// daemon reports with a status message.
	OperationFlush
)

// submission is the header of each submission, written by the submitting
// client and consumed by the daemon before it handles the submission.
type submission struct {
	// Operation is the requested operation.
	Operation Operation
}

// status reports whether a command route reached an executing endpoint. It is
// written by the executing endpoint on success or by any intermediate hop that
// is unable to establish the route.
type status struct {
	// Error is the routing error, if any.
	Error string
}

// flushResponse reports the outcome of a flush. Hops that are unable to reach
// the daemon send a status instead, which decodes compatibly.
type flushResponse struct {
	// Error is the flush or routing error, if any.
	Error string
	// Report describes content that the session couldn't synchronize. It's
	// non-nil if and only if the flush succeeded.
	Report *Report `json:",omitempty"`
}

// result distinguishes command exit status from execution infrastructure errors.
type result struct {
	// ExitCode is the command exit code.
	ExitCode int
	// Error is the execution infrastructure error, if any.
	Error string
	// Entry is the requested entry for file requests, excluding its data,
	// which is sent as output.
	Entry *Entry `json:",omitempty"`
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
