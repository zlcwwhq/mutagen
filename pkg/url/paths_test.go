package url

import (
	"testing"
)

func TestIsWindowsPathLowercase(t *testing.T) {
	if !IsWindowsDrivePath(`c:\something`) {
		t.Error("Windows path not classified as such")
	}
}

func TestIsWindowsPathLowercaseForwardSlash(t *testing.T) {
	if !IsWindowsDrivePath(`c:/something`) {
		t.Error("Windows path not classified as such")
	}
}

func TestIsWindowsPath(t *testing.T) {
	if !IsWindowsDrivePath(`C:\something`) {
		t.Error("Windows path not classified as such")
	}
}

func TestIsWindowsPathForwardSlash(t *testing.T) {
	if !IsWindowsDrivePath(`C:/something`) {
		t.Error("Windows path not classified as such")
	}
}

func TestIsWindowsPathLengthTwoDrive(t *testing.T) {
	if IsWindowsDrivePath(`CD:\something`) {
		t.Error("non-Windows path classified as such")
	}
}

func TestWindowsPathSyntax(t *testing.T) {
	for _, test := range []struct {
		path, volume string
		drive, unc   bool
	}{
		{`C:\work`, "C:", true, false},
		{"c:/work", "c:", true, false},
		{"C:work", "", false, false},
		{`CD:\work`, "", false, false},
		{`\\server\share\work`, `\\server\share`, false, true},
		{"//server/share/work", "//server/share", false, true},
		{`\\server\share`, `\\server\share`, false, true},
		{`\\server`, `\\server`, false, true},
		{"/work", "", false, false},
		{`work\sub`, "", false, false},
		{"", "", false, false},
	} {
		if actual := test.path[:WindowsVolumeLength(test.path)]; actual != test.volume {
			t.Errorf("volume of %q: %q, expected %q", test.path, actual, test.volume)
		}
		if IsWindowsDrivePath(test.path) != test.drive || IsWindowsUNCPath(test.path) != test.unc {
			t.Errorf("incorrect Windows syntax classification for %q", test.path)
		}
	}
}
