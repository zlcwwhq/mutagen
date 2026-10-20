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

	// The daemon receives the conflict root and token, and reports the token
	// of the checkout that the submitting endpoint held, or why it couldn't be
	// claimed or released.
	type request struct{ path, token string }
	serve := func(held string, outcome error) (net.Conn, chan request) {
		client, server := net.Pipe()
		received := make(chan request, 1)
		go func() {
			path, token, err := ReceiveCheckout(ctx, server)
			if err != nil {
				RespondToCheckout(server, "", err)
				received <- request{}
				return
			}
			RespondToCheckout(server, held, outcome)
			received <- request{path, token}
		}()
		return client, received
	}
	for _, path := range []string{"", "file", "sub/file"} {
		client, received := serve("previous", nil)
		if held, err := ClaimCheckout(ctx, client, path, "token"); err != nil || held != "previous" {
			t.Fatalf("claim of %q exchanged incorrectly: %q, %v", path, held, err)
		} else if r := <-received; r != (request{path, "token"}) {
			t.Fatalf("claim of %q received incorrectly: %+v", path, r)
		}
	}
	client, received := serve("", nil)
	if held, err := ClaimCheckout(ctx, client, "file", "token"); err != nil || held != "" || <-received != (request{"file", "token"}) {
		t.Fatal("new claim exchanged incorrectly:", held, err)
	}
	client, received = serve("previous", nil)
	if held, err := ReleaseCheckout(ctx, client, "file", ""); err != nil || held != "previous" || <-received != (request{"file", ""}) {
		t.Fatal("release exchanged incorrectly:", held, err)
	}
	client, _ = serve("", errors.New("is checked out elsewhere"))
	if held, err := ClaimCheckout(ctx, client, "file", "token"); err == nil || err.Error() != "is checked out elsewhere" || held != "" {
		t.Fatal("claim failure not reported:", held, err)
	}

	// Paths that can't be conflict roots are rejected.
	for _, path := range []string{".", "/file", "../file", "sub/../file", "sub\\file", "a\x00b"} {
		client, _ := serve("previous", nil)
		if _, err := ClaimCheckout(ctx, client, path, "token"); err == nil || err.Error() != "invalid conflict root" {
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
	if _, err := ReleaseCheckout(ctx, client, "file", "token"); err == nil || err.Error() != "beta is not connected" {
		t.Fatal("rejection not reported:", err)
	}
}

func TestListingIncludesCheckouts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := &Listing{
		Conflicts: []ConflictRoot{{Path: "file", Files: true}},
		Problems:  []Problem{{Path: "problem", Error: "unreadable"}},
		Checkouts: []Checkout{{Path: "file", Alpha: true, Token: "token"}, {Path: "unfinished", Token: "other"}},
	}
	client, server := net.Pipe()
	go RespondWithConflicts(server, sent, nil)
	if received, err := ListConflicts(ctx, client); err != nil || !reflect.DeepEqual(received, sent) {
		t.Fatalf("listing received incorrectly: %+v, %v", received, err)
	}
}
