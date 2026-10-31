package synchronization

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestGitLinkEntryJSONRoundTrip tests that submodule entries convert to their
// model representation and survive a JSON round trip, both directly and as the
// contents of a directory.
func TestGitLinkEntryJSONRoundTrip(t *testing.T) {
	// Create an internal entry and verify its conversion.
	internal := &core.Entry{
		Kind:   core.EntryKind_GitLink,
		Digest: []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
		Url:    "https://example.org/submodule",
	}
	entry := newEntryFromInternalEntry(internal)
	if entry.Kind != core.EntryKind_GitLink {
		t.Error("entry kind does not match expected:", entry.Kind)
	} else if entry.GitLinkEntry == nil {
		t.Fatal("submodule fields are missing")
	} else if entry.Commit != "0123456789abcdef" {
		t.Error("commit does not match expected:", entry.Commit)
	} else if entry.URL != internal.Url {
		t.Error("URL does not match expected:", entry.URL)
	} else if entry.DirectoryEntry != nil || entry.FileEntry != nil ||
		entry.SymbolicLinkEntry != nil || entry.ProblematicEntry != nil {
		t.Error("fields of other entry kinds are present")
	}

	// Verify the JSON representation.
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal("unable to marshal entry:", err)
	}
	expected := `{"kind":"gitlink","commit":"0123456789abcdef","url":"https://example.org/submodule"}`
	if string(data) != expected {
		t.Errorf("JSON representation does not match expected: %s != %s", data, expected)
	}

	// Verify round trips of the entry itself and of a directory containing it.
	for _, original := range []*Entry{
		entry,
		newEntryFromInternalEntry(&core.Entry{
			Contents: map[string]*core.Entry{"submodule": internal},
		}),
	} {
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal("unable to marshal entry:", err)
		}
		decoded := &Entry{}
		if err := json.Unmarshal(data, decoded); err != nil {
			t.Fatal("unable to unmarshal entry:", err)
		} else if !reflect.DeepEqual(decoded, original) {
			t.Errorf("entry does not round trip: %s", data)
		}
	}
}
