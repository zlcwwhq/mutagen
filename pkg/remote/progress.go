package remote

import (
	"io"
	"time"
)

// Progress describes what a session is doing while a submission waits for its
// synchronization cycle (see Submission.Progress). The session may first
// finish a cycle that started before the submission's, so a submission can
// see phases repeat.
type Progress struct {
	// Status is the session's synchronization status, in the textual form of
	// the synchronization package's Status (such as scanning or
	// staging-alpha). Daemons may report statuses that submitters don't know.
	Status string
	// Staging describes the files being staged, if Status is staging-alpha or
	// staging-beta and the staging endpoint has started receiving them.
	Staging *StagingProgress `json:",omitempty"`
}

// StagingProgress describes the staging of files on an endpoint, which receives
// their contents from the opposite endpoint before applying changes. The total
// size of the files isn't known in advance.
type StagingProgress struct {
	// ReceivedFiles is the number of files received completely, as of the
	// last update from the staging endpoint.
	ReceivedFiles uint64
	// ExpectedFiles is the number of files to receive.
	ExpectedFiles uint64
	// ReceivedSize is the total size of the file contents received so far,
	// including parts reused from existing files rather than transferred.
	ReceivedSize uint64
	// Path is the slash-separated path, relative to the synchronization root,
	// of the file being received, if any.
	Path string `json:",omitempty"`
	// FileReceivedSize is the size of the file being received that has been
	// received so far.
	FileReceivedSize uint64 `json:",omitempty"`
	// FileExpectedSize is the size of the file being received.
	FileExpectedSize uint64 `json:",omitempty"`
}

// equalProgress determines whether two progress samples are equal.
func equalProgress(first, second *Progress) bool {
	if first == nil || second == nil {
		return first == second
	} else if first.Status != second.Status || (first.Staging == nil) != (second.Staging == nil) {
		return false
	}
	return first.Staging == nil || *first.Staging == *second.Staging
}

// ReportProgress sends progress messages over the connection of a flush or
// conflicts submission that requested them (see Submission.Progress) until
// stop is closed, with samples of the session's progress obtained from sample,
// which returns nil if there's nothing to report. It sends the first sample at
// once, even if stop is already closed, so that submitters see the phase in
// which their wait began. It then samples at the specified interval and sends
// samples that differ from the last one sent, and others once the heartbeat
// interval has elapsed since the last message, which shows submitters that the
// daemon is still waiting. It returns once stop is closed, so that the caller
// can send the final response, or at the first failure to send, such as when
// the submitter abandons the wait, which doesn't affect synchronization.
func ReportProgress(connection io.Writer, sample func() *Progress, interval, heartbeat time.Duration, stop <-chan struct{}) {
	MonitorProgress(func(progress *Progress) error {
		return writeMessage(connection, &progressMessage{Progress: progress})
	}, sample, interval, heartbeat, stop)
}

// MonitorProgress sends sampled progress through a caller's structured
// transport, using the same sampling and heartbeat policy as flush reporting.
// It stops on the first transport error, allowing callers to cancel their wait.
func MonitorProgress(emit func(*Progress) error, sample func() *Progress, interval, heartbeat time.Duration, stop <-chan struct{}) error {
	// Send the first sample.
	var last *Progress
	var sent time.Time
	send := func(progress *Progress) error {
		if progress == nil {
			return nil
		} else if err := emit(progress); err != nil {
			return err
		}
		last, sent = progress, time.Now()
		return nil
	}
	if err := send(sample()); err != nil {
		return err
	}

	// Send later samples that differ from the last one, or are due as
	// heartbeats.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
		}
		progress := sample()
		if equalProgress(progress, last) && time.Since(sent) < heartbeat {
			continue
		} else if err := send(progress); err != nil {
			return err
		}
	}
}
