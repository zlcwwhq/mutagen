package synchronization

import (
	"fmt"
)

// IsDefault indicates whether or not the Git mode is GitMode_GitModeDefault.
func (m GitMode) IsDefault() bool {
	return m == GitMode_GitModeDefault
}

// MarshalText implements encoding.TextMarshaler.MarshalText.
func (m GitMode) MarshalText() ([]byte, error) {
	var result string
	switch m {
	case GitMode_GitModeDefault:
	case GitMode_GitModeDisabled:
		result = "disabled"
	case GitMode_GitModeEnabled:
		result = "enabled"
	default:
		result = "unknown"
	}
	return []byte(result), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.UnmarshalText.
func (m *GitMode) UnmarshalText(textBytes []byte) error {
	// Convert the bytes to a string.
	text := string(textBytes)

	// Convert to a Git mode.
	switch text {
	case "disabled":
		*m = GitMode_GitModeDisabled
	case "enabled":
		*m = GitMode_GitModeEnabled
	default:
		return fmt.Errorf("unknown Git mode specification (expected enabled or disabled): %s", text)
	}

	// Success.
	return nil
}

// Supported indicates whether or not a particular Git mode is a valid,
// non-default value.
func (m GitMode) Supported() bool {
	switch m {
	case GitMode_GitModeDisabled:
		return true
	case GitMode_GitModeEnabled:
		return true
	default:
		return false
	}
}

// Description returns a human-readable description of a Git mode.
func (m GitMode) Description() string {
	switch m {
	case GitMode_GitModeDefault:
		return "Default"
	case GitMode_GitModeDisabled:
		return "Disabled"
	case GitMode_GitModeEnabled:
		return "Enabled"
	default:
		return "Unknown"
	}
}
