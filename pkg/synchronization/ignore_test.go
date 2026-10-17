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
