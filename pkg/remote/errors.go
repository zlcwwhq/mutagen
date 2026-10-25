package remote

import (
	"context"
	"errors"
	"io"
)

// ErrorKind distinguishes failures of an operation from failures to reach or
// communicate with an endpoint. It is transmitted alongside diagnostics so
// callers never have to classify errors by matching their text.
type ErrorKind string

const (
	// ErrorKindOperation denotes invalid input, refused operations, and I/O
	// failures on either endpoint.
	ErrorKindOperation ErrorKind = "operation"
	// ErrorKindConnection denotes unavailable sessions, failed connections,
	// and invalid or incomplete protocol exchanges.
	ErrorKindConnection ErrorKind = "connection"
	// ErrorKindTimeout denotes an operation whose deadline expired.
	ErrorKindTimeout ErrorKind = "timeout"
)

// classifiedError adds a classification without losing the underlying cause.
type classifiedError struct {
	kind ErrorKind
	err  error
}

func (e *classifiedError) Error() string { return e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

// ConnectionError marks an error at a connection or protocol boundary. Errors
// originating in local validation or filesystem operations need no wrapper:
// their default classification is ErrorKindOperation.
func ConnectionError(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{ErrorKindConnection, err}
}

// ClassifyError classifies an error, including joined batch errors. Deadlines
// take precedence over connection failures, which take precedence over
// operation failures, independently of the order in which errors occurred.
func ClassifyError(err error) ErrorKind {
	if err == nil {
		return ""
	} else if classified, ok := err.(*classifiedError); ok {
		// A connection's internal deadline is still a connection failure.
		// The caller's own deadline remains a separate cause (or context).
		return classified.kind
	} else if err == context.DeadlineExceeded {
		return ErrorKindTimeout
	}
	kind := ErrorKindOperation
	var causes []error
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes = joined.Unwrap()
	} else if cause := errors.Unwrap(err); cause != nil {
		causes = []error{cause}
	}
	for _, cause := range causes {
		if next := ClassifyError(cause); next == ErrorKindTimeout {
			return next
		} else if next == ErrorKindConnection && kind != ErrorKindTimeout {
			kind = next
		}
	}
	return kind
}

// receivedError reconstructs a classified protocol error, retaining any
// operation-specific type in cause. Unknown kinds are protocol failures;
// missing kinds from older messages retain operation-error semantics.
func receivedError(kind ErrorKind, cause error) error {
	switch kind {
	case "", ErrorKindOperation:
		return cause
	case ErrorKindConnection, ErrorKindTimeout:
		return &classifiedError{kind, cause}
	default:
		return ConnectionError(cause)
	}
}

// failedResult serializes an operation failure without losing its category.
func failedResult(err error) result {
	return result{Error: err.Error(), ErrorKind: ClassifyError(err)}
}

// connectionReader and connectionWriter identify transport failures even when
// they occur inside filesystem operations that also perform local I/O.
type connectionReader struct{ io.Reader }

func (r connectionReader) Read(data []byte) (int, error) {
	count, err := r.Reader.Read(data)
	if err != io.EOF {
		err = ConnectionError(err)
	}
	return count, err
}

type connectionWriter struct{ io.Writer }

func (w connectionWriter) Write(data []byte) (int, error) {
	count, err := w.Writer.Write(data)
	if err == nil && count != len(data) {
		err = io.ErrShortWrite
	}
	return count, ConnectionError(err)
}
