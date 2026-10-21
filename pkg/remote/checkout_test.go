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

	// The daemon receives the conflict root and token, and reports whether the
	// submitting endpoint held a checkout and its token, along with the
	// sequence number of a claim that succeeded, or why it couldn't be claimed
	// or released.
	type request struct{ path, token string }
	serve := func(status CheckoutStatus, outcome error) (net.Conn, chan request) {
		client, server := net.Pipe()
		received := make(chan request, 1)
		go func() {
			path, token, err := ReceiveCheckout(ctx, server)
			if err != nil {
				RespondToCheckout(server, CheckoutStatus{}, err)
				received <- request{}
				return
			}
			RespondToCheckout(server, status, outcome)
			received <- request{path, token}
		}()
		return client, received
	}
	for _, path := range []string{"", "file", "sub/file"} {
		client, received := serve(CheckoutStatus{Held: true, Token: "token", Sequence: 7}, nil)
		if sequence, held, err := ClaimCheckout(ctx, client, path, "token"); err != nil || sequence != 7 || held != "" {
			t.Fatalf("claim of %q exchanged incorrectly: %d, %q, %v", path, sequence, held, err)
		} else if r := <-received; r != (request{path, "token"}) {
			t.Fatalf("claim of %q received incorrectly: %+v", path, r)
		}
	}
	client, received := serve(CheckoutStatus{Sequence: 8}, nil)
	if sequence, held, err := ClaimCheckout(ctx, client, "file", "token"); err != nil || sequence != 8 || held != "" || <-received != (request{"file", "token"}) {
		t.Fatal("new claim exchanged incorrectly:", sequence, held, err)
	}

	// A claim of a checkout recorded without a token, by a build that didn't
	// issue tokens, succeeds, while one of a checkout held under another token
	// reports that token instead.
	client, _ = serve(CheckoutStatus{Held: true, Sequence: 9}, nil)
	if sequence, held, err := ClaimCheckout(ctx, client, "file", "token"); err != nil || sequence != 9 || held != "" {
		t.Fatal("claim of a checkout without a token exchanged incorrectly:", sequence, held, err)
	}
	client, _ = serve(CheckoutStatus{Held: true, Token: "previous"}, nil)
	if sequence, held, err := ClaimCheckout(ctx, client, "file", "token"); err != nil || sequence != 0 || held != "previous" {
		t.Fatal("claim of a checkout held under another token exchanged incorrectly:", sequence, held, err)
	}

	// A claim must succeed or report another token.
	for _, status := range []CheckoutStatus{{}, {Held: true}, {Held: true, Token: "token"}} {
		client, _ := serve(status, nil)
		if _, _, err := ClaimCheckout(ctx, client, "file", "token"); err == nil {
			t.Fatalf("invalid claim status accepted: %+v", status)
		}
	}

	// Releases report whether a checkout was held, including one recorded
	// without a token, and its token.
	for _, status := range []CheckoutStatus{{Held: true, Token: "previous"}, {Held: true}, {}} {
		client, received := serve(status, nil)
		if held, token, err := ReleaseCheckout(ctx, client, "file", ""); err != nil || held != status.Held || token != status.Token || <-received != (request{"file", ""}) {
			t.Fatalf("release exchanged incorrectly: %t, %q, %v", held, token, err)
		}
	}
	client, _ = serve(CheckoutStatus{}, errors.New("is checked out elsewhere"))
	if sequence, held, err := ClaimCheckout(ctx, client, "file", "token"); err == nil || err.Error() != "is checked out elsewhere" || sequence != 0 || held != "" {
		t.Fatal("claim failure not reported:", sequence, held, err)
	}

	// Paths that can't be conflict roots are rejected.
	for _, path := range []string{".", "/file", "../file", "sub/../file", "sub\\file", "a\x00b"} {
		client, _ := serve(CheckoutStatus{Sequence: 1}, nil)
		if _, _, err := ClaimCheckout(ctx, client, path, "token"); err == nil || err.Error() != "invalid conflict root" {
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
	if _, _, err := ReleaseCheckout(ctx, client, "file", "token"); err == nil || err.Error() != "beta is not connected" {
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
