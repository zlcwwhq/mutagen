package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaximumJobLogSize bounds the contents read and returned by a job log tail.
// One additional byte is read to recognize a line boundary at this limit.
const MaximumJobLogSize = 1024 * 1024

// JobLog describes the requested tail of a detached job's log.
type JobLog struct {
	// Data contains the final requested lines, limited to MaximumJobLogSize
	// bytes. It's transmitted separately from the metadata.
	Data []byte `json:"-"`
	// Truncated indicates that the requested lines exceed MaximumJobLogSize.
	// In that case, Data may start within a line.
	Truncated bool `json:",omitempty"`
	// Error reports why the log couldn't be read. It doesn't affect the job's
	// exit status, and Data is empty if it's non-empty.
	Error string `json:",omitempty"`
}

// readJobLog reads a bounded snapshot of the final lines of a job's log. It
// follows symbolic links, as starting the job does, but only reads regular
// files. Appends after the size is determined aren't included.
func readJobLog(ctx context.Context, path string, lines int64) *JobLog {
	if lines == 0 {
		return &JobLog{}
	} else if err := ctx.Err(); err != nil {
		return &JobLog{Error: err.Error()}
	}
	file, err := openJobLog(path)
	if err != nil {
		return &JobLog{Error: err.Error()}
	}
	defer file.Close()
	stop := context.AfterFunc(ctx, func() { file.Close() })
	defer stop()
	info, err := file.Stat()
	if err != nil {
		return &JobLog{Error: err.Error()}
	}
	size := info.Size()
	offset := max(int64(0), size-MaximumJobLogSize-1)
	data := make([]byte, int(size-offset))
	if _, err := file.ReadAt(data, offset); err != nil {
		return &JobLog{Error: fmt.Sprintf("unable to read log snapshot: %v", err)}
	} else if err := ctx.Err(); err != nil {
		return &JobLog{Error: err.Error()}
	}

	// An ending newline terminates the last line rather than starting an
	// empty one. An unterminated final line counts as a line as well. Count
	// down, so even the largest requested count can't overflow.
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	truncated := offset > 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			lines--
			if lines == 0 {
				data, truncated = data[i+1:], false
				break
			}
		}
	}
	if len(data) > MaximumJobLogSize {
		data, truncated = data[len(data)-MaximumJobLogSize:], true
	}
	return &JobLog{Data: data, Truncated: truncated}
}

// jobLogBuffer bounds received log data independently of the executor. Excess
// output is discarded so that the job's exit status can still be received.
type jobLogBuffer struct {
	data     []byte
	overflow bool
}

func (b *jobLogBuffer) Write(data []byte) (int, error) {
	count := min(len(data), MaximumJobLogSize-len(b.data))
	b.data = append(b.data, data[:count]...)
	b.overflow = b.overflow || count < len(data)
	return len(data), nil
}

// WaitJobWithTail waits as WaitJob does and also reads the final lines of the
// job's log. Zero lines disables log reading and returns a nil log. Errors
// reading the log are reported in JobLog.Error without affecting the job's
// status. The entire tail is buffered until the request succeeds, so callers
// can retry interrupted requests without duplicating partial output.
func WaitJobWithTail(ctx context.Context, connection io.ReadWriteCloser, id string, lines int64) (*Job, *JobLog, error) {
	var output jobLogBuffer
	response, err := execute(ctx, connection, &Request{Job: &JobRequest{Operation: JobWait, ID: id, Tail: lines}}, strings.NewReader(""), &output, io.Discard)
	if err != nil {
		return nil, nil, err
	} else if response.Error != "" {
		return nil, nil, &JobError{response.Error}
	} else if response.Job == nil {
		return nil, nil, errors.New("no job received")
	} else if lines == 0 {
		return response.Job, nil, nil
	}
	log := response.JobLog
	if log == nil {
		log = &JobLog{Error: "the executing endpoint doesn't support log tails"}
	} else if output.overflow {
		log = &JobLog{Error: "the executing endpoint returned a log tail larger than 1 MiB"}
	} else if log.Error == "" {
		log.Data = output.data
	}
	return response.Job, log, nil
}
