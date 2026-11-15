package synchronization

import (
	"fmt"
)

// Endpoints in Git mode read ignore rules from their own disks, so they can
// disagree about what's ignored: briefly, while a .gitignore is being
// synchronized, or for good, with rules in .git/info/exclude, which is never
// synchronized. Five mechanisms deal with that. Each covers a case that the
// others can't see, so none of them replaces another:
//
//   - A path exists on an endpoint whose rules ignore it: reconciliation
//     propagates nothing to or from that side (see reconciler.reconcile in
//     the core package). This is all that protects existing content under a
//     rule that only one endpoint has.
//   - A path is absent on an endpoint whose rules ignore it, where a scan
//     can't mark it untracked: that endpoint doesn't stage or write it (see
//     gitReceivableFile and gitTransitionPruner in the local endpoint
//     package).
//   - A .gitignore is on its way to an endpoint whose scan predates it: the
//     cycle delivers only rules, and the next one computes everything else
//     again (see ruleOnlyTransitions). Otherwise, the one-way-replica mode
//     would delete what the rule is about to ignore, incoming content would
//     be judged by rules that the endpoint doesn't have yet, and a flush
//     would return before newly tracked content is reconciled. This never
//     applies to exclude files, which aren't delivered.
//   - A rule stops ignoring content on beta in the one-way-replica mode,
//     which mirroring would then remove or overwrite: the session keeps it
//     (see ReplicaRecord).
//   - A root's .git entry is untracked and survives the deletion of
//     everything else, which would keep the session from halting when a
//     root is emptied (see rootEmptied).

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
