package synchronization

import (
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// TestCreationSpecificationIgnores tests that creation specifications with
// invalid ignores are rejected before session creation.
func TestCreationSpecificationIgnores(t *testing.T) {
	// Define test cases.
	tests := []struct {
		description   string
		configuration *synchronization.Configuration
		expectValid   bool
	}{
		{"no ignores", &synchronization.Configuration{}, true},
		{"valid ignores", &synchronization.Configuration{Ignores: []string{"*.log"}}, true},
		{"invalid Mutagen-style ignores", &synchronization.Configuration{Ignores: []string{"!"}}, false},
		{"invalid Docker-style ignores", &synchronization.Configuration{
			IgnoreSyntax: ignore.Syntax_SyntaxDocker,
			Ignores:      []string{"!"},
		}, false},
	}

	// Process test cases.
	alpha, beta := t.TempDir(), t.TempDir()
	for _, test := range tests {
		specification := &CreationSpecification{
			Alpha:              &url.URL{Kind: url.Kind_Synchronization, Path: alpha},
			Beta:               &url.URL{Kind: url.Kind_Synchronization, Path: beta},
			Configuration:      test.configuration,
			ConfigurationAlpha: &synchronization.Configuration{},
			ConfigurationBeta:  &synchronization.Configuration{},
		}
		if err := specification.ensureValid(); test.expectValid && err != nil {
			t.Errorf("%s: valid specification rejected: %v", test.description, err)
		} else if !test.expectValid && err == nil {
			t.Errorf("%s: invalid specification accepted", test.description)
		}
	}
}

// TestCreationSpecificationTree tests that creation specifications with
// invalid workspace tree membership are rejected before session creation.
func TestCreationSpecificationTree(t *testing.T) {
	alpha, beta := t.TempDir(), t.TempDir()
	for _, test := range []struct {
		description string
		tree        *synchronization.TreeMembership
		expectValid bool
	}{
		{"no membership", nil, true},
		{"valid membership", &synchronization.TreeMembership{Identifier: "project", Root: "a", Alpha: "a", Beta: "b"}, true},
		{"missing child", &synchronization.TreeMembership{Identifier: "project", Root: "a", Alpha: "a"}, false},
	} {
		specification := &CreationSpecification{
			Alpha:              &url.URL{Kind: url.Kind_Synchronization, Path: alpha},
			Beta:               &url.URL{Kind: url.Kind_Synchronization, Path: beta},
			Configuration:      &synchronization.Configuration{},
			ConfigurationAlpha: &synchronization.Configuration{},
			ConfigurationBeta:  &synchronization.Configuration{},
			Tree:               test.tree,
		}
		if err := specification.ensureValid(); test.expectValid && err != nil {
			t.Errorf("%s: valid specification rejected: %v", test.description, err)
		} else if !test.expectValid && err == nil {
			t.Errorf("%s: invalid specification accepted", test.description)
		}
	}
}
