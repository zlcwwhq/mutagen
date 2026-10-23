package core

import (
	"testing"
)

func TestSymbolicLinkWindowsBackslashConversionValid(t *testing.T) {
	if target, err := normalizeSymbolicLinkAndEnsurePortable("file", "subdirectory\\other"); err != nil {
		t.Fatal("portable symbolic link treated as invalid:", err)
	} else if target != "subdirectory/other" {
		t.Error("portable symbolic link target incorrect:", target, "!=", "subdirectory/other")
	}
}

func TestSymbolicLinkWindowsBackslashConversionEscapeInvalid(t *testing.T) {
	if _, err := normalizeSymbolicLinkAndEnsurePortable("file", "child\\\\..\\..\\outside"); err == nil {
		t.Fatal("symbolic link that escapes root through repeated backslashes treated as portable")
	}
}

func TestSymbolicLinkWindowsBackslashConversionRepeatedValid(t *testing.T) {
	if target, err := normalizeSymbolicLinkAndEnsurePortable("file", "child\\\\..\\inside"); err != nil {
		t.Fatal("portable symbolic link treated as invalid:", err)
	} else if target != "child//../inside" {
		t.Error("portable symbolic link target incorrect:", target, "!=", "child//../inside")
	}
}
