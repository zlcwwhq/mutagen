package cli

import "testing"

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
