package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

// CommandEvent carries synchronization metadata separately from command output.
// Each edge has one phase before execution and, for foreground commands, one
// after it. Progress samples precede a Done event containing the cycle report.
type CommandEvent struct {
	Alpha    string
	Beta     string
	Phase    string
	Progress *Progress `json:",omitempty"`
	Report   *Report   `json:",omitempty"`
	Done     bool      `json:",omitempty"`
	Error    string    `json:",omitempty"`
}

// CommandResult is the complete executor result retained by daemon forwarding.
// Keeping it intact preserves job metadata and structured execution failures.
type CommandResult = result

// ExecutionError reconstructs an executor error, or returns nil on success.
func (r *result) ExecutionError() error {
	if r.Error == "" {
		return nil
	}
	return r.executionError()
}

// ForwardCommand performs an execution exchange without discarding result
// metadata. It owns the connection; executor failures remain in its result.
func ForwardCommand(ctx context.Context, connection io.ReadWriteCloser, request *Request, stdin io.Reader, stdout, stderr io.Writer) (*CommandResult, error) {
	return execute(ctx, connection, request, stdin, stdout, stderr)
}

// boundCommandEvent bounds reports as flush responses are bounded, including
// the enclosing command event's metadata in the message size calculation.
func boundCommandEvent(event *CommandEvent) (*CommandEvent, error) {
	bounded := *event
	if event.Report == nil {
		return &bounded, nil
	}
	report, err := boundReport(event.Report)
	if err != nil {
		return nil, err
	}
	report.Conflicts = slices.Clone(report.Conflicts)
	report.Problems = slices.Clone(report.Problems)
	bounded.Report = report
	for {
		data, err := json.Marshal(&result{Event: &bounded})
		if err != nil {
			return nil, err
		}
		if len(data) <= maximumMessageSize {
			return &bounded, nil
		}
		if len(report.Problems) > 0 {
			report.Problems = report.Problems[:len(report.Problems)-1]
			report.ExcludedProblems++
		} else if len(report.Conflicts) > 0 {
			report.Conflicts = report.Conflicts[:len(report.Conflicts)-1]
			report.ExcludedConflicts++
		} else {
			return nil, errors.New("command synchronization event exceeds size limit")
		}
	}
}
