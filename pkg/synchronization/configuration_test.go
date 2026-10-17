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
