package cli

import (
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// selection locates the source workspace, while the daemon selects the
// destination, which is the opposite endpoint of the edge through which the
// operation is submitted. At a star's center, the registration of the
// requested node's edge is used, so that the operation doesn't depend on
// other edges (see remote.Selection).
func (o *options) selection(corresponding bool) *remote.Selection {
	return &remote.Selection{Workspace: true, WorkspaceOutside: o.node != "", Corresponding: corresponding, Node: o.node, Resolve: true, Target: o.target}
}

// followUpCommand returns the start of the remote command lines that remote
// suggests for later operations on the destination selected by the specified
// node, such as waiting for a detached job, so that they can be run as
// printed: remote --node NAME if the destination was selected with --node,
// since omitting it fails at a star's center with several other nodes, and
// remote otherwise, since the daemon's default resolution then selects the
// same destination again. Node names consist of letters, digits, and dashes,
// which need no quoting, but names that the daemon hasn't accepted, as when
// options are refused, are quoted for a POSIX shell and kept on one line (see
// neutralizeLine). The deprecated session selectors are never repeated, since
// they're refused rather than selecting a destination.
func followUpCommand(node string) string {
	if node == "" {
		return "remote"
	}
	return "remote --node " + neutralizeLine(shellQuote(node))
}
