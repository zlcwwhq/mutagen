// Command releaseversion validates fork release tags and stamps the version used
// by the CLI, agents, and release archive builder in the CI working tree.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/mutagen"
)

var forkVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// versionTagPattern matches the VersionTag declaration, including an optional
// carriage return, since Windows checkouts use CRLF line endings.
var versionTagPattern = regexp.MustCompile(`(?m)^\tVersionTag = "[^"]*"(\r?)$`)

// releaseSuffix checks the upstream version and returns the complete version
// suffix to compile into all release binaries.
func releaseSuffix(tag, base, upstreamTag string) (string, error) {
	// Development snapshots use the upstream numeric version. Preserve actual
	// upstream prerelease identifiers, such as beta1 and rc1.
	if upstreamTag != "" && upstreamTag != "dev" {
		base += "-" + upstreamTag
	}
	prefix := "v" + base + "-zlcwwhq."
	if !strings.HasPrefix(tag, prefix) || !forkVersionPattern.MatchString(strings.TrimPrefix(tag, prefix)) {
		return "", fmt.Errorf("release tag must match %sX.Y.Z (no leading zeroes)", prefix)
	}
	return strings.TrimPrefix(strings.TrimPrefix(tag, "v"), strings.Split(base, "-")[0]+"-"), nil
}

// stampVersion replaces the VersionTag declaration in content with suffix,
// preserving the declaration's line ending.
func stampVersion(content []byte, suffix string) ([]byte, error) {
	if len(versionTagPattern.FindAll(content, -1)) != 1 {
		return nil, fmt.Errorf("expected exactly one VersionTag declaration")
	}
	return versionTagPattern.ReplaceAll(content, []byte("\tVersionTag = \""+suffix+"\"${1}")), nil
}

func main() {
	tag := flag.String("tag", "", "release tag to validate")
	write := flag.Bool("write", false, "stamp pkg/mutagen/version.go")
	flag.Parse()
	base := fmt.Sprintf("%d.%d.%d", mutagen.VersionMajor, mutagen.VersionMinor, mutagen.VersionPatch)
	suffix, err := releaseSuffix(*tag, base, mutagen.VersionTag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *write {
		const path = "pkg/mutagen/version.go"
		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		content, err = stampVersion(content, suffix)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Println(strings.TrimPrefix(*tag, "v"))
}
