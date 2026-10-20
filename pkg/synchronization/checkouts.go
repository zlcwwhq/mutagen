package synchronization

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/remote"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// errCheckedOutOpposite indicates that a conflict is checked out through the
// opposite endpoint of the one claiming its checkout.
var errCheckedOutOpposite = errors.New("is checked out through the opposite endpoint, which alone can apply or discard it")

// errNotConflictRoot indicates that a checkout was claimed for a path that's
// neither checked out nor a conflict root.
var errNotConflictRoot = errors.New("isn't a conflict root (see remote --conflicts)")

// compareCheckout orders checkouts by path.
func compareCheckout(checkout remote.Checkout, path string) int {
	return strings.Compare(checkout.Path, path)
}

// loadCheckouts loads the session's checkouts (see remote.Checkout), sorted by
// path. It must be called with checkoutsLock held.
func (c *controller) loadCheckouts() ([]remote.Checkout, error) {
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("unable to read conflict checkouts: %w", err)
	}
	var checkouts []remote.Checkout
	if err := json.Unmarshal(data, &checkouts); err != nil {
		return nil, fmt.Errorf("invalid conflict checkouts: %w", err)
	}
	slices.SortFunc(checkouts, func(a, b remote.Checkout) int {
		return compareCheckout(a, b.Path)
	})
	return checkouts, nil
}

// saveCheckouts saves the session's checkouts, removing them from disk if there
// are none. It must be called with checkoutsLock held.
func (c *controller) saveCheckouts(checkouts []remote.Checkout) error {
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		return err
	} else if len(checkouts) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unable to remove conflict checkouts: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(checkouts)
	if err != nil {
		return fmt.Errorf("unable to encode conflict checkouts: %w", err)
	} else if err := filesystem.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("unable to save conflict checkouts: %w", err)
	}
	return nil
}

// checkouts returns the session's checkouts, sorted by path.
func (c *controller) checkouts() ([]remote.Checkout, error) {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	return c.loadCheckouts()
}

// claimCheckout records a checkout of a conflict root through an endpoint and
// returns whether the endpoint already held it. Each conflict is checked out
// through a single endpoint at a time, which alone can apply or discard its
// resolution, so the claim fails if the opposite endpoint holds the checkout.
// New checkouts are only recorded for conflict roots of the last
// synchronization cycle, while existing ones may outlive their conflicts, such
// as for unfinished resolutions. Checkouts are kept until released, including
// across daemon restarts, since workspaces outlive both.
func (c *controller) claimCheckout(alpha bool, path string) (bool, error) {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	checkouts, err := c.loadCheckouts()
	if err != nil {
		return false, err
	}
	index, found := slices.BinarySearchFunc(checkouts, path, compareCheckout)
	if found && checkouts[index].Alpha == alpha {
		return true, nil
	} else if found {
		return false, errCheckedOutOpposite
	}
	c.stateLock.Lock()
	listed := slices.ContainsFunc(c.state.Conflicts, func(conflict *core.Conflict) bool {
		return conflict.Root == path
	})
	c.stateLock.UnlockWithoutNotify()
	if !listed {
		return false, errNotConflictRoot
	}
	checkouts = slices.Insert(checkouts, index, remote.Checkout{Path: path, Alpha: alpha})
	return false, c.saveCheckouts(checkouts)
}

// releaseCheckout removes the checkout of a conflict root held by an endpoint,
// if any, and returns whether it held one. A checkout held by the opposite
// endpoint is left as it is.
func (c *controller) releaseCheckout(alpha bool, path string) (bool, error) {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	checkouts, err := c.loadCheckouts()
	if err != nil {
		return false, err
	}
	index, found := slices.BinarySearchFunc(checkouts, path, compareCheckout)
	if !found || checkouts[index].Alpha != alpha {
		return false, nil
	}
	return true, c.saveCheckouts(slices.Delete(checkouts, index, index+1))
}

// removeCheckouts removes the session's checkouts from disk when the session
// is terminated.
func (c *controller) removeCheckouts() error {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	return c.saveCheckouts(nil)
}
