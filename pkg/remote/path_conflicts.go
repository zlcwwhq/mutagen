package remote

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
)

// EdgeConflicts labels a complete conflict listing by the actual edge. Its
// session identity is used only for checkout ownership at the submitting node.
type EdgeConflicts struct {
	Alpha       string   `json:"alpha"`
	Beta        string   `json:"beta"`
	Session     string   `json:"-"`
	SourceAlpha bool     `json:"-"`
	Listing     *Listing `json:"listing"`
}

type conflictEdge struct {
	Alpha, Beta, Session string
	SourceAlpha          bool
}
type pathConflictsMessage struct {
	// Event brackets an edge's inspection, and Progress carries samples from
	// the existing synchronization progress protocol between those brackets.
	Event     *CommandEvent `json:",omitempty"`
	Progress  *Progress     `json:",omitempty"`
	Error     string
	ErrorKind ErrorKind     `json:",omitempty"`
	Edge      *conflictEdge `json:",omitempty"`
	Conflict  *ConflictRoot `json:",omitempty"`
	Problem   *Problem      `json:",omitempty"`
	Checkout  *Checkout     `json:",omitempty"`
	Done      bool          `json:",omitempty"`
}

// RespondWithPathConflicts streams bounded records for every actual path edge.
func RespondWithPathConflicts(connection io.ReadWriteCloser, edges []EdgeConflicts, failure error) error {
	defer connection.Close()
	if failure != nil {
		return writeMessage(connection, &pathConflictsMessage{Error: failure.Error(), ErrorKind: ClassifyError(failure)})
	}
	writer := bufio.NewWriter(connection)
	for _, edge := range edges {
		if err := writeMessage(writer, &pathConflictsMessage{Edge: &conflictEdge{edge.Alpha, edge.Beta, edge.Session, edge.SourceAlpha}}); err != nil {
			return err
		}
		for i := range edge.Listing.Conflicts {
			conflict, err := boundConflict(&edge.Listing.Conflicts[i])
			if err != nil {
				if sendErr := writeMessage(writer, &pathConflictsMessage{Error: err.Error(), ErrorKind: ClassifyError(err)}); sendErr != nil {
					return sendErr
				}
				return writer.Flush()
			}
			if err = writeMessage(writer, &pathConflictsMessage{Conflict: conflict}); err != nil {
				return err
			}
		}
		for i := range edge.Listing.Problems {
			if err := writeMessage(writer, &pathConflictsMessage{Problem: &edge.Listing.Problems[i]}); err != nil {
				return err
			}
		}
		for i := range edge.Listing.Checkouts {
			if err := writeMessage(writer, &pathConflictsMessage{Checkout: &edge.Listing.Checkouts[i]}); err != nil {
				return err
			}
		}
	}
	if err := writeMessage(writer, &pathConflictsMessage{Done: true}); err != nil {
		return err
	}
	return writer.Flush()
}

// ListPathConflicts receives the same edge schema for pairs and multi-edge paths.
func ListPathConflicts(ctx context.Context, connection io.ReadWriteCloser) ([]EdgeConflicts, error) {
	return ListPathConflictsWithProgress(ctx, connection, nil)
}

// SendPathConflictProgress associates ordinary progress samples with an edge.
func SendPathConflictProgress(connection io.Writer, event *CommandEvent) error {
	return writeMessage(connection, &pathConflictsMessage{Event: event})
}

// ListPathConflictsWithProgress preserves edge context during path inspection.
func ListPathConflictsWithProgress(ctx context.Context, connection io.ReadWriteCloser, progress func(*CommandEvent)) ([]EdgeConflicts, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	reader := bufio.NewReader(connection)
	var edges []EdgeConflicts
	var current CommandEvent
	for {
		var message pathConflictsMessage
		if err := readMessage(reader, &message); err != nil {
			return nil, ConnectionError(fmt.Errorf("unable to receive path conflicts: %w", err))
		}
		if message.Error != "" {
			return nil, receivedError(message.ErrorKind, errors.New(message.Error))
		}
		if message.Done {
			return edges, nil
		} else if message.Event != nil {
			current = *message.Event
			if progress != nil {
				progress(message.Event)
			}
			continue
		} else if message.Progress != nil {
			event := current
			event.Progress = message.Progress
			if progress != nil {
				progress(&event)
			}
			continue
		}
		if e := message.Edge; e != nil {
			edges = append(edges, EdgeConflicts{Alpha: e.Alpha, Beta: e.Beta, Session: e.Session, SourceAlpha: e.SourceAlpha, Listing: &Listing{}})
			continue
		}
		if len(edges) == 0 {
			return nil, ConnectionError(errors.New("path conflict without an edge"))
		}
		listing := edges[len(edges)-1].Listing
		if message.Conflict != nil {
			listing.Conflicts = append(listing.Conflicts, *message.Conflict)
		} else if message.Problem != nil {
			listing.Problems = append(listing.Problems, *message.Problem)
		} else if message.Checkout != nil {
			listing.Checkouts = append(listing.Checkouts, *message.Checkout)
		} else {
			return nil, ConnectionError(errors.New("invalid path conflict listing"))
		}
	}
}
