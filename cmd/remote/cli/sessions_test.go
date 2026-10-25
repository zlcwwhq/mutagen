package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

func TestExecutionDirectory(t *testing.T) {
	for _, test := range []struct{ root, relative, expected string }{
		{"", "sub", "unknown"},
		{"/work/tree", "sub/dir", "/work/tree/sub/dir"},
		{"/", "sub", "/sub"},
		{"/work/spaces here", "sub", "/work/spaces here/sub"},
		{"work/tree", "sub", "~/work/tree/sub"},
		{".", "", "~"},
		{"~", "", "~"},
		{"~", "sub", "~/sub"},
		{"~other", "", "~other"},
		{`~\work`, "sub", "~/work/sub"},
		{"~/work", "sub", "~/work/sub"},
		{"~other/work", "sub", "~other/work/sub"},
		{"../work", "sub", "~/../work/sub"},
		{"~/../work", "sub", "~/../work/sub"},
		{`C:\`, "", `C:\`},
		{"C:/", "", "C:/"},
		{`C:\work`, "sub/dir", `C:\work\sub\dir`},
		{"C:/work", "sub/dir", "C:/work/sub/dir"},
		{`\\server\share\work`, "sub/dir", `\\server\share\work\sub\dir`},
		{"//server/share/work", "sub/dir", "//server/share/work/sub/dir"},
		{`C:\..\work`, "sub", `C:\work\sub`},
		{"C:/base/../../work", "sub", "C:/work/sub"},
		{`C:\work\..\..`, "", `C:\`},
		{`\\server\share`, "", `\\server\share`},
		{`\\server\share\..\work`, "sub", `\\server\share\work\sub`},
		{"//server/share/base/../../work", "sub", "//server/share/work/sub"},
		{`\\server\share\work\..\..\..`, "", `\\server\share\`},
	} {
		if actual := executionDirectory(test.root, test.relative); actual != test.expected {
			t.Errorf("%q + %q: got %q, expected %q", test.root, test.relative, actual, test.expected)
		}
	}
}

func TestPrintSessionsExecutionLocation(t *testing.T) {
	// Registrations are probed locally; listing never opens execution routes.
	data, err := os.MkdirTemp("", "ms-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	root := t.TempDir()
	child := filepath.Join(root, "sub")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []*remote.Endpoint{
		{Session: "sync_current", Name: "same", Root: root, Alpha: true, Submit: true,
			ExecutionHost: "docker-worker", ExecutionRoot: "~/project",
			Ignores: &remote.Ignores{Syntax: ignore.Syntax_SyntaxMutagen, Patterns: []string{"sub"}}},
		{Session: "sync_other", Name: "same", Root: t.TempDir(), Submit: true, ExecutionHost: "workstation", ExecutionRoot: "/local/project"},
		{Session: "sync_old", Name: "old", Root: t.TempDir(), Submit: true},
	} {
		channel, err := remote.NewLocalChannel(nil, endpoint)
		if err != nil {
			t.Fatal(err)
		}
		defer channel.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output strings.Builder
	if err := printSessions(ctx, child, "", &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"sync_current: local root " + root + " (alpha); runs on docker-worker:~/project; cwd ~/project/sub",
		"used by default", "ignores the current directory; cannot run directly",
		"(beta); runs on workstation:/local/project; cwd /local/project",
		"outside local root; select with -s and --cwd",
		"old: local root", "runs on unknown:unknown; cwd unknown",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("missing %q in listing:\n%s", expected, output.String())
		}
	}
	output.Reset()
	if err := printSessions(ctx, t.TempDir(), "sync_other", &output); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(output.String(), "outside local root; uses execution root") ||
		!strings.Contains(output.String(), "outside local root; -s selects execution root") {
		t.Fatal("incorrect outside-root mapping:", output.String())
	}
}
