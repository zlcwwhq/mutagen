package remote

import (
	"context"
	"io"
)

// Flush is FlushWithProgress without progress.
func Flush(ctx context.Context, connection io.ReadWriteCloser) (*Report, error) {
	return FlushWithProgress(ctx, connection, nil)
}

// ListConflicts is ListConflictsWithProgress without progress.
func ListConflicts(ctx context.Context, connection io.ReadWriteCloser) (*Listing, error) {
	return ListConflictsWithProgress(ctx, connection, nil)
}

// WaitJob is WaitJobWithOptions without a log.
func WaitJob(ctx context.Context, connection io.ReadWriteCloser, id string) (*Job, error) {
	job, _, err := WaitJobWithTail(ctx, connection, id, 0)
	return job, err
}

// WaitJobWithTail is WaitJobWithOptions with a log tail of the specified number
// of lines (see WaitOptions.Tail).
func WaitJobWithTail(ctx context.Context, connection io.ReadWriteCloser, id string, lines int64) (*Job, *JobLog, error) {
	return WaitJobWithOptions(ctx, connection, id, &WaitOptions{Tail: lines})
}
