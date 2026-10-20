package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Checkout records that a conflict was checked out for resolution through an
// endpoint, which alone can apply or discard its resolution, so that each
// conflict has a single workspace on either endpoint. Workspaces are kept on
// the system that checked them out, where the opposite endpoint can't see
// them, so the daemon keeps each session's checkouts.
type Checkout struct {
	// Path is the slash-separated conflict root relative to the
	// synchronization root, which is empty for the root itself.
	Path string
	// Alpha indicates whether the conflict was checked out through alpha.
	Alpha bool `json:",omitempty"`
}

// checkoutRequest identifies the conflict root of a claim or release
// submission. It follows the submission's header.
type checkoutRequest struct {
	// Path is the slash-separated conflict root relative to the
	// synchronization root, which is empty for the root itself.
	Path string
}

// checkoutResponse reports the outcome of a claim or release submission. Hops
// that are unable to reach the daemon send a status instead, which decodes
// compatibly.
type checkoutResponse struct {
	// Error is the claim, release, or routing error, if any.
	Error string `json:",omitempty"`
	// Held indicates whether the submitting endpoint held the checkout before
	// the submission.
	Held bool `json:",omitempty"`
}

// validConflictRoot determines whether a path can be a conflict root.
func validConflictRoot(path string) bool {
	return path == "" || (path != "." && utf8.ValidString(path) && strings.IndexByte(path, 0) < 0 && validRelativePath(path))
}

// exchangeCheckout sends the conflict root of a claim or release submission
// (see Dial) and returns whether the submitting endpoint held its checkout
// beforehand. It owns and closes the connection.
func exchangeCheckout(ctx context.Context, connection io.ReadWriteCloser, path string) (bool, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	// A hop that rejects the submission may close the connection before
	// reading the request, so write errors are ignored in favor of the status
	// read next.
	writeMessage(connection, &checkoutRequest{Path: path})
	var response checkoutResponse
	if err := readMessage(connection, &response); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("unable to receive checkout status: %w", err)
	} else if response.Error != "" {
		return false, errors.New(response.Error)
	}
	return response.Held, nil
}

// ClaimCheckout records a checkout of a conflict root by the submitting
// endpoint over a claim submission (see Dial), failing if the opposite endpoint
// holds it or, if neither does, if the path isn't a conflict root. It returns
// whether the submitting endpoint already held it. It owns and closes the
// connection.
func ClaimCheckout(ctx context.Context, connection io.ReadWriteCloser, path string) (bool, error) {
	return exchangeCheckout(ctx, connection, path)
}

// ReleaseCheckout removes the checkout of a conflict root held by the
// submitting endpoint over a release submission (see Dial), and returns whether
// it held one. A checkout held by the opposite endpoint is left as it is. It
// owns and closes the connection.
func ReleaseCheckout(ctx context.Context, connection io.ReadWriteCloser, path string) (bool, error) {
	return exchangeCheckout(ctx, connection, path)
}

// ReceiveCheckout reads the conflict root of a claim or release submission
// arriving at the daemon, whose header has been received. The connection is
// closed if the request isn't received within a timeout or before
// cancellation.
func ReceiveCheckout(ctx context.Context, connection io.ReadWriteCloser) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, submissionTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	var request checkoutRequest
	err := readMessage(connection, &request)
	if !stop() {
		return "", fmt.Errorf("checkout request not received: %w", ctx.Err())
	} else if err != nil {
		return "", fmt.Errorf("unable to receive checkout request: %w", err)
	} else if !validConflictRoot(request.Path) {
		return "", errors.New("invalid conflict root")
	}
	return request.Path, nil
}

// RespondToCheckout reports the outcome of a claim or release submission and
// closes the connection. It returns any error encountered sending the
// response.
func RespondToCheckout(connection io.ReadWriteCloser, held bool, err error) error {
	defer connection.Close()
	if err != nil {
		return writeMessage(connection, &checkoutResponse{Error: err.Error()})
	}
	return writeMessage(connection, &checkoutResponse{Held: held})
}
