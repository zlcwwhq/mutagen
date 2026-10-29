package project

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	svc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// specificationSession describes a planned edge without assigning its eventual
// identifier or creation time, which aren't needed for tree validation.
func specificationSession(specification *svc.CreationSpecification) *synchronization.Session {
	return &synchronization.Session{
		Name:               specification.Name,
		Alpha:              specification.Alpha,
		Beta:               specification.Beta,
		Configuration:      specification.Configuration,
		ConfigurationAlpha: specification.ConfigurationAlpha,
		ConfigurationBeta:  specification.ConfigurationBeta,
		Labels:             specification.Labels,
		Tree:               specification.Tree,
	}
}

// preserveTreeEndpoints makes newly created edges use the exact endpoint URL
// of any retained incident edge naming the same node. Declarative matching
// intentionally ignores captured Docker environment and accepts alternate
// local paths to the same directory. A new edge must not reinterpret either
// as a new execution identity for a node that existing edges still connect to.
// Specifications are cloned when rewritten, so planning does not modify its
// caller's configuration. The complete resulting trees are validated before
// a plan can perform any mutation.
func preserveTreeEndpoints(current []*synchronization.Session, changes []*sessionChange[*synchronization.Session, *svc.CreationSpecification]) error {
	removed := make(map[*synchronization.Session]bool, len(changes))
	for _, change := range changes {
		if change.current != nil {
			removed[change.current] = true
		}
	}
	type nodeIdentity struct{ tree, name string }
	retained := make(map[nodeIdentity]*url.URL)
	owners := make(map[nodeIdentity]string)
	var final []*synchronization.Session
	for _, session := range current {
		if removed[session] {
			continue
		}
		final = append(final, session)
		if session.Tree == nil {
			continue
		}
		for index, name := range []string{session.Tree.Alpha, session.Tree.Beta} {
			key := nodeIdentity{session.Tree.Identifier, name}
			endpoint := []*url.URL{session.Alpha, session.Beta}[index]
			if previous := retained[key]; previous != nil && !proto.Equal(previous, endpoint) {
				return fmt.Errorf("retained tree node %s has inconsistent endpoints in sessions %s and %s; inspect them with mutagen sync list -l and recreate the affected tree edges together after choosing one endpoint definition", name, owners[key], session.Identifier)
			}
			retained[key] = endpoint
			owners[key] = session.Identifier
		}
	}
	for _, change := range changes {
		specification := change.specification
		if specification == nil {
			continue
		} else if specification.Tree != nil {
			// Each endpoint may be anchored by a different retained edge. Clone
			// once, and keep all declarative settings other than the URL intact.
			specification = proto.Clone(specification).(*svc.CreationSpecification)
			for index, name := range []string{specification.Tree.Alpha, specification.Tree.Beta} {
				if endpoint := retained[nodeIdentity{specification.Tree.Identifier, name}]; endpoint != nil {
					if index == 0 {
						specification.Alpha = proto.Clone(endpoint).(*url.URL)
					} else {
						specification.Beta = proto.Clone(endpoint).(*url.URL)
					}
				}
			}
			change.specification = specification
		}
		final = append(final, specificationSession(specification))
	}

	// Validate the actual post-apply sessions rather than only the freshly
	// parsed declarations. Retained transport environments can differ from
	// the invoking shell, which may also affect overlap with ordinary pairs.
	groups := make(map[string][]*synchronization.Session)
	for _, session := range final {
		if session.Tree != nil {
			groups[session.Tree.Identifier] = append(groups[session.Tree.Identifier], session)
		}
	}
	identifiers := make([]string, 0, len(groups))
	for identifier := range groups {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	for _, identifier := range identifiers {
		if err := synchronization.ValidateWorkspaceTreeSessions(groups[identifier]); err != nil {
			return fmt.Errorf("invalid resulting workspace tree: %w", err)
		}
		for _, edge := range groups[identifier] {
			for _, other := range final {
				if other.Tree.GetIdentifier() == identifier {
					continue
				}
				for _, a := range []*url.URL{edge.Alpha, edge.Beta} {
					for _, b := range []*url.URL{other.Alpha, other.Beta} {
						if synchronization.TreeEndpointsOverlap(a, b) {
							return fmt.Errorf("session %s overlaps the resulting workspace tree", other.Name)
						}
					}
				}
			}
		}
	}
	return nil
}
