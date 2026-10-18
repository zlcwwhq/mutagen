package synchronization

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/state"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

func TestCommandRelayFlush(t *testing.T) {
	// Flushes complete once the synchronization loop responds.
	relay := &commandRelay{flushRequests: make(chan chan error, 1)}
	go func() { (<-relay.flushRequests) <- nil }()
	if err := relay.flush(context.Background()); err != nil {
		t.Fatal("flush failed:", err)
	}

	// Flushes fail once synchronization stops, rather than waiting for a
	// response that won't arrive, both after their request is queued and while
	// waiting to queue it (behind the request left by the first case).
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if err := relay.flush(ctx); err != errFlushTerminated {
		t.Fatal("queued flush not terminated:", err)
	} else if err := relay.flush(ctx); err != errFlushTerminated {
		t.Fatal("unqueued flush not terminated:", err)
	}
}

func TestRemoteReport(t *testing.T) {
	// Create a state with more conflicts and problems than are reported, in
	// reverse order.
	file := &core.Entry{Kind: core.EntryKind_File}
	directory := &core.Entry{Kind: core.EntryKind_Directory}
	var conflicts []*core.Conflict
	for i := maximumListConflicts; i >= 0; i-- {
		path := fmt.Sprintf("conflict%02d", i)
		conflicts = append(conflicts, &core.Conflict{
			Root:         path,
			AlphaChanges: []*core.Change{{Path: path, Old: directory}},
			BetaChanges: []*core.Change{{
				Path: path + "/file",
				Old:  file,
				New:  &core.Entry{Kind: core.EntryKind_File, Executable: true},
			}},
		})
	}
	var scanProblems []*core.Problem
	for i := maximumListScanProblems; i >= 0; i-- {
		scanProblems = append(scanProblems, &core.Problem{Path: fmt.Sprintf("scan%02d", i), Error: "unreadable"})
	}
	c := &controller{
		stateLock: state.NewTrackingLock(state.NewTracker()),
		state: &State{
			Conflicts:  conflicts,
			AlphaState: &EndpointState{ScanProblems: scanProblems},
			BetaState:  &EndpointState{TransitionProblems: []*core.Problem{{Path: "transition", Error: "denied"}}},
		},
	}

	// Lists are sorted, truncated, and converted without modifying the state.
	report := c.remoteReport()
	if len(report.Conflicts) != maximumListConflicts || report.ExcludedConflicts != 1 {
		t.Fatalf("conflicts truncated incorrectly: %d, %d", len(report.Conflicts), report.ExcludedConflicts)
	} else if expected := (remote.Conflict{
		Root:  "conflict00",
		Alpha: []remote.Change{{Path: "conflict00", Old: "directory"}},
		Beta:  []remote.Change{{Path: "conflict00/file", Old: "file", New: "executable file"}},
	}); !reflect.DeepEqual(report.Conflicts[0], expected) {
		t.Fatalf("conflict converted incorrectly: %+v", report.Conflicts[0])
	} else if len(report.Problems) != maximumListScanProblems+1 || report.ExcludedProblems != 1 {
		t.Fatalf("problems truncated incorrectly: %d, %d", len(report.Problems), report.ExcludedProblems)
	} else if first, last := report.Problems[0], report.Problems[maximumListScanProblems]; !reflect.DeepEqual(first, remote.Problem{Alpha: true, Path: "scan00", Error: "unreadable"}) ||
		!reflect.DeepEqual(last, remote.Problem{Path: "transition", Error: "denied"}) {
		t.Fatalf("problems converted incorrectly: %+v, %+v", first, last)
	} else if c.state.Conflicts[0].Root != fmt.Sprintf("conflict%02d", maximumListConflicts) || c.state.AlphaState.ScanProblems[0].Path != fmt.Sprintf("scan%02d", maximumListScanProblems) {
		t.Fatal("state modified")
	}

	// Sessions without conflicts or problems produce empty reports.
	c.state = &State{AlphaState: &EndpointState{}, BetaState: &EndpointState{}}
	if report := c.remoteReport(); !report.Empty() {
		t.Fatalf("non-empty report: %+v", report)
	}
}

func TestRemoteDirectoryConflicts(t *testing.T) {
	file := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{1}}
	changed := &core.Entry{Kind: core.EntryKind_File, Digest: []byte{2}}
	directory := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"child": file}}
	modified := &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"child": changed}}
	wrap := func(entry *core.Entry) *core.Entry {
		if entry == nil {
			return &core.Entry{Kind: core.EntryKind_Directory}
		}
		return &core.Entry{Kind: core.EntryKind_Directory, Contents: map[string]*core.Entry{"conflict": entry}}
	}
	for _, mode := range []core.SynchronizationMode{
		core.SynchronizationMode_SynchronizationModeTwoWaySafe,
		core.SynchronizationMode_SynchronizationModeOneWaySafe,
	} {
		for _, modifiedDirectory := range []bool{false, true} {
			for _, directoryOnAlpha := range []bool{false, true} {
				t.Run(fmt.Sprintf("mode=%d/modified=%t/alpha=%t", mode, modifiedDirectory, directoryOnAlpha), func(t *testing.T) {
					ancestor, alpha, beta := (*core.Entry)(nil), directory, file
					if modifiedDirectory {
						ancestor, alpha = directory, modified
					}
					if !directoryOnAlpha {
						alpha, beta = beta, alpha
					}
					_, _, _, conflicts := core.Reconcile(wrap(ancestor), wrap(alpha), wrap(beta), mode)
					if len(conflicts) != 1 {
						t.Fatalf("unexpected conflicts: %+v", conflicts)
					} else if root := resolution(conflicts[0], mode, "sha1"); root.Path != "conflict" || root.Resolvable() {
						t.Fatalf("directory conflict reported as resolvable: %+v", root)
					}
				})
			}
		}
	}
}
