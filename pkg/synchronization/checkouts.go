package synchronization

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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

// checkoutsRecord is the serialized form of a session's checkouts (see
// remote.Checkout). Builds that didn't number claims serialized the checkouts
// alone, as an array, which loadCheckouts still reads.
type checkoutsRecord struct {
	// Sequence is the sequence number of the session's latest successful claim
	// (see claimCheckout). It's kept once the session's checkouts are released,
	// so that later claims are numbered after it.
	Sequence uint64 `json:",omitempty"`
	// Checkouts are the session's checkouts, sorted by path.
	Checkouts []remote.Checkout
}

// sequenceName is the name of the file in the checkouts directory that records
// the latest sequence number that the daemon issued to any session's claim
// (see issueSequence), which no session identifier matches.
const sequenceName = "sequence"

// sequenceLock serializes the issue of sequence numbers to the claims of the
// daemon's sessions (see issueSequence).
var sequenceLock sync.Mutex

// issueSequence issues a sequence number that's later than a session's
// previous one and than any that the daemon issued before, the latest of which
// it records durably in its data directory.
func issueSequence(previous uint64) (uint64, error) {
	sequenceLock.Lock()
	defer sequenceLock.Unlock()
	path, err := pathForCheckouts(sequenceName)
	if err != nil {
		return 0, err
	}
	var latest uint64
	if data, err := os.ReadFile(path); err == nil {
		if latest, err = strconv.ParseUint(string(data), 10, 64); err != nil {
			return 0, fmt.Errorf("the latest sequence number of conflict checkouts in %s, on the Mutagen daemon's system, is corrupt (%v); remove that file to resolve conflicts again", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("unable to read the latest sequence number of conflict checkouts: %w", err)
	}
	sequence := max(latest+1, previous+1, uint64(time.Now().UnixNano()))
	if err := remote.WriteFileDurably(path, []byte(strconv.FormatUint(sequence, 10))); err != nil {
		return 0, fmt.Errorf("unable to save the latest sequence number of conflict checkouts: %w", err)
	}
	return sequence, nil
}

// number issues the sequence number of a successful claim, which is later than
// that of the session's previous one, and than those that the daemon issued to
// other sessions' claims, since replacements compare sequence numbers by root
// and path (see remote.Replacement.Sequence). The daemon records the latest one
// that it issued (see issueSequence), so that sequence numbers remain ordered
// after earlier ones when the session's record of those is lost, such as when
// the session is recreated or its checkouts file is removed, even if the
// system clock was turned back meanwhile. Sequence numbers are also no earlier
// than the time at which they're issued, in nanoseconds since the epoch, so
// that they likely remain ordered after earlier ones if the daemon's record is
// lost too, or if another daemon issued them.
func (r *checkoutsRecord) number() (uint64, error) {
	sequence, err := issueSequence(r.Sequence)
	if err != nil {
		return 0, err
	}
	r.Sequence = sequence
	return sequence, nil
}

// loadCheckouts loads the session's checkouts, sorted by path, along with the
// sequence number of its latest successful claim. A file that can't be decoded
// is reported, rather than taken for one without checkouts, which would let
// both endpoints check out the same conflict. It must be called with
// checkoutsLock held.
func (c *controller) loadCheckouts() (*checkoutsRecord, error) {
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &checkoutsRecord{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("unable to read conflict checkouts: %w", err)
	}
	record := &checkoutsRecord{}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
		err = json.Unmarshal(data, &record.Checkouts)
	} else {
		err = json.Unmarshal(data, record)
	}
	if err != nil {
		return nil, fmt.Errorf("the session's conflict checkouts in %s, on the Mutagen daemon's system, are corrupt (%v); remove that file to resolve conflicts again, after which workspaces can only be applied if their conflicts are still listed, and must otherwise be discarded", path, err)
	}
	slices.SortFunc(record.Checkouts, func(a, b remote.Checkout) int {
		return compareCheckout(a, b.Path)
	})
	return record, nil
}

// saveCheckouts saves the session's checkouts durably (see
// remote.WriteFileDurably), since an operating system crash could otherwise
// leave the file empty, which loadCheckouts would then refuse. The file is kept
// once the session's checkouts are released, since it records the sequence
// number of its latest claim. It must be called with checkoutsLock held.
func (c *controller) saveCheckouts(record *checkoutsRecord) error {
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("unable to encode conflict checkouts: %w", err)
	} else if err := remote.WriteFileDurably(path, data); err != nil {
		return fmt.Errorf("unable to save conflict checkouts: %w", err)
	}
	return nil
}

// checkouts returns the session's checkouts, sorted by path.
func (c *controller) checkouts() ([]remote.Checkout, error) {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	record, err := c.loadCheckouts()
	if err != nil {
		return nil, err
	}
	return record.Checkouts, nil
}

// claimCheckout records a checkout of a conflict root through an endpoint,
// identified by the claim's token, and reports whether the endpoint held a
// checkout of the conflict root beforehand, along with its token. Each
// conflict is checked out through a single endpoint at a time, which alone can
// apply or discard its resolution, so the claim fails if the opposite endpoint
// holds the checkout. New checkouts are only recorded for conflict roots of
// the last synchronization cycle, while existing ones may outlive their
// conflicts, such as for unfinished resolutions. Checkouts are kept until
// released, including across daemon restarts, since workspaces outlive both.
//
// A claim leaves a checkout that the endpoint already holds as it is, so it
// only succeeds if the endpoint holds none or holds one with its token.
// Claims and releases may arrive after their submitters stopped waiting for
// them, such as on timeout, and each submission is handled separately, so a
// checkout keeps the token of the claim that recorded it until it's released
// by that token (see releaseCheckout): neither a late claim nor a late release
// can take over or remove the checkout of a later claim. A checkout held under
// another token must be released first, once its endpoint knows that it's
// abandoned. Checkouts recorded by builds that didn't issue tokens are the
// exception: they take the token of the first claim through their endpoint, so
// that workspaces checked out by those builds keep their checkouts, even if
// their conflicts are no longer listed, and late requests can't take them over
// or remove them afterward.
//
// A claim that succeeds is numbered with a sequence number, which orders the
// replacements that resolutions make after claiming their checkouts (see
// remote.Replacement.Sequence), since resolutions claim their checkouts again
// before applying their results.
func (c *controller) claimCheckout(alpha bool, path, token string) (remote.CheckoutStatus, error) {
	if token == "" {
		return remote.CheckoutStatus{}, errMissingToken
	}
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	record, err := c.loadCheckouts()
	if err != nil {
		return remote.CheckoutStatus{}, err
	}
	var status remote.CheckoutStatus
	index, found := slices.BinarySearchFunc(record.Checkouts, path, compareCheckout)
	if found && record.Checkouts[index].Alpha == alpha {
		status = remote.CheckoutStatus{Held: true, Token: record.Checkouts[index].Token}
		if status.Token != "" && status.Token != token {
			return status, nil
		}
		record.Checkouts[index].Token = token
	} else if found {
		return remote.CheckoutStatus{}, errCheckedOutOpposite
	} else {
		c.stateLock.Lock()
		listed := slices.ContainsFunc(c.state.Conflicts, func(conflict *core.Conflict) bool {
			return conflict.Root == path
		})
		c.stateLock.UnlockWithoutNotify()
		if !listed {
			return remote.CheckoutStatus{}, errNotConflictRoot
		}
		record.Checkouts = slices.Insert(record.Checkouts, index, remote.Checkout{Path: path, Alpha: alpha, Token: token})
	}
	if status.Sequence, err = record.number(); err != nil {
		return remote.CheckoutStatus{}, err
	} else if err := c.saveCheckouts(record); err != nil {
		return remote.CheckoutStatus{}, err
	}
	return status, nil
}

// releaseCheckout removes the checkout of a conflict root held by an endpoint
// if it has the specified token, and reports whether the endpoint held one,
// along with its token. A checkout held by the opposite endpoint is left as it
// is, as is one recorded by another claim, so that a release that arrives
// after its submitter stopped waiting for it doesn't remove the checkout of a
// later claim. Claims require a token, so a release without one only reports
// the token, unless a build that didn't issue tokens recorded the checkout,
// which the release then removes.
func (c *controller) releaseCheckout(alpha bool, path, token string) (remote.CheckoutStatus, error) {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	record, err := c.loadCheckouts()
	if err != nil {
		return remote.CheckoutStatus{}, err
	}
	index, found := slices.BinarySearchFunc(record.Checkouts, path, compareCheckout)
	if !found || record.Checkouts[index].Alpha != alpha {
		return remote.CheckoutStatus{}, nil
	}
	status := remote.CheckoutStatus{Held: true, Token: record.Checkouts[index].Token}
	if status.Token != token {
		return status, nil
	}
	record.Checkouts = slices.Delete(record.Checkouts, index, index+1)
	if err := c.saveCheckouts(record); err != nil {
		return remote.CheckoutStatus{}, err
	}
	return status, nil
}

// removeCheckouts removes the session's checkouts from disk when the session
// is terminated.
func (c *controller) removeCheckouts() error {
	c.checkoutsLock.Lock()
	defer c.checkoutsLock.Unlock()
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		return err
	} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unable to remove conflict checkouts: %w", err)
	}
	return nil
}
