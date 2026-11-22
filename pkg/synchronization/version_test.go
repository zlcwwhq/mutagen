package synchronization

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/encoding"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// supportedSessionVersions defines the supported session versions that should
// be used in testing. It should be updated as new versions are added.
var supportedSessionVersions = []Version{
	Version_Version1,
	Version_Version2,
	Version_Version3,
}

// TestSupportedVersions verifies that session version support is as expected.
func TestSupportedVersions(t *testing.T) {
	// Set up test cases.
	testCases := []struct {
		version  Version
		expected bool
	}{
		{Version_Invalid, false},
		{Version_Version1, true},
		{Version_Version2, true},
		{Version_Version3, true},
		{Version_Version3 + 1, false},
	}

	// Process test cases.
	for _, testCase := range testCases {
		if supported := testCase.version.Supported(); supported != testCase.expected {
			t.Errorf(
				"session version (%s) support does not match expected: %t != %t",
				testCase.version,
				supported,
				testCase.expected,
			)
		}
	}
}

// TestDefaultWatchPollingIntervalNonZero verifies that
// DefaultWatchPollingInterval results are non-zero, which is required for watch
// operations.
func TestDefaultWatchPollingIntervalNonZero(t *testing.T) {
	for _, version := range supportedSessionVersions {
		if version.DefaultWatchPollingInterval() == 0 {
			t.Error("zero-valued default watch polling interval")
		}
	}
}

// TestDefaultFileModeValid verifies that DefaultFileMode results are valid for
// use in the default permissions mode.
func TestDefaultFileModeValid(t *testing.T) {
	for _, version := range supportedSessionVersions {
		if err := core.EnsureDefaultFileModeValid(
			version.DefaultPermissionsMode(),
			version.DefaultFileMode(),
		); err != nil {
			t.Error("invalid default file mode:", err)
		}
	}
}

// TestDefaultDirectoryModeValid verifies that DefaultDirectoryMode results are
// valid for use in the default permissions mode.
func TestDefaultDirectoryModeValid(t *testing.T) {
	for _, version := range supportedSessionVersions {
		if err := core.EnsureDefaultDirectoryModeValid(
			version.DefaultPermissionsMode(),
			version.DefaultDirectoryMode(),
		); err != nil {
			t.Error("invalid default directory mode:", err)
		}
	}
}

// TestGitModeVersionDefaults verifies that the Git mode and restricted
// submission session versions have the defaults of the default session
// version, so that the version that a session gets for its configuration
// doesn't change what the configuration means.
func TestGitModeVersionDefaults(t *testing.T) {
	versionType := reflect.TypeOf(DefaultVersion)
	checked := 0
	for m := 0; m < versionType.NumMethod(); m++ {
		method := versionType.Method(m)
		if !strings.HasPrefix(method.Name, "Default") || method.Type.NumIn() != 1 {
			continue
		}
		expected := method.Func.Call([]reflect.Value{reflect.ValueOf(DefaultVersion)})
		for _, version := range []Version{gitModeVersion, restrictedSubmissionVersion} {
			actual := method.Func.Call([]reflect.Value{reflect.ValueOf(version)})
			if !reflect.DeepEqual(expected[0].Interface(), actual[0].Interface()) {
				t.Errorf("%s differs for version %s: %v != %v", method.Name, version, actual[0], expected[0])
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no defaults checked")
	}
}

// TestGitModeSessionVersion verifies that sessions in Git mode get the Git mode
// session version, which daemons that don't support Git mode don't load, both
// when they're created and when a session in Git mode is loaded with the
// default version, as sessions created before the Git mode version existed
// are, and that other sessions keep the default version.
func TestGitModeSessionVersion(t *testing.T) {
	// Create a manager with its own data directory, which avoids t.TempDir,
	// whose long paths could exceed socket path limits on macOS.
	data, err := os.MkdirTemp("", "mtg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	manager, err := NewManager(nil)
	if err != nil {
		t.Fatal("unable to create manager:", err)
	}
	base := starEdgeTestDirectory(t)

	// Create paused sessions, which don't connect, in Git mode and otherwise.
	create := func(name string, configuration *Configuration) string {
		t.Helper()
		identifier, err := manager.Create(
			context.Background(),
			starEdgeTestLocal(filepath.Join(base, name, "alpha")),
			starEdgeTestLocal(filepath.Join(base, name, "beta")),
			configuration, &Configuration{}, &Configuration{},
			name, nil, nil, true, "",
		)
		if err != nil {
			manager.Shutdown()
			t.Fatal("unable to create session:", err)
		}
		return identifier
	}
	gitMode := create("git", &Configuration{GitMode: GitMode_GitModeEnabled})
	ordinary := create("ordinary", &Configuration{})
	disabled := create("disabled", &Configuration{GitMode: GitMode_GitModeDisabled})
	expected := map[string]Version{gitMode: gitModeVersion, ordinary: DefaultVersion, disabled: DefaultVersion}

	// recorded reads the version of a session from disk.
	recorded := func(identifier string) Version {
		t.Helper()
		path, err := pathForSession(identifier)
		if err != nil {
			t.Fatal(err)
		}
		session := &Session{}
		if err := encoding.LoadAndUnmarshalProtobuf(path, session); err != nil {
			t.Fatal("unable to load session:", err)
		}
		return session.Version
	}
	for identifier, version := range expected {
		if actual := recorded(identifier); actual != version {
			t.Errorf("session %s created with version %s instead of %s", identifier, actual, version)
		}
	}

	// Record the session in Git mode with the default version, as earlier
	// versions of Mutagen did.
	manager.Shutdown()
	path, err := pathForSession(gitMode)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	if err := encoding.LoadAndUnmarshalProtobuf(path, session); err != nil {
		t.Fatal("unable to load session:", err)
	}
	session.Version = DefaultVersion
	if err := encoding.MarshalAndSaveProtobuf(path, session); err != nil {
		t.Fatal("unable to save session:", err)
	}

	// Load the sessions again, which records the Git mode version for the
	// session in Git mode and leaves the others alone.
	reloaded, err := NewManager(nil)
	if err != nil {
		t.Fatal("unable to create manager:", err)
	}
	defer reloaded.Shutdown()
	for identifier, version := range expected {
		if actual := reloaded.sessions[identifier].session.Version; actual != version {
			t.Errorf("session %s loaded with version %s instead of %s", identifier, actual, version)
		}
		if actual := recorded(identifier); actual != version {
			t.Errorf("session %s recorded with version %s instead of %s", identifier, actual, version)
		}
	}
}

// TestRestrictedSubmissionVersion verifies that sessions get the restricted
// submission version, which daemons that don't support remote submission
// modes don't load, if and only if their remote submission mode keeps an
// endpoint from submitting commands that it would submit by default, in Git
// mode or not, since those daemons would let it submit them.
func TestRestrictedSubmissionVersion(t *testing.T) {
	const (
		dflt     = RemoteCommandMode_RemoteCommandModeDefault
		disabled = RemoteCommandMode_RemoteCommandModeDisabled
		alpha    = RemoteCommandMode_RemoteCommandModeAlpha
		beta     = RemoteCommandMode_RemoteCommandModeBeta
		both     = RemoteCommandMode_RemoteCommandModeBoth
	)
	for _, test := range []struct {
		execution, submission RemoteCommandMode
		git                   bool
		expected              Version
	}{
		{dflt, dflt, false, DefaultVersion},
		{both, dflt, true, gitModeVersion},
		{both, both, false, DefaultVersion},
		{alpha, beta, false, DefaultVersion},
		{disabled, beta, false, DefaultVersion},
		{disabled, both, true, gitModeVersion},
		{dflt, disabled, false, DefaultVersion},
		{both, alpha, false, restrictedSubmissionVersion},
		{both, beta, true, restrictedSubmissionVersion},
		{alpha, disabled, false, restrictedSubmissionVersion},
		{beta, beta, false, restrictedSubmissionVersion},
	} {
		configuration := &Configuration{RemoteCommandMode: test.execution, RemoteSubmissionMode: test.submission}
		if test.git {
			configuration.GitMode = GitMode_GitModeEnabled
		}
		if version := versionFor(configuration); version != test.expected {
			t.Errorf("execution %v, submission %v, Git mode %t: version %s instead of %s",
				test.execution, test.submission, test.git, version, test.expected)
		}
	}
}
