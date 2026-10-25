package remote

import "errors"

// DirectoryErrorKind identifies why the executor could not use a working
// directory. It travels with the original diagnostic in the result protocol.
type DirectoryErrorKind string

const (
	// DirectoryErrorInvalid indicates that a directory could not be expanded.
	DirectoryErrorInvalid DirectoryErrorKind = "invalid"
	// DirectoryErrorMissing indicates that the directory does not exist.
	DirectoryErrorMissing DirectoryErrorKind = "missing"
	// DirectoryErrorInaccessible indicates another filesystem access failure.
	DirectoryErrorInaccessible DirectoryErrorKind = "inaccessible"
	// DirectoryErrorNotDirectory indicates that the path is not a directory.
	DirectoryErrorNotDirectory DirectoryErrorKind = "not-directory"
)

// DirectoryError preserves an executor's working directory failure so clients
// can offer relevant guidance without matching its diagnostic text.
type DirectoryError struct {
	// Kind classifies the failure.
	Kind DirectoryErrorKind
	// Path is the failed execution path, or an explicit path that could not be
	// expanded on the executor.
	Path string
	// Message is the original executor diagnostic, including its cause.
	Message string
}

// Error implements error.Error.
func (e *DirectoryError) Error() string {
	return e.Message
}

// executionError reconstructs an execution infrastructure error from a result.
func (r *result) executionError() error {
	if r.DirectoryError != "" {
		return &DirectoryError{Kind: r.DirectoryError, Path: r.Directory, Message: r.Error}
	}
	return errors.New(r.Error)
}
