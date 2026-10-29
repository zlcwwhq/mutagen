package cli

import (
	"github.com/mutagen-io/mutagen/pkg/remote"
)

// selection locates the source workspace, while the daemon selects the
// destination, which is the opposite endpoint of a single edge. At a star's
// center, the registration of the requested node's edge is preferred, so that
// the operation doesn't depend on other edges (see remote.Selection).
func (o *options) selection(corresponding bool) *remote.Selection {
	return &remote.Selection{Workspace: true, WorkspaceOutside: o.node != "", Corresponding: corresponding, Node: o.node, Resolve: true, Target: o.target}
}
