package remote

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestCheckoutExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The daemon receives the conflict root and reports whether the
	// submitting endpoint held its checkout, or why it couldn't be claimed.
	exchange := func(path string, held bool, outcome error) (string, bool, error) {
		client, server := net.Pipe()
		received := make(chan string, 1)
		go func() {
			path, err := ReceiveCheckout(ctx, server)
			if err != nil {
				RespondToCheckout(server, false, err)
				received <- ""
				return
			}
			RespondToCheckout(server, held, outcome)
			received <- path
		}()
		held, err := ClaimCheckout(ctx, client, path)
		return <-received, held, err
	}
	for _, path := range []string{"", "file", "sub/file"} {
		if received, held, err := exchange(path, true, nil); err != nil || received != path || !held {
			t.Fatalf("checkout of %q exchanged incorrectly: %q, %t, %v", path, received, held, err)
		}
	}
	if _, held, err := exchange("file", false, errors.New("is checked out elsewhere")); err == nil || err.Error() != "is checked out elsewhere" || held {
		t.Fatal("claim failure not reported:", held, err)
	}

	// Paths that can't be conflict roots are rejected.
	for _, path := range []string{".", "/file", "../file", "sub/../file", "sub\\file", "a\x00b"} {
		if _, _, err := exchange(path, true, nil); err == nil || err.Error() != "invalid conflict root" {
			t.Fatalf("invalid conflict root %q accepted: %v", path, err)
		}
	}

	// Hops that can't reach the daemon send a status, which is reported. Pipes
	// are unbuffered, so the hop reads the request first.
	client, server := net.Pipe()
	go func() {
		var request checkoutRequest
		readMessage(server, &request)
		Reject(server, errors.New("beta is not connected"))
	}()
	if _, err := ReleaseCheckout(ctx, client, "file"); err == nil || err.Error() != "beta is not connected" {
		t.Fatal("rejection not reported:", err)
	}
}

func TestListingIncludesCheckouts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := &Listing{
		Conflicts: []ConflictRoot{{Path: "file", Files: true}},
		Problems:  []Problem{{Path: "problem", Error: "unreadable"}},
		Checkouts: []Checkout{{Path: "file", Alpha: true}, {Path: "unfinished"}},
	}
	client, server := net.Pipe()
	go RespondWithConflicts(server, sent, nil)
	if received, err := ListConflicts(ctx, client); err != nil || !reflect.DeepEqual(received, sent) {
		t.Fatalf("listing received incorrectly: %+v, %v", received, err)
	}
}
