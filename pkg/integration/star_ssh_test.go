package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/agent"
	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/project"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

// fakeSSH is a replacement for ssh (see MUTAGEN_SSH_PATH) that runs the
// requested command in the fake remote home directory on this system, as if
// on another system: without the daemon's data directory or SSH path. The
// host is ignored, so every SSH node shares the fake remote system.
const fakeSSH = `#!/bin/sh
for command; do :; done
unset MUTAGEN_DATA_DIRECTORY MUTAGEN_SSH_PATH
cd "$FAKE_REMOTE_HOME" && HOME="$FAKE_REMOTE_HOME" exec sh -c "$command"
`

// fakeSCP is a replacement for scp (see MUTAGEN_SSH_PATH) that copies the
// source to the destination's path in the fake remote home directory.
const fakeSCP = `#!/bin/sh
source=
for destination; do
	[ -n "$previous" ] && source="$previous"
	previous="$destination"
done
destination="${destination#*:}"
case "$destination" in
/*) ;;
*) destination="$FAKE_REMOTE_HOME/$destination" ;;
esac
exec cp "$source" "$destination"
`

// TestStarFakeSSH runs a star end to end over the SSH transport, using fake
// ssh and scp commands, so that agents are installed on the nodes and the
// agents' remote executable submits commands, as on remote systems. It
// requires the agent bundle of a local build (see BUILDING.md).
func TestStarFakeSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake SSH commands require a POSIX shell")
	}

	// The fake remote system is this one, whose platform the agent installer
	// probes with uname (see pkg/agent/probe.go), so the bundle must contain
	// the agent for the host's platform, which isn't the test's when it's
	// built for another architecture, such as 386 on amd64.
	platform, err := exec.Command("go", "env", "GOHOSTOS", "GOHOSTARCH").Output()
	if err != nil {
		t.Skip("unable to determine the host platform with go env:", err)
	}
	host := strings.Fields(string(platform))
	if len(host) != 2 {
		t.Fatalf("unexpected host platform: %q", platform)
	}
	if path, err := agent.ExecutableForPlatform(host[0], host[1], ""); err != nil {
		t.Skipf("agent bundle has no agent for the host platform %s/%s (perform a local build for it first): %v", host[0], host[1], err)
	} else {
		os.Remove(path)
	}

	// Create the fake remote system and the center's workspace in a short
	// directory, which keeps the registries' socket paths within Unix limits:
	// sun_path holds 104 bytes on macOS, including the terminating NUL. The
	// temporary directory there is long (/var/folders/xx/<28 characters>/T,
	// which resolves to /private/var/...), so use /tmp, which resolves to
	// /private/tmp. The base is then at most 27 bytes long, and the longest
	// socket path, a scope registration of a development agent in the fake
	// home directory, is at most 27 + 54 = 81 bytes long.
	base, err := os.MkdirTemp("/tmp", "mss-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error("unable to remove test directory:", err)
		}
	})
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	tools, home, local, data := filepath.Join(base, "tools"), filepath.Join(base, "home"), filepath.Join(base, "local"), filepath.Join(base, "data")
	for _, directory := range []string{filepath.Join(home, filesystem.MutagenDataDirectoryDevelopmentName), data} {
		for _, registry := range []string{"remote", "remote-scopes"} {
			if socket := filepath.Join(directory, registry, strings.Repeat("0", 16)+".sock"); len(socket) > 103 {
				t.Fatalf("registration socket paths would exceed Unix limits: %s", socket)
			}
		}
	}
	for _, directory := range []string{tools, home, local} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tools, "ssh"), []byte(fakeSSH), 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(tools, "scp"), []byte(fakeSCP), 0700); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(local, "input"), []byte("from the center"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUTAGEN_SSH_PATH", tools)
	t.Setenv("FAKE_REMOTE_HOME", home)
	t.Setenv("MUTAGEN_DATA_DIRECTORY", data)
	t.Chdir(base)

	// Expand and create the star.
	configuration := &project.Configuration{Star: &project.StarConfiguration{
		Remote: true,
		Center: "local",
		Nodes: map[string]project.StarNode{
			"local":  {Endpoint: local},
			"server": {Endpoint: "server:server"},
			"gpu":    {Endpoint: "gpu:gpu"},
		},
	}}
	specifications, err := project.NewSessionSpecifications(configuration, nil, "star-ssh", false)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := synchronization.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Terminate(context.Background(), &selection.Selection{All: true}, "")
		manager.Shutdown()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, s := range specifications.Synchronization {
		if _, err := manager.Create(ctx, s.Alpha, s.Beta, s.Configuration, s.ConfigurationAlpha, s.ConfigurationBeta, s.Name, s.Labels, s.Star, false, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for the center to reach both nodes.
	for {
		connection, _, err := (&remote.Selection{Workspace: true}).Submit(ctx, local, &remote.Submission{Operation: remote.OperationNodes})
		var graph *remote.NodeGraph
		if err == nil {
			graph, err = remote.ListNodes(ctx, connection)
		}
		if err == nil && len(graph.Nodes) == 3 && !slices.ContainsFunc(graph.Nodes, func(node remote.Node) bool { return !node.Available }) {
			break
		} else if ctx.Err() != nil {
			t.Fatal("star didn't connect:", err, graph)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Run a command from the center on an SSH node.
	code, output, diagnostic := runRemote(t, local, "--node", "server", "sh", "-c", `cat input; printf ':%s:' "$MUTAGEN_REMOTE_NODE"; pwd -P`)
	if code != 0 || output != "from the center:server:"+filepath.Join(home, "server")+"\n" {
		t.Fatalf("command on SSH node failed: %d %q %s", code, output, diagnostic)
	}

	// Run the agent's remote executable on the SSH node, as on its system.
	matches, err := filepath.Glob(filepath.Join(home, ".mutagen*", "agents", "*", "remote"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("agent's remote executable not installed: %v %v", matches, err)
	}
	nodeRemote := func(arguments ...string) (int, string, string) {
		t.Helper()
		command := exec.CommandContext(ctx, matches[0], arguments...)
		command.Dir = filepath.Join(home, "server")
		command.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		var output, errorOutput bytes.Buffer
		command.Stdout, command.Stderr = &output, &errorOutput
		err := command.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), output.String(), errorOutput.String()
		} else if err != nil {
			t.Fatal("unable to run the node's remote executable:", err)
		}
		return 0, output.String(), errorOutput.String()
	}
	code, output, diagnostic = nodeRemote("--nodes", "--json")
	var listing starListing
	if code != 0 || json.Unmarshal([]byte(output), &listing) != nil {
		t.Fatalf("node listing failed on SSH node: %d %q %s", code, output, diagnostic)
	} else if listing.Current != "server" || !listing.Star || len(listing.Nodes) != 2 || !listing.Nodes[0].Center || listing.Nodes[0].Name != "local" ||
		!listing.Nodes[0].Available || listing.Nodes[0].Root != local {
		t.Fatalf("incorrect listing on SSH node: %s", output)
	}
	if err := os.WriteFile(filepath.Join(home, "server", "result"), []byte("from the node"), 0600); err != nil {
		t.Fatal(err)
	}
	code, output, diagnostic = nodeRemote("sh", "-c", `cat result; printf ':%s:' "$MUTAGEN_REMOTE_NODE"; pwd -P`)
	if code != 0 || output != fmt.Sprintf("from the node:local:%s\n", local) {
		t.Fatalf("command on the center from SSH node failed: %d %q %s", code, output, diagnostic)
	}
	code, _, diagnostic = nodeRemote("--node", "gpu", "true")
	if code != 125 || !strings.Contains(diagnostic, "no multi-hop") || !strings.Contains(diagnostic, "center node local") {
		t.Fatalf("multi-hop command not refused on SSH node: %d %s", code, diagnostic)
	}
}
