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

// SubmissionDescription returns a human-readable description of a remote
// command mode that specifies which endpoints submit commands (see
// Configuration.RemoteSubmissionMode).
func (m RemoteCommandMode) SubmissionDescription() string {
	switch m {
	case RemoteCommandMode_RemoteCommandModeDefault:
		return "Default"
	case RemoteCommandMode_RemoteCommandModeDisabled:
		return "Disabled"
	case RemoteCommandMode_RemoteCommandModeAlpha:
		return "Submitted from Alpha"
	case RemoteCommandMode_RemoteCommandModeBeta:
		return "Submitted from Beta"
	case RemoteCommandMode_RemoteCommandModeBoth:
		return "Submitted from Alpha and Beta"
	default:
		return "Unknown"
	}
}

// DefaultRemoteSubmissionMode returns the remote command mode that names the
// endpoints that submit commands by default (see
// Configuration.RemoteSubmissionMode) under the specified remote command mode,
// which must not be the default mode: those whose opposite endpoints execute
// commands.
func DefaultRemoteSubmissionMode(execution RemoteCommandMode) RemoteCommandMode {
	alpha, beta := execution.Executes()
	return RemoteCommandModeFor(beta, alpha)
}

// RemoteCommandModeFor returns the remote command mode that names the specified
// endpoints.
func RemoteCommandModeFor(alpha, beta bool) RemoteCommandMode {
	switch {
	case alpha && beta:
		return RemoteCommandMode_RemoteCommandModeBoth
	case alpha:
		return RemoteCommandMode_RemoteCommandModeAlpha
	case beta:
		return RemoteCommandMode_RemoteCommandModeBeta
	default:
		return RemoteCommandMode_RemoteCommandModeDisabled
	}
}

// Executes indicates which endpoints execute commands submitted from the
// opposite endpoint. The remote command mode must not be the default mode,
// which should be resolved with Version.DefaultRemoteCommandMode first.
// Unsupported modes execute commands on neither endpoint. Since the mode names
// endpoints, this also indicates which endpoints a remote submission mode
// names (see Configuration.RemoteSubmissionMode).
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
