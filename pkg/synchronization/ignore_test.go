package synchronization

import (
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

// TestNewIgnorer tests NewIgnorer.
func TestNewIgnorer(t *testing.T) {
	// Define test cases.
	tests := []struct {
		description   string
		configuration *Configuration
		expectFailure bool
		ignoredPath   string
		nominalPath   string
	}{
		{"default configuration", &Configuration{}, false, "", "file"},
		{"Mutagen-style ignores", &Configuration{Ignores: []string{"*.log"}}, false, "file.log", "file"},
		{"Docker-style ignores", &Configuration{IgnoreSyntax: ignore.Syntax_SyntaxDocker, Ignores: []string{"*.log"}}, false, "file.log", "file"},
		{"default ignores", &Configuration{DefaultIgnores: []string{"*.log"}}, false, "file.log", "file"},
		{"VCS ignores", &Configuration{IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModeIgnore}, false, ".git", "file"},
		{"explicit VCS propagation", &Configuration{IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModePropagate}, false, "", ".git"},
		{"Git mode disabled", &Configuration{GitMode: GitMode_GitModeDisabled}, false, "", ".git"},
		{"Git mode forces VCS ignores", &Configuration{GitMode: GitMode_GitModeEnabled}, false, ".git", "file"},
		{"Git mode with VCS ignores", &Configuration{GitMode: GitMode_GitModeEnabled, IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModeIgnore}, false, ".git", "file"},
		{"Git mode with Docker-style ignores", &Configuration{GitMode: GitMode_GitModeEnabled, IgnoreSyntax: ignore.Syntax_SyntaxDocker, Ignores: []string{"*.log"}}, false, ".git", "file"},
		{"Git mode with explicit VCS propagation", &Configuration{GitMode: GitMode_GitModeEnabled, IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModePropagate}, true, "", ""},
		{"invalid Mutagen-style ignores", &Configuration{Ignores: []string{"!"}}, true, "", ""},
		{"invalid Docker-style ignores", &Configuration{IgnoreSyntax: ignore.Syntax_SyntaxDocker, Ignores: []string{"!"}}, true, "", ""},
		{"invalid default ignores", &Configuration{DefaultIgnores: []string{"!"}}, true, "", ""},
	}

	// Process test cases.
	for _, test := range tests {
		ignorer, err := NewIgnorer(test.configuration, DefaultVersion)
		if test.expectFailure {
			if err == nil {
				t.Errorf("%s: ignorer creation succeeded unexpectedly", test.description)
			}
			continue
		} else if err != nil {
			t.Errorf("%s: unable to create ignorer: %v", test.description, err)
			continue
		}
		if test.ignoredPath != "" {
			if status, _ := ignorer.Ignore(test.ignoredPath, true); status != ignore.IgnoreStatusIgnored {
				t.Errorf("%s: path not ignored: %s", test.description, test.ignoredPath)
			}
		}
		if status, _ := ignorer.Ignore(test.nominalPath, false); status == ignore.IgnoreStatusIgnored {
			t.Errorf("%s: path ignored: %s", test.description, test.nominalPath)
		}
	}
}

// TestGitModeEnabled tests GitModeEnabled.
func TestGitModeEnabled(t *testing.T) {
	// Define test cases.
	tests := []struct {
		description   string
		configuration *Configuration
		expected      bool
	}{
		{"default", &Configuration{}, false},
		{"disabled", &Configuration{GitMode: GitMode_GitModeDisabled}, false},
		{"enabled", &Configuration{GitMode: GitMode_GitModeEnabled}, true},
	}

	// Process test cases.
	for _, test := range tests {
		if enabled := GitModeEnabled(test.configuration, DefaultVersion); enabled != test.expected {
			t.Errorf("%s: mode status (%t) does not match expected (%t)", test.description, enabled, test.expected)
		}
	}
}

// TestEffectiveIgnoresGitMode tests that Git mode forces VCS ignoring in the
// effective ignores that remote uses, and that nothing else does by default.
func TestEffectiveIgnoresGitMode(t *testing.T) {
	// Define test cases.
	tests := []struct {
		description   string
		configuration *Configuration
		expectedVCS   bool
	}{
		{"default", &Configuration{}, false},
		{"VCS propagation", &Configuration{IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModePropagate}, false},
		{"VCS ignores", &Configuration{IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModeIgnore}, true},
		{"mode disabled", &Configuration{GitMode: GitMode_GitModeDisabled}, false},
		{"mode enabled", &Configuration{GitMode: GitMode_GitModeEnabled}, true},
		{"mode enabled with VCS ignores", &Configuration{GitMode: GitMode_GitModeEnabled, IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModeIgnore}, true},
	}

	// Process test cases. The ignores that remote uses carry Git mode exactly
	// when the configuration enables it, so that remote evaluates rule files
	// and submodules for those sessions only.
	for _, test := range tests {
		ignores := EffectiveIgnores(test.configuration, DefaultVersion)
		if ignores.VCS != test.expectedVCS {
			t.Errorf("%s: VCS ignoring (%t) does not match expected (%t)", test.description, ignores.VCS, test.expectedVCS)
		}
		if expectedGit := test.configuration.GitMode == GitMode_GitModeEnabled; ignores.Git != expectedGit {
			t.Errorf("%s: Git mode (%t) does not match expected (%t)", test.description, ignores.Git, expectedGit)
		}
	}
}

// TestNewIgnorerGitModeDefaultVCS tests that Git mode is only incompatible with
// an explicitly requested VCS ignore mode of propagate, and not with the
// default VCS ignore mode, which resolves to propagating VCS directories.
func TestNewIgnorerGitModeDefaultVCS(t *testing.T) {
	// Verify the premise of this test: that the default VCS ignore mode of the
	// session version resolves to propagation.
	if DefaultVersion.DefaultIgnoreVCSMode() != ignore.IgnoreVCSMode_IgnoreVCSModePropagate {
		t.Skip("default VCS ignore mode does not propagate VCS directories")
	}

	// Verify that the mode is accepted with the default VCS ignore mode.
	configuration := &Configuration{GitMode: GitMode_GitModeEnabled}
	if !configuration.IgnoreVCSMode.IsDefault() {
		t.Fatal("VCS ignore mode not default")
	} else if _, err := NewIgnorer(configuration, DefaultVersion); err != nil {
		t.Error("mode rejected with default VCS ignore mode:", err)
	}

	// Verify that the mode is rejected with an explicit VCS ignore mode of
	// propagate when they come from different configurations that are merged,
	// as they can when specified by global and command line configuration.
	merged := MergeConfigurations(
		&Configuration{IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModePropagate},
		&Configuration{GitMode: GitMode_GitModeEnabled},
	)
	if _, err := NewIgnorer(merged, DefaultVersion); err == nil {
		t.Error("mode accepted with explicit VCS ignore mode of propagate")
	}
}
