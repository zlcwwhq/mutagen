package synchronization

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// claims checks the outcomes of claims, which must be numbered in order if
// they succeed.
type claims struct {
	// last is the sequence number of the last successful claim.
	last uint64
}

// succeeded determines whether a claim succeeded with a sequence number later
// than that of the last one, and whether it reported the checkout that the
// endpoint held beforehand.
func (c *claims) succeeded(status remote.CheckoutStatus, err error, held bool, token string) bool {
	if err != nil || status.Held != held || status.Token != token || status.Sequence <= c.last {
		return false
	}
	c.last = status.Sequence
	return true
}

func TestCheckoutClaims(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	c := newCheckoutController("", "file", "other")
	claims := &claims{}

	// New checkouts are only recorded for conflict roots, and claims require a
	// token.
	if _, err := c.claimCheckout(true, "missing", "a"); !errors.Is(err, errNotConflictRoot) {
		t.Fatal("checkout of a path that isn't a conflict root claimed:", err)
	} else if _, err := c.claimCheckout(true, "file", ""); !errors.Is(err, errMissingToken) {
		t.Fatal("checkout claimed without a token:", err)
	}

	// Each conflict is checked out through a single endpoint: claiming it again
	// through the same endpoint, under its token, succeeds and reports that
	// it's held, while the opposite endpoint's claim fails.
	if status, err := c.claimCheckout(true, "file", "a"); !claims.succeeded(status, err, false, "") {
		t.Fatal("checkout not claimed:", status, err)
	} else if status, err := c.claimCheckout(true, "file", "a"); !claims.succeeded(status, err, true, "a") {
		t.Fatal("held checkout not claimed again:", status, err)
	} else if _, err := c.claimCheckout(false, "file", "b"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout claimed through both endpoints:", err)
	} else if status, err := c.claimCheckout(false, "other", "c"); !claims.succeeded(status, err, false, "") {
		t.Fatal("checkout of another conflict not claimed:", status, err)
	} else if status, err := c.claimCheckout(false, "", "d"); !claims.succeeded(status, err, false, "") {
		t.Fatal("checkout of the root not claimed:", status, err)
	}

	// Checkouts are kept on disk, sorted by path, so that they survive daemon
	// restarts, and they outlive their conflicts, such as for unfinished
	// resolutions. Claims are numbered after those made before the restart.
	restarted := newCheckoutController()
	expected := []remote.Checkout{{Path: "", Token: "d"}, {Path: "file", Alpha: true, Token: "a"}, {Path: "other", Token: "c"}}
	if checkouts, err := restarted.checkouts(); err != nil || !reflect.DeepEqual(checkouts, expected) {
		t.Fatalf("checkouts not kept: %+v, %v", checkouts, err)
	} else if status, err := restarted.claimCheckout(true, "file", "a"); !claims.succeeded(status, err, true, "a") {
		t.Fatal("checkout of a conflict that's no longer listed not claimed again:", status, err)
	} else if _, err := restarted.claimCheckout(true, "other", "e"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout of a conflict that's no longer listed claimed by the opposite endpoint:", err)
	}

	// Only the endpoint holding a checkout releases it, by its token, after
	// which the opposite endpoint can claim it. A release without the token
	// only reports it.
	held := remote.CheckoutStatus{Held: true, Token: "a"}
	if status, err := c.releaseCheckout(false, "file", "a"); err != nil || status.Held {
		t.Fatal("checkout released by the opposite endpoint:", status, err)
	} else if status, err := c.releaseCheckout(true, "file", ""); err != nil || status != held {
		t.Fatal("token of the held checkout not reported:", status, err)
	} else if status, err := c.releaseCheckout(true, "file", "a"); err != nil || status != held {
		t.Fatal("checkout not released:", status, err)
	} else if status, err := c.releaseCheckout(true, "file", "a"); err != nil || status.Held {
		t.Fatal("released checkout reported as held:", status, err)
	} else if status, err := c.claimCheckout(false, "file", "f"); !claims.succeeded(status, err, false, "") {
		t.Fatal("released checkout not claimed by the opposite endpoint:", status, err)
	}

	// Listings include checkouts.
	if listing, err := c.remoteConflicts("sha1"); err != nil || len(listing.Checkouts) != 3 || listing.Checkouts[1] != (remote.Checkout{Path: "file", Token: "f"}) {
		t.Fatalf("checkouts not listed: %+v, %v", listing, err)
	}

	// The checkouts are kept on disk once released, since they record the
	// sequence number of the latest claim, and removed when the session is
	// terminated.
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	for _, checkout := range []remote.Checkout{{Path: "", Token: "d"}, {Path: "file", Token: "f"}, {Path: "other", Token: "c"}} {
		if _, err := c.releaseCheckout(false, checkout.Path, checkout.Token); err != nil {
			t.Fatal(err)
		}
	}
	if checkouts, err := c.checkouts(); err != nil || len(checkouts) != 0 {
		t.Fatalf("checkouts not released: %+v, %v", checkouts, err)
	} else if _, err := os.Lstat(path); err != nil {
		t.Fatal("checkouts not kept once released:", err)
	} else if status, err := c.claimCheckout(true, "file", "g"); !claims.succeeded(status, err, false, "") {
		t.Fatal("checkout not claimed once the checkouts were released:", status, err)
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
	claims := &claims{}

	// A claim and a release can arrive after their submitter stopped waiting
	// for them, such as on timeout, and after the conflict was checked out
	// again through the same endpoint. The checkout keeps the token of the
	// claim that recorded it, so the late claim fails, reporting the token,
	// and the late release leaves the checkout, which the opposite endpoint
	// thus can't claim, until it's released by its token.
	expected := []remote.Checkout{{Path: "file", Alpha: true, Token: "new"}}
	held := remote.CheckoutStatus{Held: true, Token: "new"}
	if status, err := c.claimCheckout(true, "file", "new"); !claims.succeeded(status, err, false, "") {
		t.Fatal("checkout not claimed:", status, err)
	} else if status, err := c.claimCheckout(true, "file", "old"); err != nil || status != held {
		t.Fatal("held checkout not reported to a late claim:", status, err)
	} else if status, err := c.releaseCheckout(true, "file", "old"); err != nil || status != held {
		t.Fatal("held checkout not reported to a late release:", status, err)
	} else if checkouts, err := c.checkouts(); err != nil || !reflect.DeepEqual(checkouts, expected) {
		t.Fatalf("checkout changed by a late claim and release: %+v, %v", checkouts, err)
	} else if _, err := c.claimCheckout(false, "file", "opposite"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout claimed by the opposite endpoint after a late claim and release:", err)
	} else if status, err := c.releaseCheckout(true, "file", "new"); err != nil || status != held {
		t.Fatal("checkout not released by its token:", status, err)
	}

	// A claim that arrives after the checkout was released records a checkout
	// without a workspace, which a later claim through the same endpoint
	// leaves as it is, until it's released by its token.
	abandoned := remote.CheckoutStatus{Held: true, Token: "old"}
	if status, err := c.claimCheckout(true, "file", "old"); !claims.succeeded(status, err, false, "") {
		t.Fatal("late claim not recorded:", status, err)
	} else if status, err := c.claimCheckout(true, "file", "next"); err != nil || status != abandoned {
		t.Fatal("abandoned checkout not reported:", status, err)
	} else if status, err := c.releaseCheckout(true, "file", "old"); err != nil || status != abandoned {
		t.Fatal("abandoned checkout not released:", status, err)
	} else if status, err := c.claimCheckout(true, "file", "next"); !claims.succeeded(status, err, false, "") {
		t.Fatal("released checkout not claimed again:", status, err)
	}
}

// writeCheckouts writes the checkouts file of a controller's session.
func writeCheckouts(t *testing.T, c *controller, data string) string {
	t.Helper()
	path, err := pathForCheckouts(c.session.Identifier)
	if err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckoutSequences(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	c := newCheckoutController("file", "other")

	// Successful claims are numbered after the latest one recorded, while
	// claims that fail aren't numbered. The latest sequence number is kept
	// once the session's checkouts are released and across restarts.
	const recorded = uint64(1) << 62
	path := writeCheckouts(t, c, `{"Sequence":4611686018427387904,"Checkouts":[{"Path":"other","Token":"held"}]}`)
	if status, err := c.claimCheckout(true, "file", "a"); err != nil || status.Sequence != recorded+1 {
		t.Fatal("claim not numbered after the latest one:", status, err)
	} else if status, err := c.claimCheckout(true, "file", "b"); err != nil || status.Sequence != 0 {
		t.Fatal("claim of a checkout held under another token numbered:", status, err)
	} else if _, err := c.claimCheckout(true, "other", "c"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout claimed through both endpoints:", err)
	} else if _, err := c.releaseCheckout(true, "file", "a"); err != nil {
		t.Fatal(err)
	} else if _, err := c.releaseCheckout(false, "other", "held"); err != nil {
		t.Fatal(err)
	} else if status, err := newCheckoutController("file").claimCheckout(false, "file", "d"); err != nil || status.Sequence != recorded+2 {
		t.Fatal("claim not numbered after those of released checkouts:", status, err)
	}

	// The daemon records the latest sequence number that it issued, so claims
	// remain numbered after earlier ones if the session's record of those is
	// lost, such as when the session is recreated, even though the system
	// clock is behind them (as if it had been turned back since), and claims
	// of other sessions are numbered after them too.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	} else if status, err := c.claimCheckout(true, "file", "e"); err != nil || status.Sequence != recorded+3 {
		t.Fatal("claim not numbered after those of a lost record:", status, err)
	}
	other := newCheckoutController("file")
	other.session.Identifier = "sync_other"
	if status, err := other.claimCheckout(false, "file", "f"); err != nil || status.Sequence != recorded+4 {
		t.Fatal("claim not numbered after those of another session:", status, err)
	}

	// Sequence numbers are no earlier than the time at which they're issued,
	// so that they likely remain ordered after earlier ones if the daemon's
	// record of those is lost too.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	before := uint64(time.Now().UnixNano())
	if status, err := c.claimCheckout(true, "file", "g"); err != nil || status.Sequence < before {
		t.Fatal("claim numbered before the time at which it was made:", status, err)
	}

	// A corrupt record of the latest sequence number is reported, rather than
	// taken for none, which could number claims before earlier ones.
	sequencePath, err := pathForCheckouts(sequenceName)
	if err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(sequencePath, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	} else if _, err := c.claimCheckout(true, "file", "g"); err == nil || !strings.Contains(err.Error(), "is corrupt") {
		t.Fatal("corrupt record of the latest sequence number not reported:", err)
	}
}

func TestCheckoutLegacyRecords(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())

	// Builds that didn't issue tokens recorded checkouts without them, as an
	// array, which is still read, including for conflicts that are no longer
	// listed, such as those of partially applied resolutions.
	c := newCheckoutController()
	path := writeCheckouts(t, c, `[{"Path":"workspace","Alpha":true},{"Path":"abandoned"}]`)
	expected := []remote.Checkout{{Path: "abandoned"}, {Path: "workspace", Alpha: true}}
	if checkouts, err := c.checkouts(); err != nil || !reflect.DeepEqual(checkouts, expected) {
		t.Fatalf("checkouts recorded without tokens not read: %+v, %v", checkouts, err)
	}

	// Releases report that such a checkout is held, even with a token, which
	// leaves it as it is, and the first claim through its endpoint succeeds,
	// recording the claim's token, so that late requests can't take it over
	// or remove it afterward.
	legacy := remote.CheckoutStatus{Held: true}
	if status, err := c.releaseCheckout(true, "workspace", "token"); err != nil || status != legacy {
		t.Fatal("checkout without a token not reported to a release:", status, err)
	} else if _, err := c.claimCheckout(false, "workspace", "opposite"); !errors.Is(err, errCheckedOutOpposite) {
		t.Fatal("checkout without a token claimed through the opposite endpoint:", err)
	} else if status, err := c.claimCheckout(true, "workspace", "token"); err != nil || !status.Held || status.Token != "" || status.Sequence == 0 {
		t.Fatal("checkout without a token not claimed:", status, err)
	} else if status, err := c.claimCheckout(true, "workspace", "late"); err != nil || status != (remote.CheckoutStatus{Held: true, Token: "token"}) {
		t.Fatal("token of a claimed checkout not kept:", status, err)
	} else if status, err := c.releaseCheckout(true, "workspace", ""); err != nil || status != (remote.CheckoutStatus{Held: true, Token: "token"}) {
		t.Fatal("claimed checkout not kept by a release without a token:", status, err)
	}
	var record checkoutsRecord
	if data, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal("checkouts not saved in the current format:", err)
	} else if !reflect.DeepEqual(record.Checkouts, []remote.Checkout{{Path: "abandoned"}, {Path: "workspace", Alpha: true, Token: "token"}}) || record.Sequence == 0 {
		t.Fatalf("checkouts not saved: %+v", record)
	}

	// A release without a token removes a checkout recorded without one.
	if status, err := c.releaseCheckout(false, "abandoned", ""); err != nil || status != legacy {
		t.Fatal("checkout without a token not released:", status, err)
	} else if status, err := c.releaseCheckout(false, "abandoned", ""); err != nil || status.Held {
		t.Fatal("released checkout reported as held:", status, err)
	}
}

func TestCorruptCheckouts(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
	c := newCheckoutController("file")

	// Checkouts that can't be decoded, such as an empty file left by an
	// operating system crash, aren't taken for no checkouts, which would let
	// both endpoints check out a conflict. Errors identify the file, which is
	// left as it is, and suggest removing it.
	path := writeCheckouts(t, c, "")
	reported := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), path) && strings.Contains(err.Error(), "corrupt") &&
			strings.Contains(err.Error(), "remove that file")
	}
	if _, err := c.checkouts(); !reported(err) {
		t.Fatal("corrupt checkouts not reported:", err)
	} else if _, err := c.remoteConflicts("sha1"); !reported(err) {
		t.Fatal("corrupt checkouts not reported by listings:", err)
	} else if _, err := c.claimCheckout(true, "file", "token"); !reported(err) {
		t.Fatal("corrupt checkouts not reported by claims:", err)
	} else if _, err := c.releaseCheckout(true, "file", ""); !reported(err) {
		t.Fatal("corrupt checkouts not reported by releases:", err)
	} else if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
		t.Fatal("corrupt checkouts changed:", err)
	}

	// Once the file is removed, conflicts can be checked out again. Checkouts
	// and the latest sequence number are saved durably, without leaving
	// temporary files behind.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	} else if status, err := c.claimCheckout(true, "file", "token"); err != nil || status.Sequence == 0 {
		t.Fatal("checkout not claimed once corrupt checkouts were removed:", status, err)
	} else if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 2 ||
		entries[0].Name() != sequenceName || entries[1].Name() != c.session.Identifier {
		t.Fatal("checkouts not saved or temporary files left behind:", entries, err)
	}
}
