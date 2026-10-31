package sync

import (
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestFormatSubmodule tests that formatEntry identifies a submodule's commit.
func TestFormatSubmodule(t *testing.T) {
	entry := &core.Entry{
		Kind:   core.EntryKind_GitLink,
		Digest: []byte{0x01, 0x23, 0xab},
		Url:    "https://example.org/submodule",
	}
	if formatted := formatEntry(entry); formatted != "Submodule (0123ab)" {
		t.Errorf("formatted entry does not match expected: %q", formatted)
	}
}
