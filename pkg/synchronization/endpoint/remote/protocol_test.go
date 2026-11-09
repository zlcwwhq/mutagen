package remote

import (
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/rsync"
)

// TestTransitionResponseSubmoduleResults tests that transition responses can
// carry the results of transitions that attach or follow a submodule's Git
// entry, which are GitLink entries at the top level of their archives.
func TestTransitionResponseSubmoduleResults(t *testing.T) {
	link := &core.Entry{
		Kind:   core.EntryKind_GitLink,
		Digest: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
		Url:    "https://example.org/repository.git",
	}
	response := &TransitionResponse{Results: []*core.Archive{{Content: link}, {}}}
	if err := response.ensureValid(2); err != nil {
		t.Error("response with GitLink result considered invalid:", err)
	}
}

// TestScanRequestSubmodules tests the validation of the ancestor's GitLink
// entries in scan requests.
func TestScanRequestSubmodules(t *testing.T) {
	link := &core.Entry{
		Kind:   core.EntryKind_GitLink,
		Digest: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
		Url:    "https://example.org/repository.git",
		Seed:   "seed",
	}
	tests := []struct {
		description string
		submodules  map[string]*core.Entry
		valid       bool
	}{
		{"none", nil, true},
		{"valid", map[string]*core.Entry{"sub": link, "sub/nested": link}, true},
		{"root", map[string]*core.Entry{"": link}, false},
		{"nil", map[string]*core.Entry{"sub": nil}, false},
		{"directory", map[string]*core.Entry{"sub": {Kind: core.EntryKind_Directory}}, false},
		{"invalid", map[string]*core.Entry{"sub": {Kind: core.EntryKind_GitLink, Digest: link.Digest}}, false},
	}
	for _, test := range tests {
		request := &ScanRequest{BaselineSnapshotSignature: &rsync.Signature{}, Submodules: test.submodules}
		if err := request.ensureValid(); (err == nil) != test.valid {
			t.Errorf("%s: incorrect validity: %v", test.description, err)
		}
	}
}
