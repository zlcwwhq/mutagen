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

// errMissingToken indicates that a checkout was claimed without a token.
var errMissingToken = errors.New("checkout claims require a token")

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

// claimCheckout records a checkout of a conflict root through an endpoint,
// identified by the claim's token, and returns the token of the checkout that
// the endpoint held beforehand, if any. Each conflict is checked out through a
// single endpoint at a time, which alone can apply or discard its resolution,
// so the claim fails if the opposite endpoint holds the checkout. New
// checkouts are only recorded for conflict roots of the last synchronization
// cycle, while existing ones may outlive their conflicts, such as for
// unfinished resolutions. Checkouts are kept until released, including across
// daemon restarts, since workspaces outlive both.
//
// A claim leaves a checkout that the endpoint already holds as it is, so it's
// only recorded if the returned token is empty or its own. Claims and releases
// may arrive after their submitters stopped waiting for them, such as on
// timeout, and each submission is handled separately, so a checkout keeps the
// token of the claim that recorded it until it's released by that token (see
// releaseCheckout): neither a late claim nor a late release can take over or
// remove the checkout of a later claim. A checkout held under another token
// must be released first, once its endpoint knows that it's abandoned.
func (c *controller) claimCheckout(alpha bool, path, token string) (string, error) {
	if token == "" {
		return "", errMissingToken
	}
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	checkouts, err := c.loadCheckouts()
	if err != nil {
		return "", err
	}
	index, found := slices.BinarySearchFunc(checkouts, path, compareCheckout)
	if found && checkouts[index].Alpha == alpha {
		return checkouts[index].Token, nil
	} else if found {
		return "", errCheckedOutOpposite
	}
	c.stateLock.Lock()
	listed := slices.ContainsFunc(c.state.Conflicts, func(conflict *core.Conflict) bool {
		return conflict.Root == path
	})
	c.stateLock.UnlockWithoutNotify()
	if !listed {
		return "", errNotConflictRoot
	}
	checkouts = slices.Insert(checkouts, index, remote.Checkout{Path: path, Alpha: alpha, Token: token})
	return "", c.saveCheckouts(checkouts)
}

// releaseCheckout removes the checkout of a conflict root held by an endpoint
// if it has the specified token, and returns the token of the checkout that
// the endpoint held, if any. A checkout held by the opposite endpoint is left
// as it is, as is one recorded by another claim, so that a release that
// arrives after its submitter stopped waiting for it doesn't remove the
// checkout of a later claim. Claims require a token, so a release without one
// only reports the token.
func (c *controller) releaseCheckout(alpha bool, path, token string) (string, error) {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	checkouts, err := c.loadCheckouts()
	if err != nil {
		return "", err
	}
	index, found := slices.BinarySearchFunc(checkouts, path, compareCheckout)
	if !found || checkouts[index].Alpha != alpha {
		return "", nil
	}
	held := checkouts[index].Token
	if held != token {
		return held, nil
	}
	return held, c.saveCheckouts(slices.Delete(checkouts, index, index+1))
}

// removeCheckouts removes the session's checkouts from disk when the session
// is terminated.
func (c *controller) removeCheckouts() error {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	return c.saveCheckouts(nil)
}
