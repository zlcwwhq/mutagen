package synchronization

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestExportRepositoryConflict tests the export of repository conflicts and
// their JSON representation.
func TestExportRepositoryConflict(t *testing.T) {
	if exportRepositoryConflict(nil) != nil {
		t.Error("nil conflict exported as non-nil")
	}
	conflict := &core.RepositoryConflict{
		Alpha: &core.Repository{Commit: bytes.Repeat([]byte{0x1a}, 20), Branch: "main", Url: "https://example.org/r.git"},
		Beta:  &core.Repository{Commit: bytes.Repeat([]byte{0x2b}, 20)},
	}
	encoded, err := json.Marshal(exportRepositoryConflict(conflict))
	if err != nil {
		t.Fatal("unable to encode conflict:", err)
	}
	expected := `{"alpha":{"commit":"1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a","branch":"main"},` +
		`"beta":{"commit":"2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"}}`
	if string(encoded) != expected {
		t.Errorf("incorrect JSON:\n%s\nexpected:\n%s", encoded, expected)
	}
	conflict.Ancestor = &core.Repository{Commit: bytes.Repeat([]byte{0x0c}, 20), Branch: "main"}
	if exported := exportRepositoryConflict(conflict); exported.Ancestor == nil ||
		*exported.Ancestor != (Repository{Commit: "0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c", Branch: "main"}) {
		t.Errorf("incorrect ancestor: %+v", exported.Ancestor)
	}
}
