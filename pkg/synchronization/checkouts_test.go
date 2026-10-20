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

	// New checkouts are only recorded for conflict roots.
	if _, err := c.claimCheckout(true, "missing"); !errors.Is(err, errNotConflictRoot) {
		t.Fatal("checkout of a path that isn't a conflict root claimed:", err)
	}

	// Each conflict is checked out through a single endpoint: claiming it again
	// through the same endpoint reports that it's held, while the opposite
	// endpoint's claim fails.
	if held, err := c.claimCheckout(true, "file"); err != nil || held {
		t.Fatal("checkout not claimed:", held, err)
	} else if held, err := c.claimCheckout(true, "file"); err != nil || !held {
		t.Fatal("held checkout not reported:", held, err)
	} else if _, err := c.claimCheckout(false, "file"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout claimed through both endpoints:", err)
	} else if held, err := c.claimCheckout(false, "other"); err != nil || held {
		t.Fatal("checkout of another conflict not claimed:", held, err)
	} else if held, err := c.claimCheckout(false, ""); err != nil || held {
		t.Fatal("checkout of the root not claimed:", held, err)
	}

	// Checkouts are kept on disk, sorted by path, so that they survive daemon
	// restarts, and they outlive their conflicts, such as for unfinished
	// resolutions.
	restarted := newCheckoutController()
	expected := []remote.Checkout{{Path: ""}, {Path: "file", Alpha: true}, {Path: "other"}}
	if checkouts, err := restarted.checkouts(); err != nil || !reflect.DeepEqual(checkouts, expected) {
		t.Fatalf("checkouts not kept: %+v, %v", checkouts, err)
	} else if held, err := restarted.claimCheckout(true, "file"); err != nil || !held {
		t.Fatal("checkout of a conflict that's no longer listed not held:", held, err)
	} else if _, err := restarted.claimCheckout(true, "other"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout of a conflict that's no longer listed claimed by the opposite endpoint:", err)
	}

	// Only the endpoint holding a checkout releases it, after which the
	// opposite endpoint can claim it.
	if held, err := c.releaseCheckout(false, "file"); err != nil || held {
		t.Fatal("checkout released by the opposite endpoint:", held, err)
	} else if held, err := c.releaseCheckout(true, "file"); err != nil || !held {
		t.Fatal("checkout not released:", held, err)
	} else if held, err := c.releaseCheckout(true, "file"); err != nil || held {
		t.Fatal("released checkout reported as held:", held, err)
	} else if held, err := c.claimCheckout(false, "file"); err != nil || held {
		t.Fatal("released checkout not claimed by the opposite endpoint:", held, err)
	}

	// Listings include checkouts.
	if listing, err := c.remoteConflicts("sha1"); err != nil || len(listing.Checkouts) != 3 || listing.Checkouts[1] != (remote.Checkout{Path: "file"}) {
		t.Fatalf("checkouts not listed: %+v, %v", listing, err)
	}

	// Checkouts are removed from disk once there are none left, and when the
	// session is terminated.
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "file", "other"} {
		if _, err := c.releaseCheckout(false, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checkouts kept without any checkout:", err)
	} else if _, err := c.claimCheckout(true, "file"); err != nil {
		t.Fatal(err)
	} else if err := c.removeCheckouts(); err != nil {
		t.Fatal("unable to remove checkouts:", err)
	} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checkouts not removed:", err)
	} else if err := c.removeCheckouts(); err != nil {
		t.Fatal("removing checkouts twice failed:", err)
	}
}
