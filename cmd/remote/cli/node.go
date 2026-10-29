package cli

import (
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// selection preserves the original source registration while the daemon selects
// the destination, which is the opposite endpoint of a single edge. Internal
// edge identities are never used to choose a source.
func (o *options) selection(corresponding bool) *remote.Selection {
	return &remote.Selection{Workspace: true, WorkspaceOutside: o.node != "", Corresponding: corresponding, Node: o.node, Resolve: true, Target: o.target}
}
