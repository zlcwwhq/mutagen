package synchronization

import (
	"fmt"
)

// IsDefault indicates whether or not the remote command mode is
// RemoteCommandMode_RemoteCommandModeDefault.
func (m RemoteCommandMode) IsDefault() bool {
	return m == RemoteCommandMode_RemoteCommandModeDefault
}

// MarshalText implements encoding.TextMarshaler.MarshalText.
func (m RemoteCommandMode) MarshalText() ([]byte, error) {
	var result string
	switch m {
	case RemoteCommandMode_RemoteCommandModeDefault:
	case RemoteCommandMode_RemoteCommandModeDisabled:
		result = "false"
	case RemoteCommandMode_RemoteCommandModeAlpha:
		result = "alpha"
	case RemoteCommandMode_RemoteCommandModeBeta:
		result = "beta"
	case RemoteCommandMode_RemoteCommandModeBoth:
		result = "true"
	default:
		result = "unknown"
	}
	return []byte(result), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.UnmarshalText. The values
// "true" and "false" are also accepted when written as unquoted YAML booleans,
// since YAML decoding passes the original scalar text to text unmarshalers.
func (m *RemoteCommandMode) UnmarshalText(textBytes []byte) error {
	// Convert the bytes to a string.
	text := string(textBytes)

	// Convert to a remote command mode.
	switch text {
	case "false":
		*m = RemoteCommandMode_RemoteCommandModeDisabled
	case "alpha":
		*m = RemoteCommandMode_RemoteCommandModeAlpha
	case "beta":
		*m = RemoteCommandMode_RemoteCommandModeBeta
	case "true":
		*m = RemoteCommandMode_RemoteCommandModeBoth
	default:
		return fmt.Errorf("unknown remote command mode specification (expected true, alpha, beta, or false): %s", text)
	}

	// Success.
	return nil
}

// Supported indicates whether or not a particular remote command mode is a
// valid, non-default value.
func (m RemoteCommandMode) Supported() bool {
	switch m {
	case RemoteCommandMode_RemoteCommandModeDisabled:
		return true
	case RemoteCommandMode_RemoteCommandModeAlpha:
		return true
	case RemoteCommandMode_RemoteCommandModeBeta:
		return true
	case RemoteCommandMode_RemoteCommandModeBoth:
		return true
	default:
		return false
	}
}

// Description returns a human-readable description of a remote command mode.
func (m RemoteCommandMode) Description() string {
	switch m {
	case RemoteCommandMode_RemoteCommandModeDefault:
		return "Default"
	case RemoteCommandMode_RemoteCommandModeDisabled:
		return "Disabled"
	case RemoteCommandMode_RemoteCommandModeAlpha:
		return "Executed on Alpha"
	case RemoteCommandMode_RemoteCommandModeBeta:
		return "Executed on Beta"
	case RemoteCommandMode_RemoteCommandModeBoth:
		return "Executed on Alpha and Beta"
	default:
		return "Unknown"
	}
}

// Executes indicates which endpoints execute commands submitted from the
// opposite endpoint. The remote command mode must not be the default mode,
// which should be resolved with Version.DefaultRemoteCommandMode first.
// Unsupported modes execute commands on neither endpoint.
func (m RemoteCommandMode) Executes() (alpha, beta bool) {
	switch m {
	case RemoteCommandMode_RemoteCommandModeAlpha:
		return true, false
	case RemoteCommandMode_RemoteCommandModeBeta:
		return false, true
	case RemoteCommandMode_RemoteCommandModeBoth:
		return true, true
	default:
		return false, false
	}
}
