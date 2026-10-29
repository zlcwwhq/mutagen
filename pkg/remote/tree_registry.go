package remote

import (
	"context"
	"errors"
)

// locateWorkspace selects the source workspace for a workspace selection (see
// Selection.Workspace), given the registrations related to the submission
// directory that are compatible with this version of Mutagen and those that
// aren't. The most specific live workspace containing the directory is used,
// or, if outside is true and no live workspace contains it, the only live
// workspace on this system. Several live registrations are only accepted when
// they identify the same node in the same tree, as the endpoints of incident
// edges do; anything else is reported as ambiguous rather than resolved by
// creation order.
//
// Registrations whose processes no longer listen are pruned and otherwise
// ignored, exactly as if they had been removed cleanly, as the registrations
// of paused or reconnecting sessions are. A more specific workspace that isn't
// connected therefore can't be told apart from one that doesn't exist, and an
// enclosing workspace is used instead. For operations corresponding to the
// directory, the enclosing workspace's ignores, which typically exclude a
// nested workspace's root, then refuse the submission (see
// Selection.Corresponding).
//
// Live registrations by other versions of Mutagen can't be used, but they
// aren't masked either: if one contains the directory at least as specifically
// as the selected workspace, or no compatible workspace is found, the version
// mismatch is reported (see mismatchedVersions), since the submission was
// probably meant for it.
func locateWorkspace(ctx context.Context, compatible, incompatible []*candidate, outside bool) (*liveEndpoint, error) {
	// Determine the live registrations by other versions of Mutagen whose roots
	// contain the directory, most specific first, pruning dead ones, so that a
	// stale record can't block selection.
	others, err := synchronizingSessions(ctx, incompatible)
	if err != nil {
		return nil, err
	}

	// Connect to the most specific live workspaces containing the directory,
	// or, if permitted and none does, to all live workspaces. Dead
	// registrations are pruned as tiers are probed, and the next tier is used
	// if a tier has no live registrations (see locate).
	live, err := locate(ctx, filter(compatible, func(c *candidate) bool {
		return c.contained || outside
	}))
	if err != nil {
		return nil, err
	} else if len(live) == 0 {
		subject := "the workspaces containing this directory are"
		if len(others) == 0 && outside {
			subject = "the workspaces on this system are"
			if others, err = liveSessions(ctx, filter(incompatible, excluding)); err != nil {
				return nil, err
			}
		}
		if len(others) > 0 {
			return nil, mismatchedVersions(subject, others)
		}
		return nil, ConnectionError(errors.New("no connected node contains the current directory; run from a workspace root or its subdirectory"))
	}
	chosen := live[0]

	// Refuse to mask a live workspace registered by another version of Mutagen
	// that contains the directory at least as specifically. Since the other
	// registrations are ordered most specific first, the first one determines
	// whether they're all equally specific.
	var masking []*candidate
	for _, c := range others {
		if c.depth >= chosen.candidate.depth {
			masking = append(masking, c)
		}
	}
	if len(masking) > 0 {
		closeEndpoints(live)
		subject := "a more specific workspace containing this directory is"
		if !chosen.candidate.contained {
			subject = "a workspace containing this directory is"
		} else if masking[0].depth == chosen.candidate.depth {
			subject = "another workspace with the same root is"
		}
		return nil, mismatchedVersions(subject, masking)
	}

	// Several live registrations must identify the same node: incident edges of
	// a tree share the node's root, while distinct pairs or trees sharing a
	// root are ambiguous.
	for _, endpoint := range live[1:] {
		first, other := chosen.candidate, endpoint.candidate
		same := first.Tree != "" && first.Tree == other.Tree && first.Node == other.Node
		if first.Tree == "" && other.Tree == "" {
			same = first.Session == other.Session && first.Alpha == other.Alpha
		}
		if !same {
			closeEndpoints(live)
			return nil, errors.New("ambiguous source node; declare overlapping pair workspaces as one tree, or run from an unambiguous workspace")
		}
	}
	for _, endpoint := range live[1:] {
		endpoint.connection.Close()
	}
	return &chosen, nil
}
