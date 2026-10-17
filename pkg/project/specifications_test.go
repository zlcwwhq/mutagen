package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/configuration/global"
)

// testConfiguration loads project YAML through the production loader.
func testConfiguration(t *testing.T, contents string) *Configuration {
	t.Helper()
	path := filepath.Join(t.TempDir(), DefaultConfigurationFileName)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	configuration, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

// testSpecifications computes specifications for project YAML without global
// configuration.
func testSpecifications(t *testing.T, contents string) *SessionSpecifications {
	t.Helper()
	specifications, err := NewSessionSpecifications(testConfiguration(t, contents), nil, "project", false)
	if err != nil {
		t.Fatal(err)
	}
	return specifications
}

// TestSessionSpecifications verifies defaults, overrides, URL resolution,
// ordering, and flush behavior.
func TestSessionSpecifications(t *testing.T) {
	t.Chdir(t.TempDir())
	specifications := testSpecifications(t, `
forward:
  defaults:
    source: "tcp:127.0.0.1:8000"
    destination: "tcp:127.0.0.1:8001"
    socket: {permissionMode: 0600}
    configurationSource: {socket: {permissionMode: 0660}}
  example: {socket: {permissionMode: 0640}}
sync:
  defaults:
    alpha: ./alpha
    beta: ./beta
    maxEntryCount: 100
    ignore: {paths: [default-ignore]}
    configurationAlpha: {watch: {pollingInterval: 5}}
    flushOnCreate: true
  example:
    maxEntryCount: 200
    ignore: {paths: [session-ignore]}
    configurationBeta: {watch: {pollingInterval: 10}}
    flushOnCreate: false
  inherited: {}
`)
	f := specifications.Forwarding[0]
	if f.Configuration.SocketPermissionMode != 0640 || f.ConfigurationSource.SocketPermissionMode != 0660 {
		t.Fatal("incorrect forwarding defaults or overrides")
	}
	s := specifications.Synchronization[0]
	if s.Name != "example" || !filepath.IsAbs(s.Alpha.Path) || !filepath.IsAbs(s.Beta.Path) {
		t.Fatal("incorrect ordering or relative URL resolution")
	}
	if s.Configuration.MaximumEntryCount != 200 || strings.Join(s.Configuration.Ignores, ",") != "default-ignore,session-ignore" ||
		s.ConfigurationAlpha.WatchPollingInterval != 5 || s.ConfigurationBeta.WatchPollingInterval != 10 {
		t.Fatal("incorrect synchronization defaults or overrides")
	}
	if specifications.FlushOnCreate["example"] || !specifications.FlushOnCreate["inherited"] {
		t.Fatal("incorrect flush-on-create inheritance")
	}
	if f.Labels[LabelKey] != "project" || s.Labels[LabelKey] != "project" || f.Paused || s.Paused {
		t.Fatal("incorrect project association or paused state")
	}
}

// TestSessionSpecificationsGlobalConfiguration verifies that project defaults
// and session configurations take priority over global configuration.
func TestSessionSpecificationsGlobalConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "global.yml")
	contents := "sync:\n  defaults:\n    maxEntryCount: 50\n    maxStagingFileSize: 1000\n    ignore: {paths: [global-ignore]}\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	globalConfiguration, err := global.LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	configuration := testConfiguration(t, `
sync:
  defaults: {maxEntryCount: 100}
  example: {alpha: ./alpha, beta: ./beta, ignore: {paths: [session-ignore]}}
`)
	specifications, err := NewSessionSpecifications(configuration, globalConfiguration, "project", true)
	if err != nil {
		t.Fatal(err)
	}
	s := specifications.Synchronization[0]
	if s.Configuration.MaximumEntryCount != 100 || s.Configuration.MaximumStagingFileSize != 1000 ||
		strings.Join(s.Configuration.Ignores, ",") != "global-ignore,session-ignore" || !s.Paused {
		t.Fatal("incorrect global configuration layering or paused state")
	}
}

// TestSessionSpecificationsEndpointModes verifies that endpoint-specific modes
// are validated per layer, as by the daemon and sync create, so that existing
// project files remain valid.
func TestSessionSpecificationsEndpointModes(t *testing.T) {
	t.Chdir(t.TempDir())
	specifications := testSpecifications(t, `
sync:
  example:
    alpha: ./alpha
    beta: ./beta
    configurationBeta: {permissions: {defaultFileMode: 0755}}
`)
	if specifications.Synchronization[0].ConfigurationBeta.DefaultFileMode != 0755 {
		t.Fatal("endpoint-specific file mode was not retained")
	}
}

// TestSessionSpecificationsInvalid verifies that configuration that the daemon
// would reject is detected before any sessions are modified.
func TestSessionSpecificationsInvalid(t *testing.T) {
	t.Chdir(t.TempDir())
	for name, contents := range map[string]string{
		"session name":            "sync:\n  1invalid: {alpha: ./alpha, beta: ./beta}\n",
		"missing URL":             "sync:\n  example: {alpha: ./alpha}\n",
		"endpoint-specific mode":  "sync:\n  example: {alpha: ./alpha, beta: ./beta, configurationBeta: {mode: two-way-safe}}\n",
		"Mutagen-style ignores":   "sync:\n  example: {alpha: ./alpha, beta: ./beta, ignore: {paths: ['!']}}\n",
		"Docker-style ignores":    "sync:\n  example: {alpha: ./alpha, beta: ./beta, ignore: {syntax: docker, paths: ['!']}}\n",
		"default ignores":         "sync:\n  defaults: {ignore: {paths: ['!']}}\n  example: {alpha: ./alpha, beta: ./beta}\n",
		"forwarding session name": "forward:\n  1invalid: {source: 'tcp:127.0.0.1:8000', destination: 'tcp:127.0.0.1:8001'}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSessionSpecifications(testConfiguration(t, contents), nil, "project", false); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
