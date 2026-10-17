package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestSessionEnvironmentDefault(t *testing.T) {
	t.Setenv("MUTAGEN_REMOTE_SESSION", "from-environment")
	if options, err := parse([]string{"true"}); err != nil || options.session != "from-environment" {
		t.Fatal("session environment variable ignored:", err)
	} else if options, err := parse([]string{"-s", "explicit", "true"}); err != nil || options.session != "explicit" {
		t.Fatal("explicit session did not take precedence:", err)
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
			strings.Count(errorOutput.String(), "no connected session") != 1 || strings.Contains(errorOutput.String(), "warning") {
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
