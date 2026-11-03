package remote

import (
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
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
