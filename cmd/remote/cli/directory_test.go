package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestPrintCommandDirectoryError(t *testing.T) {
	home := t.TempDir()
	variable := "HOME"
	if runtime.GOOS == "windows" {
		variable = "USERPROFILE"
	}
	t.Setenv(variable, home)
	for _, test := range []struct {
		name, directory, hint string
		kind                  remote.DirectoryErrorKind
	}{
		{"home", home, "-C '~'", remote.DirectoryErrorMissing},
		{"child", filepath.Join(home, "dev", "x"), "-C '~/dev/x'", remote.DirectoryErrorMissing},
		{"spaces", filepath.Join(home, "my files"), "-C '~/my files'", remote.DirectoryErrorMissing},
		{"quotes", filepath.Join(home, "it's here"), "-C '~/it'\"'\"'s here'", remote.DirectoryErrorMissing},
		{"outside", filepath.Join(home, "..", "elsewhere"), "", remote.DirectoryErrorMissing},
		{"prefix", home + "-other", "", remote.DirectoryErrorMissing},
		{"quoted-home", "~", "", remote.DirectoryErrorMissing},
		{"quoted-child", "~/dev/x", "", remote.DirectoryErrorMissing},
		{"relative", "dev/x", "", remote.DirectoryErrorMissing},
		{"mapped", "", "", remote.DirectoryErrorMissing},
		{"inaccessible", home, "", remote.DirectoryErrorInaccessible},
		{"not-directory", home, "", remote.DirectoryErrorNotDirectory},
		{"invalid", "~mutagen-no-such-user", "", remote.DirectoryErrorInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			failure := &remote.DirectoryError{Kind: test.kind, Path: test.directory, Message: "original executor cause"}
			printCommandError(&output, fmt.Errorf("wrapped: %w", failure), test.directory)
			actual := output.String()
			if !strings.HasPrefix(actual, "remote: wrapped: original executor cause\n") {
				t.Fatal("executor error was changed:", actual)
			}
			if test.hint == "" {
				if strings.Contains(actual, "quote ~") {
					t.Fatal("unexpected home suggestion:", actual)
				}
			} else if !strings.Contains(actual, "If you meant the remote home directory, quote ~: "+test.hint+".") {
				t.Fatal("missing conditional home suggestion:", actual)
			}
		})
	}
	var output strings.Builder
	printCommandError(&output, errors.New("unable to access working directory: no such file"), home)
	if strings.Contains(output.String(), "quote ~") {
		t.Fatal("unstructured errors must not trigger suggestions")
	}
}
