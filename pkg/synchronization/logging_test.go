package synchronization

import (
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestFormatSubmoduleForLogging tests that formatEntryForLogging identifies a
// submodule's commit and URL.
func TestFormatSubmoduleForLogging(t *testing.T) {
	entry := &core.Entry{
		Kind:   core.EntryKind_GitLink,
		Digest: []byte{0x01, 0x23, 0xab},
		Url:    "https://example.org/submodule",
	}
	expected := "Submodule (Commit 0123ab, URL https://example.org/submodule)"
	if formatted := formatEntryForLogging(entry); formatted != expected {
		t.Errorf("formatted entry does not match expected: %q != %q", formatted, expected)
	}
}
