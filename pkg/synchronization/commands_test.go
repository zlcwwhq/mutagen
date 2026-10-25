package synchronization

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/state"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
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

	// Resolution listings use the same actual changes as warnings, including
	// directory descendants and type changes, without deriving history from
	// the current copies' types.
	listed := resolution(conflicts[len(conflicts)-1], "sha1")
	if !reflect.DeepEqual(listed.Changes(), report.Conflicts[0]) {
		t.Fatalf("listing and warning disagree: %+v, %+v", listed.Changes(), report.Conflicts[0])
	}

	// Sessions without conflicts or problems produce empty reports.
	c.state = &State{AlphaState: &EndpointState{}, BetaState: &EndpointState{}}
	if report := c.remoteReport(); !report.Empty() {
		t.Fatalf("non-empty report: %+v", report)
	}
}

func TestRemoteFileConflictDigests(t *testing.T) {
	// Conflicts between two files report both copies' digests, with their
	// common ancestor's digest if they have one, so that checkouts can verify
	// that they download the listed copies. Other conflicts report none.
	file := func(digest byte) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_File, Digest: []byte{digest}}
	}
	link := &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "target"}
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
		for _, test := range []struct {
			ancestor, alpha, beta *core.Entry
			expected              remote.ConflictRoot
		}{
			{file(1), file(2), file(3), remote.ConflictRoot{AlphaDigest: "02", BetaDigest: "03", BaseDigest: "01", Algorithm: "sha1"}},
			{nil, file(2), file(3), remote.ConflictRoot{AlphaDigest: "02", BetaDigest: "03", Algorithm: "sha1"}},
			{file(1), link, file(3), remote.ConflictRoot{}},
		} {
			_, _, _, conflicts := core.Reconcile(wrap(test.ancestor), wrap(test.alpha), wrap(test.beta), mode)
			if len(conflicts) != 1 {
				t.Fatalf("unexpected conflicts: %+v", conflicts)
			}
			root := resolution(conflicts[0], "sha1")
			if root.AlphaDigest != test.expected.AlphaDigest || root.BetaDigest != test.expected.BetaDigest ||
				root.BaseDigest != test.expected.BaseDigest || root.Algorithm != test.expected.Algorithm {
				t.Fatalf("mode %d: unexpected digests for %+v: %+v", mode, conflicts[0], root)
			}
		}
	}
}

func TestRemoteTakableCopies(t *testing.T) {
	// Either copy of a conflict other than between two files can be taken in
	// both safe modes, since it's written to the other endpoint, unless it
	// can't be synchronized.
	file := func(digest byte) *core.Entry {
		return &core.Entry{Kind: core.EntryKind_File, Digest: []byte{digest}}
	}
	link := &core.Entry{Kind: core.EntryKind_SymbolicLink, Target: "target"}
	untracked := &core.Entry{Kind: core.EntryKind_Untracked}
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
		for _, test := range []struct {
			ancestor, alpha, beta     *core.Entry
			alphaTakable, betaTakable bool
		}{
			{file(1), link, file(3), true, true},
			{file(1), file(3), link, true, true},
			{nil, file(2), untracked, true, false},
		} {
			_, _, _, conflicts := core.Reconcile(wrap(test.ancestor), wrap(test.alpha), wrap(test.beta), mode)
			if len(conflicts) != 1 {
				t.Fatalf("mode %d: unexpected conflicts: %+v", mode, conflicts)
			} else if root := resolution(conflicts[0], "sha1"); root.Files ||
				root.AlphaTakable != test.alphaTakable || root.BetaTakable != test.betaTakable {
				t.Fatalf("mode %d: unexpected resolution of %+v: %+v", mode, conflicts[0], root)
			}
		}
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
					} else if root := resolution(conflicts[0], "sha1"); root.Path != "conflict" || root.Resolvable() {
						t.Fatalf("directory conflict reported as resolvable: %+v", root)
					}
				})
			}
		}
	}
}

func TestCommandExecutionLocation(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		protocol                 urlpkg.Protocol
		host, path, expectedHost string
	}{
		{urlpkg.Protocol_Local, "", "/local/project", hostname},
		{urlpkg.Protocol_SSH, "ssh-alias", "project", "ssh-alias"},
		{urlpkg.Protocol_SSH, "ssh-alias", "~other/project", "ssh-alias"},
		{urlpkg.Protocol_Docker, "container", "/root/project", "container"},
	} {
		url := &urlpkg.URL{Protocol: test.protocol, Host: test.host, Path: test.path,
			User: "private-user", Environment: map[string]string{"SECRET": "private"},
			Parameters: map[string]string{"secret": "private"}}
		host, root := commandExecutionLocation(url)
		if host != test.expectedHost || root != test.path {
			t.Fatalf("incorrect execution metadata: %q, %q", host, root)
		}
	}
}
