package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFlushLargeReport(t *testing.T) {
	// A single directory conflict can exceed the protocol limit despite
	// truncation of the number of conflicts. Both change lists contribute.
	conflict := Conflict{Root: "directory", ExcludedChanges: 3}
	for i := 0; i < 18000; i++ {
		conflict.Beta = append(conflict.Beta, Change{
			Path: fmt.Sprintf("directory/file-%040d", i), Old: "file", New: "file",
		})
	}
	conflict.Alpha = []Change{{Path: "directory", Old: "directory", New: "file"}}
	report := &Report{Conflicts: []Conflict{conflict}, ExcludedConflicts: 2}
	original, err := json.Marshal(report)
	if err != nil || len(original) <= maximumMessageSize {
		t.Fatal("test report doesn't exceed the protocol limit:", err)
	}
	client, server := net.Pipe()
	responseErrors := make(chan error, 1)
	go func() { responseErrors <- RespondToFlush(server, report, nil) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received, err := Flush(ctx, client)
	if err != nil {
		t.Fatal("unable to receive large report:", err)
	} else if err := <-responseErrors; err != nil {
		t.Fatal("unable to send large report:", err)
	} else if len(received.Conflicts) != 1 || received.ExcludedConflicts != 2 {
		t.Fatalf("conflict root omitted: %+v", received)
	}
	actual := received.Conflicts[0]
	if actual.Root != conflict.Root || actual.ExcludedChanges <= conflict.ExcludedChanges ||
		uint64(len(actual.Alpha)+len(actual.Beta))+actual.ExcludedChanges != uint64(len(conflict.Alpha)+len(conflict.Beta))+conflict.ExcludedChanges {
		t.Fatalf("changes truncated incorrectly: kept %d/%d, excluded %d", len(actual.Alpha), len(actual.Beta), actual.ExcludedChanges)
	}
	if after, err := json.Marshal(report); err != nil || !bytes.Equal(original, after) {
		t.Fatal("original report modified:", err)
	}
}

func TestBoundReportOversizedEntries(t *testing.T) {
	// Bound actual encoded size, including escaping and individual strings
	// that exceed the limit, and preserve existing omission counts.
	report := &Report{
		Conflicts:         []Conflict{{Root: strings.Repeat("\x00", maximumMessageSize)}},
		ExcludedConflicts: 2,
		Problems:          []Problem{{Path: "file", Error: strings.Repeat("x", maximumMessageSize)}},
		ExcludedProblems:  3,
	}
	bounded, err := boundReport(report)
	if err != nil {
		t.Fatal(err)
	} else if bounded.ExcludedConflicts != 3 || bounded.ExcludedProblems != 4 || bounded.Empty() {
		t.Fatalf("incorrect omission counts: %+v", bounded)
	} else if data, err := json.Marshal(&flushResponse{Report: bounded}); err != nil || len(data) > maximumMessageSize {
		t.Fatal("report still exceeds the limit:", err)
	}
}

func TestRespondToFlushWriteFailure(t *testing.T) {
	client, server := net.Pipe()
	client.Close()
	if err := RespondToFlush(server, &Report{}, nil); err == nil {
		t.Fatal("write failure not reported")
	}
}
