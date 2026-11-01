package sync

import (
	"bytes"
	"io"
	"testing"

	"github.com/fatih/color"

	"github.com/mutagen-io/mutagen/cmd/mutagen/common"
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

// TestPrintProblems tests that printProblems lists warnings separately from
// other problems, and that it counts excluded problems as what they must be.
func TestPrintProblems(t *testing.T) {
	failure := &core.Problem{Path: "failure", Error: "unable to write"}
	warning := &core.Problem{Path: "warning", Error: "ignored", Warning: true}
	tests := []struct {
		description string
		problems    []*core.Problem
		excluded    uint64
		mode        common.SessionDisplayMode
		expected    string
	}{
		{"nothing", nil, 0, common.SessionDisplayModeList, ""},
		{"failures", []*core.Problem{failure}, 0, common.SessionDisplayModeList, "\tTransition problems: 1\n"},
		{"warnings", []*core.Problem{warning}, 0, common.SessionDisplayModeList, "\tTransition warnings: 1\n"},
		{
			"both",
			[]*core.Problem{failure, warning}, 0, common.SessionDisplayModeList,
			"\tTransition problems: 1\n\tTransition warnings: 1\n",
		},
		{"excluded warnings", []*core.Problem{warning}, 2, common.SessionDisplayModeList, "\tTransition warnings: 3\n"},
		{"excluded unknowns", []*core.Problem{failure}, 2, common.SessionDisplayModeList, "\tTransition problems: 3\n"},
		{
			"long",
			[]*core.Problem{failure, warning}, 2, common.SessionDisplayModeListLong,
			"\tTransition problems:\n\t\tfailure: unable to write\n" +
				"\tTransition warnings:\n\t\twarning: ignored\n\t\t...+2 more...\n",
		},
		{"monitor", []*core.Problem{failure, warning}, 2, common.SessionDisplayModeMonitor, ""},
	}

	// Capture the output of the color package, without colors.
	defer func(output io.Writer, noColor bool) {
		color.Output, color.NoColor = output, noColor
	}(color.Output, color.NoColor)
	color.NoColor = true
	for _, test := range tests {
		var output bytes.Buffer
		color.Output = &output
		printProblems("Transition", test.problems, test.excluded, test.mode)
		if output.String() != test.expected {
			t.Errorf("%s: incorrect output: %q != %q", test.description, output.String(), test.expected)
		}
	}
}
