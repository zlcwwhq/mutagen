package synchronization

import (
	"testing"
)

// TestConfigurationRemotePrologue tests validation, comparison, and merging of
// remote prologues, which may be specified on an endpoint-specific basis.
func TestConfigurationRemotePrologue(t *testing.T) {
	// Verify validation.
	for _, endpointSpecific := range []bool{false, true} {
		if err := (&Configuration{RemotePrologue: "~/prologue.sh"}).EnsureValid(endpointSpecific); err != nil {
			t.Errorf("valid remote prologue rejected (endpoint-specific: %t): %v", endpointSpecific, err)
		}
		if (&Configuration{RemotePrologue: "prologue\x00.sh"}).EnsureValid(endpointSpecific) == nil {
			t.Errorf("remote prologue with null byte accepted (endpoint-specific: %t)", endpointSpecific)
		}
	}

	// Verify comparison.
	if (&Configuration{RemotePrologue: "a.sh"}).Equal(&Configuration{RemotePrologue: "b.sh"}) {
		t.Error("remote prologue difference not detected")
	}

	// Verify that higher-priority prologues take precedence.
	lower, higher := &Configuration{RemotePrologue: "lower.sh"}, &Configuration{RemotePrologue: "higher.sh"}
	if merged := MergeConfigurations(lower, higher); merged.RemotePrologue != "higher.sh" {
		t.Error("higher-priority remote prologue not used:", merged.RemotePrologue)
	} else if merged = MergeConfigurations(lower, &Configuration{}); merged.RemotePrologue != "lower.sh" {
		t.Error("lower-priority remote prologue not inherited:", merged.RemotePrologue)
	}
}

// TestConfigurationGitMode tests validation, comparison, and merging of Git
// modes, which can't be specified on an endpoint-specific basis.
func TestConfigurationGitMode(t *testing.T) {
	// Verify validation.
	for _, mode := range []GitMode{
		GitMode_GitModeDefault,
		GitMode_GitModeDisabled,
		GitMode_GitModeEnabled,
	} {
		if err := (&Configuration{GitMode: mode}).EnsureValid(false); err != nil {
			t.Errorf("valid Git mode (%s) rejected: %v", mode, err)
		}
		if err := (&Configuration{GitMode: mode}).EnsureValid(true); mode.IsDefault() && err != nil {
			t.Errorf("default Git mode rejected for endpoint-specific configuration: %v", err)
		} else if !mode.IsDefault() && err == nil {
			t.Errorf("Git mode (%s) accepted for endpoint-specific configuration", mode)
		}
	}
	if (&Configuration{GitMode: GitMode_GitModeEnabled + 1}).EnsureValid(false) == nil {
		t.Error("unsupported Git mode accepted")
	}

	// Verify comparison.
	if (&Configuration{GitMode: GitMode_GitModeEnabled}).Equal(&Configuration{}) {
		t.Error("Git mode difference not detected")
	} else if (&Configuration{GitMode: GitMode_GitModeEnabled}).Equal(&Configuration{GitMode: GitMode_GitModeDisabled}) {
		t.Error("Git mode difference not detected")
	} else if !(&Configuration{GitMode: GitMode_GitModeEnabled}).Equal(&Configuration{GitMode: GitMode_GitModeEnabled}) {
		t.Error("identical Git modes not equal")
	}

	// Verify that higher-priority modes take precedence and that unspecified
	// modes are inherited.
	lower, higher := &Configuration{GitMode: GitMode_GitModeEnabled}, &Configuration{GitMode: GitMode_GitModeDisabled}
	if merged := MergeConfigurations(lower, higher); merged.GitMode != GitMode_GitModeDisabled {
		t.Error("higher-priority Git mode not used:", merged.GitMode)
	} else if merged = MergeConfigurations(lower, &Configuration{}); merged.GitMode != GitMode_GitModeEnabled {
		t.Error("lower-priority Git mode not inherited:", merged.GitMode)
	} else if merged = MergeConfigurations(&Configuration{}, &Configuration{}); !merged.GitMode.IsDefault() {
		t.Error("unspecified Git modes not merged to the default:", merged.GitMode)
	}
}

// TestDefaultGitMode tests that the default Git mode of each supported session
// version is a supported, non-default value.
func TestDefaultGitMode(t *testing.T) {
	for _, version := range supportedSessionVersions {
		if mode := version.DefaultGitMode(); !mode.Supported() {
			t.Errorf("unsupported default Git mode (%s) for session version %s", mode, version)
		} else if version == Version_Version1 && mode != GitMode_GitModeDisabled {
			t.Errorf("default Git mode (%s) for session version %s is not disabled", mode, version)
		}
	}
}
