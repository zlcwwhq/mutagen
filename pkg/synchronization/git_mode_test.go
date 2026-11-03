package synchronization

import (
	"testing"
)

// TestGitModeUnmarshal tests that unmarshaling from a string specification
// succeeds for GitMode.
func TestGitModeUnmarshal(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		text          string
		expectedMode  GitMode
		expectFailure bool
	}{
		{"", GitMode_GitModeDefault, true},
		{"asdf", GitMode_GitModeDefault, true},
		{"true", GitMode_GitModeDefault, true},
		{"false", GitMode_GitModeDefault, true},
		{"Enabled", GitMode_GitModeDefault, true},
		{"disabled", GitMode_GitModeDisabled, false},
		{"enabled", GitMode_GitModeEnabled, false},
	}

	// Process test cases.
	for _, testCase := range testCases {
		var mode GitMode
		if err := mode.UnmarshalText([]byte(testCase.text)); err != nil {
			if !testCase.expectFailure {
				t.Errorf("unable to unmarshal text (%s): %s", testCase.text, err)
			}
		} else if testCase.expectFailure {
			t.Error("unmarshaling succeeded unexpectedly for text:", testCase.text)
		} else if mode != testCase.expectedMode {
			t.Errorf(
				"unmarshaled mode (%s) does not match expected (%s)",
				mode,
				testCase.expectedMode,
			)
		}
	}
}

// TestGitModeMarshal tests that GitMode values marshal to their expected text
// representations and that supported values round-trip through them.
func TestGitModeMarshal(t *testing.T) {
	// Verify the text of each value, including unsupported ones.
	for mode, expected := range map[GitMode]string{
		GitMode_GitModeDefault:       "",
		GitMode_GitModeDisabled:      "disabled",
		GitMode_GitModeEnabled:       "enabled",
		(GitMode_GitModeEnabled + 1): "unknown",
	} {
		if text, err := mode.MarshalText(); err != nil {
			t.Errorf("unable to marshal mode (%s): %s", mode, err)
		} else if string(text) != expected {
			t.Errorf("marshaled text (%s) does not match expected (%s)", text, expected)
		}
	}

	// Verify that supported values round-trip.
	for _, mode := range []GitMode{
		GitMode_GitModeDisabled,
		GitMode_GitModeEnabled,
	} {
		var unmarshaled GitMode
		if text, err := mode.MarshalText(); err != nil {
			t.Errorf("unable to marshal mode (%s): %s", mode, err)
		} else if err := unmarshaled.UnmarshalText(text); err != nil {
			t.Errorf("unable to unmarshal text (%s): %s", text, err)
		} else if unmarshaled != mode {
			t.Errorf("round-tripped mode (%s) does not match original (%s)", unmarshaled, mode)
		}
	}
}

// TestGitModeIsDefault tests that GitMode default detection works as expected.
func TestGitModeIsDefault(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		mode          GitMode
		expectDefault bool
	}{
		{GitMode_GitModeDefault, true},
		{GitMode_GitModeDisabled, false},
		{GitMode_GitModeEnabled, false},
		{(GitMode_GitModeEnabled + 1), false},
	}

	// Process test cases.
	for _, testCase := range testCases {
		if isDefault := testCase.mode.IsDefault(); isDefault != testCase.expectDefault {
			t.Errorf(
				"mode default status (%t) does not match expected (%t) for mode %s",
				isDefault,
				testCase.expectDefault,
				testCase.mode,
			)
		}
	}
}

// TestGitModeSupported tests that GitMode support detection works as expected.
func TestGitModeSupported(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		mode            GitMode
		expectSupported bool
	}{
		{GitMode_GitModeDefault, false},
		{GitMode_GitModeDisabled, true},
		{GitMode_GitModeEnabled, true},
		{(GitMode_GitModeEnabled + 1), false},
	}

	// Process test cases.
	for _, testCase := range testCases {
		if supported := testCase.mode.Supported(); supported != testCase.expectSupported {
			t.Errorf(
				"mode support status (%t) does not match expected (%t)",
				supported,
				testCase.expectSupported,
			)
		}
	}
}

// TestGitModeDescription tests that GitMode description generation works as
// expected.
func TestGitModeDescription(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		mode                GitMode
		expectedDescription string
	}{
		{GitMode_GitModeDefault, "Default"},
		{GitMode_GitModeDisabled, "Disabled"},
		{GitMode_GitModeEnabled, "Enabled"},
		{(GitMode_GitModeEnabled + 1), "Unknown"},
	}

	// Process test cases.
	for _, testCase := range testCases {
		if description := testCase.mode.Description(); description != testCase.expectedDescription {
			t.Errorf(
				"mode description (%s) does not match expected (%s)",
				description,
				testCase.expectedDescription,
			)
		}
	}
}
