package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestParseArgumentBoundary(t *testing.T) {
	options, err := parse([]string{"-e", "VALUE=a,b", "--timeout", "2m", "program", "--help", "--env", "unchanged", "", "two words"})
	if err != nil {
		t.Fatal(err)
	} else if !reflect.DeepEqual(options.arguments, []string{"program", "--help", "--env", "unchanged", "", "two words"}) {
		t.Fatalf("arguments changed: %#v", options.arguments)
	} else if !reflect.DeepEqual(options.environment, []string{"VALUE=a,b"}) || options.timeout != 2*time.Minute || options.help {
		t.Fatalf("options parsed incorrectly: %+v", options)
	}
	for _, arguments := range [][]string{nil, {"-e", "X=1"}, {"--timeout", "-1s", "true"}, {"--unknown", "true"}} {
		if _, err := parse(arguments); err == nil {
			t.Fatalf("invalid arguments accepted: %#v", arguments)
		}
	}
	if options, err := parse([]string{"--help"}); err != nil || !options.help {
		t.Fatal("help not recognized:", err)
	}
}

func TestParseTimeout(t *testing.T) {
	// Numbers without a unit are seconds, as with timeout.
	for value, expected := range map[string]time.Duration{
		"600":   10 * time.Minute,
		"1.5":   1500 * time.Millisecond,
		"0":     0,
		"10m":   10 * time.Minute,
		"1h30m": 90 * time.Minute,
		"250ms": 250 * time.Millisecond,
	} {
		if options, err := parse([]string{"--timeout", value, "true"}); err != nil {
			t.Errorf("--timeout %s rejected: %v", value, err)
		} else if options.timeout != expected {
			t.Errorf("--timeout %s parsed as %v instead of %v", value, options.timeout, expected)
		}
	}
	for _, value := range []string{"", "-1", "-1s", "1.2.3", ".", "10x", "1e3", "inf", "s"} {
		if _, err := parse([]string{"--timeout", value, "true"}); err == nil {
			t.Errorf("invalid --timeout %q accepted", value)
		}
	}
}

func TestParseSessions(t *testing.T) {
	if options, err := parse([]string{"--sessions"}); err != nil || !options.sessions {
		t.Fatal("--sessions rejected:", err)
	}
	for _, arguments := range [][]string{{"--sessions", "true"}, {"--sessions", "-s", "name"}, {"--sessions", "--flush"}, {"--sessions", "--conflicts"}} {
		if _, err := parse(arguments); err == nil {
			t.Errorf("invalid arguments accepted: %v", arguments)
		}
	}
}

func TestWarnOverlapping(t *testing.T) {
	var output strings.Builder
	warnOverlapping(&output, &remote.Target{Session: "sync_inner", Name: "inner"})
	if output.Len() != 0 {
		t.Fatal("warned without overlapping sessions:", output.String())
	}
	for _, test := range []struct {
		overlapping []string
		expected    string
	}{
		{[]string{"gpu1", "gpu2"}, "remote: warning: sessions gpu1 and gpu2 both synchronize the current directory; using gpu1 (select one with -s)\n"},
		{[]string{"gpu1", "sync_x", "gpu3"}, "remote: warning: sessions gpu1, sync_x, and gpu3 all synchronize the current directory; using gpu1 (select one with -s)\n"},
	} {
		output.Reset()
		warnOverlapping(&output, &remote.Target{Overlapping: test.overlapping})
		if output.String() != test.expected {
			t.Errorf("incorrect warning: %q", output.String())
		}
	}
}

func TestParseNoPrologue(t *testing.T) {
	if options, err := parse([]string{"true"}); err != nil || options.noPrologue {
		t.Fatal("prologue disabled by default:", err)
	} else if options, err := parse([]string{"--no-prologue", "true", "--no-prologue"}); err != nil || !options.noPrologue ||
		!reflect.DeepEqual(options.arguments, []string{"true", "--no-prologue"}) {
		t.Fatalf("--no-prologue parsed incorrectly: %+v, %v", options, err)
	}
}

func TestParseFlush(t *testing.T) {
	if options, err := parse([]string{"true"}); err != nil || options.flush || options.noFlush {
		t.Fatal("flush options set by default:", err)
	} else if options, err := parse([]string{"--no-flush", "true"}); err != nil || !options.noFlush {
		t.Fatal("--no-flush not recognized:", err)
	} else if options, err := parse([]string{"--flush"}); err != nil || !options.flush || len(options.arguments) != 0 {
		t.Fatal("--flush without a command rejected:", err)
	} else if options, err := parse([]string{"--flush", "true"}); err != nil || !options.flush || len(options.arguments) != 1 {
		t.Fatal("--flush with a command rejected:", err)
	} else if _, err := parse([]string{"--flush", "--no-flush"}); err == nil {
		t.Fatal("conflicting flush options accepted")
	} else if _, err := parse([]string{"--no-flush"}); err == nil {
		t.Fatal("--no-flush without a command accepted")
	}
}

func TestParseDiff(t *testing.T) {
	if options, err := parse([]string{"--diff"}); err != nil || !options.diff || len(options.arguments) != 0 {
		t.Fatal("--diff without paths rejected:", err)
	} else if options, err := parse([]string{"--diff", "a", "--flush"}); err != nil || !reflect.DeepEqual(options.arguments, []string{"a", "--flush"}) {
		t.Fatal("--diff paths parsed incorrectly:", options, err)
	}
	for _, arguments := range [][]string{{"--diff", "--flush"}, {"--diff", "--no-flush", "a"}} {
		if _, err := parse(arguments); err == nil {
			t.Fatalf("conflicting options accepted: %v", arguments)
		}
	}
}

func TestHelp(t *testing.T) {
	var output strings.Builder
	if code := Run([]string{"--help"}, strings.NewReader(""), &output, io.Discard); code != 0 {
		t.Fatal("help failed:", code)
	}
	help := output.String()
	for _, expected := range []string{"Usage: remote", "Exit status", "--env NAME[=VALUE]", "--cwd DIR", "--timeout DURATION", "--no-flush", "--flush", "--diff", "--script FILE", "--detach", "--log FILE", "--wait ID", "--kill ID", "--jobs", "--ignores", "--pull"} {
		if !strings.Contains(help, expected) {
			t.Errorf("help is missing %q", expected)
		}
	}
	// Conflict options are only described by their help topic, and the zero
	// timeout isn't shown as a default.
	for _, unexpected := range []string{"stringArray", "duration ", "--take", "default"} {
		if strings.Contains(help, unexpected) {
			t.Errorf("help contains %q", unexpected)
		}
	}

	// Every listed topic has its own help.
	expectations := map[string][]string{
		"sessions":    {"MUTAGEN_REMOTE_SESSION"},
		"termination": {fmt.Sprintf("SIGKILL %d seconds later", remote.TerminationGracePeriod/time.Second)},
		"jobs":        {fmt.Sprintf("SIGKILL %d seconds later", remote.TerminationGracePeriod/time.Second), "--log FILE"},
		"scripts":     {fmt.Sprintf("limited to %d KiB", remote.MaximumScriptSize/1024), "or with " + remote.DefaultScriptInterpreter + " if it has none"},
		"conflicts":   {"--conflict-checkout", "--take SIDE", "--base FILE", "--json"},
	}
	for _, topic := range helpTopics {
		if !strings.Contains(help, "\n  "+topic.name+" ") {
			t.Errorf("help doesn't list topic %q", topic.name)
		}
		output.Reset()
		if code := Run([]string{"--help", topic.name}, strings.NewReader(""), &output, io.Discard); code != 0 {
			t.Errorf("help for topic %q failed: %d", topic.name, code)
		}
		for _, expected := range expectations[topic.name] {
			if !strings.Contains(output.String(), expected) {
				t.Errorf("help for topic %q is missing %q", topic.name, expected)
			}
		}
	}
	if code := Run([]string{"--help", "unknown"}, strings.NewReader(""), io.Discard, io.Discard); code != failureExitCode {
		t.Error("unknown help topic accepted:", code)
	}
}

func TestSessionEnvironmentDefault(t *testing.T) {
	// The environment variable is only a default, which can't redirect
	// submissions from inside another session's root, so it's kept separate
	// from an explicit selection, which takes precedence.
	t.Setenv("MUTAGEN_REMOTE_SESSION", "from-environment")
	if options, err := parse([]string{"true"}); err != nil || options.session != "" || options.defaultSession != "from-environment" {
		t.Fatalf("session environment variable parsed incorrectly: %+v, %v", options, err)
	} else if options, err := parse([]string{"-s", "explicit", "true"}); err != nil || options.session != "explicit" || options.defaultSession != "" {
		t.Fatalf("explicit session did not take precedence: %+v, %v", options, err)
	}
}

func TestRunFailures(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	var output, errorOutput strings.Builder
	if code := Run([]string{"--version"}, strings.NewReader(""), &output, &errorOutput); code != 0 || output.Len() == 0 {
		t.Fatal("version failed:", code)
	} else if code := Run(nil, strings.NewReader(""), &output, &errorOutput); code != failureExitCode {
		t.Fatal("missing command not reported:", code)
	}
	for _, arguments := range [][]string{{"true"}, {"--no-flush", "true"}, {"--flush"}, {"--diff"}} {
		errorOutput.Reset()
		if code := Run(arguments, strings.NewReader(""), &output, &errorOutput); code != failureExitCode ||
			strings.Count(errorOutput.String(), "no connected session") != 1 || strings.Contains(errorOutput.String(), "remote: warning:") {
			t.Fatalf("missing session not reported by %v: %d, %q", arguments, code, errorOutput.String())
		}
	}
}

func TestLoadEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environment")
	if err := os.WriteFile(path, []byte("# comment\n\nVALUE=from file\r\nEMPTY=\nLITERAL=$HOME 'quoted' = value  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_FORWARDED", "from local")
	actual, err := loadEnvironment([]string{path}, []string{"VALUE=override", "REMOTE_FORWARDED"})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"VALUE=from file", "EMPTY=", "LITERAL=$HOME 'quoted' = value  ", "VALUE=override", "REMOTE_FORWARDED=from local"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("incorrect environment: %#v", actual)
	}
	if _, err := loadEnvironment(nil, []string{"MUTAGEN_MISSING_REMOTE_TEST_VARIABLE"}); err == nil {
		t.Fatal("missing local variable accepted")
	}
	if err := os.WriteFile(path, []byte("SECRET-VALUE-WITHOUT-EQUALS"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnvironment([]string{path}, nil); err == nil {
		t.Fatal("invalid file accepted")
	}
}

func TestParseModes(t *testing.T) {
	// Each mode accepts its own options and arguments.
	for _, arguments := range [][]string{
		{"--jobs"},
		{"-s", "name", "--jobs"},
		{"--wait", "0123abcd"},
		{"--wait", "0123abcd", "--timeout", "10", "--no-flush", "-s", "name"},
		{"--kill", "0123abcd", "--timeout", "10"},
		{"--kill", "0123abcd", "--no-flush"},
		{"--no-flush", "--kill", "0123abcd", "--timeout", "10", "-s", "name"},
		{"--ignores"},
		{"--ignores", "results", "a.log"},
		{"--pull", "results", "a.log"},
		{"-s", "name", "--timeout", "1m", "--pull", "results"},
		{"--detach", "python", "train.py"},
		{"--detach", "--log", "train.log", "-C", "sub", "-e", "X=1", "--no-flush", "--no-prologue", "python"},
		{"--script", "-"},
		{"--script", "sweep.sh", "a", "--help"},
		{"--script", "-", "--detach", "--log", "sweep.log", "--timeout", "0"},
		{"--script", "-", "--timeout", "10m", "-C", "/tmp", "--flush"},
	} {
		if _, err := parse(arguments); err != nil {
			t.Errorf("valid arguments %v rejected: %v", arguments, err)
		}
	}
	// The arguments that follow a script are its own, even if they look like
	// options.
	for _, test := range []struct {
		arguments, expected []string
	}{
		{[]string{"--script", "sweep.sh", "--help", "x"}, []string{"--help", "x"}},
		{[]string{"--script=sweep.sh", "--lr", "3e-4"}, []string{"--lr", "3e-4"}},
		{[]string{"-e", "--script", "--script", "-", "-e", "X"}, []string{"-e", "X"}},
		{[]string{"-sname", "-C", "sub", "--no-flush", "--script", "-"}, nil},
		{[]string{"--script", "-", "--", "x"}, []string{"--", "x"}},
	} {
		options, err := parse(test.arguments)
		if err == nil && options.script == "" {
			err = errors.New("no script")
		}
		if err != nil || options.help || len(options.arguments)+len(test.expected) > 0 && !reflect.DeepEqual(options.arguments, test.expected) {
			t.Errorf("script arguments %v parsed incorrectly: %#v, %v", test.arguments, options, err)
		}
	}
	if options, err := parse([]string{"-e", "--script", "true"}); err != nil || options.script != "" || !reflect.DeepEqual(options.arguments, []string{"true"}) {
		t.Errorf("option value mistaken for --script: %+v, %v", options, err)
	}

	// Modes are mutually exclusive, and accept only some options.
	for _, arguments := range [][]string{
		{"--jobs", "x"},
		{"--jobs", "--wait", "0123abcd"},
		{"--jobs", "--flush"},
		{"--jobs", "--conflicts"},
		{"--wait"},
		{"--wait", ""},
		{"--wait", "0123abcd", "x"},
		{"--wait", "0123abcd", "-C", "sub"},
		{"--kill", "0123abcd", "--flush"},
		{"--kill", "0123abcd", "--no-flush", "--flush"},
		{"--kill", "0123abcd", "--no-flush", "echo"},
		{"--kill", "0123abcd", "--wait", "0123abcd"},
		{"--ignores", "--timeout", "10"},
		{"--ignores", "--diff"},
		{"--ignores", "a", "-b"},
		{"--pull"},
		{"--pull", "--flush", "a"},
		{"--pull", "-e", "X=1", "a"},
		{"--pull", "a", "-b"},
		{"--sessions", "--jobs"},
		{"--log", "train.log", "python"},
		{"--detach"},
		{"--detach", "--timeout", "10", "python"},
		{"--detach", "--diff"},
		{"--script", "", "x"},
		{"--diff", "--script", "-"},
		{"--conflicts", "--script", "-"},
		{"--detach", "--conflict-discard", "a"},
		{"--jobs", "--script", "-"},
	} {
		if _, err := parse(arguments); err == nil {
			t.Errorf("invalid arguments %v accepted", arguments)
		}
	}
}
