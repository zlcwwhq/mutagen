package synchronization

import (
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mutagen-io/mutagen/pkg/identifier"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/url"
)

// TestValidateTreeMembershipPrologue verifies that sessions created through the
// API can't attach a prologue to the configuration shared by both endpoints of
// a tree edge, where it would run on every node without its own. Node
// prologues belong in the endpoint-specific configurations.
func TestValidateTreeMembershipPrologue(t *testing.T) {
	// edge creates a valid tree edge whose parent has a node prologue.
	edge := func() *Session {
		return &Session{
			Configuration: &Configuration{
				SynchronizationMode: core.SynchronizationMode_SynchronizationModeTwoWaySafe,
				RemoteCommandMode:   RemoteCommandMode_RemoteCommandModeBoth,
			},
			ConfigurationAlpha: &Configuration{RemotePrologue: "parent.sh"},
			ConfigurationBeta:  &Configuration{},
			Labels:             map[string]string{"io.mutagen.project": "project"},
			Tree:               &TreeMembership{Identifier: "project", Root: "a", Alpha: "a", Beta: "b"},
		}
	}
	if err := validateTreeMembership(edge()); err != nil {
		t.Fatal("valid tree session rejected:", err)
	}

	// A shared prologue must be refused for tree edges.
	shared := edge()
	shared.Configuration.RemotePrologue = "shared.sh"
	if err := validateTreeMembership(shared); err == nil {
		t.Fatal("tree session with a shared prologue accepted")
	}

	// Ordinary pairs keep their shared prologues.
	pair := edge()
	pair.Tree = nil
	pair.Configuration.RemotePrologue = "shared.sh"
	if err := validateTreeMembership(pair); err != nil {
		t.Fatal("pair session with a shared prologue rejected:", err)
	}
}

// TestTreeMembershipValidation verifies that malformed membership is refused,
// both on creation and when persisted sessions are loaded.
func TestTreeMembershipValidation(t *testing.T) {
	valid := &TreeMembership{Identifier: "project", Root: "a", Alpha: "a", Beta: "b"}
	if err := valid.EnsureValid(); err != nil {
		t.Fatal("valid membership rejected:", err)
	}
	for _, test := range []struct {
		name       string
		membership *TreeMembership
	}{
		{"nil", nil},
		{"identifier", &TreeMembership{Root: "a", Alpha: "a", Beta: "b"}},
		{"root", &TreeMembership{Identifier: "project", Alpha: "a", Beta: "b"}},
		{"parent", &TreeMembership{Identifier: "project", Root: "a", Beta: "b"}},
		{"child", &TreeMembership{Identifier: "project", Root: "a", Alpha: "a"}},
		{"name", &TreeMembership{Identifier: "project", Root: "a", Alpha: "a", Beta: "-b"}},
		{"self", &TreeMembership{Identifier: "project", Root: "a", Alpha: "b", Beta: "b"}},
		{"root-child", &TreeMembership{Identifier: "project", Root: "a", Alpha: "b", Beta: "a"}},
	} {
		if err := test.membership.EnsureValid(); err == nil {
			t.Errorf("%s: invalid membership accepted", test.name)
		}
	}

	// Loading validates membership along with the rest of the session.
	session := treeTestEdge("a", "b")
	var err error
	if session.Identifier, err = identifier.New(identifier.PrefixSynchronization); err != nil {
		t.Fatal(err)
	}
	session.Version = Version_Version1
	session.CreationTime = timestamppb.Now()
	session.Alpha.Kind, session.Beta.Kind = url.Kind_Synchronization, url.Kind_Synchronization
	if err := session.EnsureValid(); err != nil {
		t.Fatal("valid tree session rejected:", err)
	}
	session.Tree.Alpha = session.Tree.Beta
	if err := session.EnsureValid(); err == nil {
		t.Fatal("tree session with invalid membership accepted")
	}
}

// TestTreeLabelsConferNoMembership verifies that labels resembling the former
// label-based membership don't make a session a tree member, since anyone can
// set labels on a session, while membership authorizes commands between nodes.
func TestTreeLabelsConferNoMembership(t *testing.T) {
	trees := newCommandTrees()
	forged := &controller{session: treeTestEdge("a", "b")}
	forged.session.Tree = nil
	for key, value := range map[string]string{
		"io.mutagen.tree": "project", "io.mutagen.tree.root": "a",
		"io.mutagen.tree.alpha": "a", "io.mutagen.tree.beta": "b",
	} {
		forged.session.Labels[key] = value
	}
	if err := validateTreeMembership(forged.session); err != nil {
		t.Fatal("labeled pair session rejected:", err)
	}
	trees.update(forged, false)
	if trees.cached["project"] != nil {
		t.Fatal("labels conferred tree membership")
	}
}
