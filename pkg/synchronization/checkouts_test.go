package synchronization

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/state"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// newCheckoutController creates a controller for testing checkouts, whose
// last synchronization cycle found conflicts at the specified roots.
func newCheckoutController(roots ...string) *controller {
	var conflicts []*core.Conflict
	for _, root := range roots {
		conflicts = append(conflicts, &core.Conflict{Root: root})
	}
	return &controller{
		stateLock: state.NewTrackingLock(state.NewTracker()),
		session:   &Session{Identifier: "sync_checkouts"},
		state:     &State{Conflicts: conflicts, AlphaState: &EndpointState{}, BetaState: &EndpointState{}},
	}
}

func TestCheckoutClaims(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	c := newCheckoutController("", "file", "other")

	// New checkouts are only recorded for conflict roots, and claims require a
	// token.
	if _, err := c.claimCheckout(true, "missing", "a"); !errors.Is(err, errNotConflictRoot) {
		t.Fatal("checkout of a path that isn't a conflict root claimed:", err)
	} else if _, err := c.claimCheckout(true, "file", ""); !errors.Is(err, errMissingToken) {
		t.Fatal("checkout claimed without a token:", err)
	}

	// Each conflict is checked out through a single endpoint: claiming it again
	// through the same endpoint reports that it's held, while the opposite
	// endpoint's claim fails.
	if held, err := c.claimCheckout(true, "file", "a"); err != nil || held != "" {
		t.Fatal("checkout not claimed:", held, err)
	} else if held, err := c.claimCheckout(true, "file", "a"); err != nil || held != "a" {
		t.Fatal("held checkout not reported:", held, err)
	} else if _, err := c.claimCheckout(false, "file", "b"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout claimed through both endpoints:", err)
	} else if held, err := c.claimCheckout(false, "other", "c"); err != nil || held != "" {
		t.Fatal("checkout of another conflict not claimed:", held, err)
	} else if held, err := c.claimCheckout(false, "", "d"); err != nil || held != "" {
		t.Fatal("checkout of the root not claimed:", held, err)
	}

	// Checkouts are kept on disk, sorted by path, so that they survive daemon
	// restarts, and they outlive their conflicts, such as for unfinished
	// resolutions.
	restarted := newCheckoutController()
	expected := []remote.Checkout{{Path: "", Token: "d"}, {Path: "file", Alpha: true, Token: "a"}, {Path: "other", Token: "c"}}
	if checkouts, err := restarted.checkouts(); err != nil || !reflect.DeepEqual(checkouts, expected) {
		t.Fatalf("checkouts not kept: %+v, %v", checkouts, err)
	} else if held, err := restarted.claimCheckout(true, "file", "a"); err != nil || held != "a" {
		t.Fatal("checkout of a conflict that's no longer listed not held:", held, err)
	} else if _, err := restarted.claimCheckout(true, "other", "e"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout of a conflict that's no longer listed claimed by the opposite endpoint:", err)
	}

	// Only the endpoint holding a checkout releases it, by its token, after
	// which the opposite endpoint can claim it. A release without the token
	// only reports it.
	if held, err := c.releaseCheckout(false, "file", "a"); err != nil || held != "" {
		t.Fatal("checkout released by the opposite endpoint:", held, err)
	} else if held, err := c.releaseCheckout(true, "file", ""); err != nil || held != "a" {
		t.Fatal("token of the held checkout not reported:", held, err)
	} else if held, err := c.releaseCheckout(true, "file", "a"); err != nil || held != "a" {
		t.Fatal("checkout not released:", held, err)
	} else if held, err := c.releaseCheckout(true, "file", "a"); err != nil || held != "" {
		t.Fatal("released checkout reported as held:", held, err)
	} else if held, err := c.claimCheckout(false, "file", "f"); err != nil || held != "" {
		t.Fatal("released checkout not claimed by the opposite endpoint:", held, err)
	}

	// Listings include checkouts.
	if listing, err := c.remoteConflicts("sha1"); err != nil || len(listing.Checkouts) != 3 || listing.Checkouts[1] != (remote.Checkout{Path: "file", Token: "f"}) {
		t.Fatalf("checkouts not listed: %+v, %v", listing, err)
	}

	// Checkouts are removed from disk once there are none left, and when the
	// session is terminated.
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	for _, checkout := range []remote.Checkout{{Path: "", Token: "d"}, {Path: "file", Token: "f"}, {Path: "other", Token: "c"}} {
		if _, err := c.releaseCheckout(false, checkout.Path, checkout.Token); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checkouts kept without any checkout:", err)
	} else if _, err := c.claimCheckout(true, "file", "g"); err != nil {
		t.Fatal(err)
	} else if err := c.removeCheckouts(); err != nil {
		t.Fatal("unable to remove checkouts:", err)
	} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checkouts not removed:", err)
	} else if err := c.removeCheckouts(); err != nil {
		t.Fatal("removing checkouts twice failed:", err)
	}
}

func TestCheckoutStaleRequests(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	c := newCheckoutController("file")

	// A claim and a release can arrive after their submitter stopped waiting
	// for them, such as on timeout, and after the conflict was checked out
	// again through the same endpoint. The checkout keeps the token of the
	// claim that recorded it, so the late claim only reports it, and the late
	// release leaves the checkout, which the opposite endpoint thus can't
	// claim, until it's released by its token.
	expected := []remote.Checkout{{Path: "file", Alpha: true, Token: "new"}}
	if held, err := c.claimCheckout(true, "file", "new"); err != nil || held != "" {
		t.Fatal("checkout not claimed:", held, err)
	} else if held, err := c.claimCheckout(true, "file", "old"); err != nil || held != "new" {
		t.Fatal("held checkout not reported to a late claim:", held, err)
	} else if held, err := c.releaseCheckout(true, "file", "old"); err != nil || held != "new" {
		t.Fatal("held checkout not reported to a late release:", held, err)
	} else if checkouts, err := c.checkouts(); err != nil || !reflect.DeepEqual(checkouts, expected) {
		t.Fatalf("checkout changed by a late claim and release: %+v, %v", checkouts, err)
	} else if _, err := c.claimCheckout(false, "file", "opposite"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout claimed by the opposite endpoint after a late claim and release:", err)
	} else if held, err := c.releaseCheckout(true, "file", "new"); err != nil || held != "new" {
		t.Fatal("checkout not released by its token:", held, err)
	}

	// A claim that arrives after the checkout was released records a checkout
	// without a workspace, which a later claim through the same endpoint
	// leaves as it is, until it's released by its token.
	if held, err := c.claimCheckout(true, "file", "old"); err != nil || held != "" {
		t.Fatal("late claim not recorded:", held, err)
	} else if held, err := c.claimCheckout(true, "file", "next"); err != nil || held != "old" {
		t.Fatal("abandoned checkout not reported:", held, err)
	} else if held, err := c.releaseCheckout(true, "file", "old"); err != nil || held != "old" {
		t.Fatal("abandoned checkout not released:", held, err)
	} else if held, err := c.claimCheckout(true, "file", "next"); err != nil || held != "" {
		t.Fatal("released checkout not claimed again:", held, err)
	}
}
