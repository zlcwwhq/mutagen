package synchronization

import (
	"testing"
)

// TestRemoteCommandModeUnmarshal tests that unmarshaling from a string
// specification succeeds for RemoteCommandMode.
func TestRemoteCommandModeUnmarshal(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		text          string
		expectedMode  RemoteCommandMode
		expectFailure bool
	}{
		{"", RemoteCommandMode_RemoteCommandModeDefault, true},
		{"asdf", RemoteCommandMode_RemoteCommandModeDefault, true},
		{"both", RemoteCommandMode_RemoteCommandModeDefault, true},
		{"false", RemoteCommandMode_RemoteCommandModeDisabled, false},
		{"alpha", RemoteCommandMode_RemoteCommandModeAlpha, false},
		{"beta", RemoteCommandMode_RemoteCommandModeBeta, false},
		{"true", RemoteCommandMode_RemoteCommandModeBoth, false},
	}

	// Process test cases.
	for _, testCase := range testCases {
		var mode RemoteCommandMode
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

// TestRemoteCommandModeMarshal tests that supported RemoteCommandMode values
// round-trip through their text representations.
func TestRemoteCommandModeMarshal(t *testing.T) {
	for _, mode := range []RemoteCommandMode{
		RemoteCommandMode_RemoteCommandModeDisabled,
		RemoteCommandMode_RemoteCommandModeAlpha,
		RemoteCommandMode_RemoteCommandModeBeta,
		RemoteCommandMode_RemoteCommandModeBoth,
	} {
		var unmarshaled RemoteCommandMode
		if text, err := mode.MarshalText(); err != nil {
			t.Errorf("unable to marshal mode (%s): %s", mode, err)
		} else if err := unmarshaled.UnmarshalText(text); err != nil {
			t.Errorf("unable to unmarshal text (%s): %s", text, err)
		} else if unmarshaled != mode {
			t.Errorf("round-tripped mode (%s) does not match original (%s)", unmarshaled, mode)
		}
	}
}

// TestRemoteCommandModeSupported tests that RemoteCommandMode support detection
// works as expected.
func TestRemoteCommandModeSupported(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		mode            RemoteCommandMode
		expectSupported bool
	}{
		{RemoteCommandMode_RemoteCommandModeDefault, false},
		{RemoteCommandMode_RemoteCommandModeDisabled, true},
		{RemoteCommandMode_RemoteCommandModeAlpha, true},
		{RemoteCommandMode_RemoteCommandModeBeta, true},
		{RemoteCommandMode_RemoteCommandModeBoth, true},
		{(RemoteCommandMode_RemoteCommandModeBoth + 1), false},
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

// TestRemoteCommandModeDescription tests that RemoteCommandMode description
// generation works as expected.
func TestRemoteCommandModeDescription(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		mode                RemoteCommandMode
		expectedDescription string
	}{
		{RemoteCommandMode_RemoteCommandModeDefault, "Default"},
		{RemoteCommandMode_RemoteCommandModeDisabled, "Disabled"},
		{RemoteCommandMode_RemoteCommandModeAlpha, "Executed on Alpha"},
		{RemoteCommandMode_RemoteCommandModeBeta, "Executed on Beta"},
		{RemoteCommandMode_RemoteCommandModeBoth, "Executed on Alpha and Beta"},
		{(RemoteCommandMode_RemoteCommandModeBoth + 1), "Unknown"},
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

// TestRemoteCommandModeExecutes tests that RemoteCommandMode computes the
// executing endpoints as expected.
func TestRemoteCommandModeExecutes(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		mode          RemoteCommandMode
		expectedAlpha bool
		expectedBeta  bool
	}{
		{RemoteCommandMode_RemoteCommandModeDefault, false, false},
		{RemoteCommandMode_RemoteCommandModeDisabled, false, false},
		{RemoteCommandMode_RemoteCommandModeAlpha, true, false},
		{RemoteCommandMode_RemoteCommandModeBeta, false, true},
		{RemoteCommandMode_RemoteCommandModeBoth, true, true},
		{(RemoteCommandMode_RemoteCommandModeBoth + 1), false, false},
	}

	// Process test cases.
	for _, testCase := range testCases {
		if alpha, beta := testCase.mode.Executes(); alpha != testCase.expectedAlpha || beta != testCase.expectedBeta {
			t.Errorf(
				"executing endpoints for mode (%s) incorrect: alpha=%t, beta=%t",
				testCase.mode,
				alpha,
				beta,
			)
		}
	}
}
