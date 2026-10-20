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
	// Token identifies the latest claim of the checkout (see ClaimCheckout).
	Token string `json:",omitempty"`
}

// checkoutRequest identifies the conflict root of a claim or release
// submission. It follows the submission's header.
type checkoutRequest struct {
	// Path is the slash-separated conflict root relative to the
	// synchronization root, which is empty for the root itself.
	Path string
	// Token identifies the claim, or the claim whose checkout to release.
	Token string `json:",omitempty"`
}

// checkoutResponse reports the outcome of a claim or release submission. Hops
// that are unable to reach the daemon send a status instead, which decodes
// compatibly.
type checkoutResponse struct {
	// Error is the claim, release, or routing error, if any.
	Error string `json:",omitempty"`
	// Token is the token of the checkout that the submitting endpoint held
	// before the submission, if any.
	Token string `json:",omitempty"`
}

// validConflictRoot determines whether a path can be a conflict root.
func validConflictRoot(path string) bool {
	return path == "" || (path != "." && utf8.ValidString(path) && strings.IndexByte(path, 0) < 0 && validRelativePath(path))
}

// exchangeCheckout sends the conflict root and token of a claim or release
// submission (see Dial) and returns the token of the checkout that the
// submitting endpoint held beforehand, if any. It owns and closes the
// connection.
func exchangeCheckout(ctx context.Context, connection io.ReadWriteCloser, path, token string) (string, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	// A hop that rejects the submission may close the connection before
	// reading the request, so write errors are ignored in favor of the status
	// read next.
	writeMessage(connection, &checkoutRequest{Path: path, Token: token})
	var response checkoutResponse
	if err := readMessage(connection, &response); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("unable to receive checkout status: %w", err)
	} else if response.Error != "" {
		return "", errors.New(response.Error)
	}
	return response.Token, nil
}

// ClaimCheckout records a checkout of a conflict root by the submitting
// endpoint over a claim submission (see Dial), failing if the opposite endpoint
// holds it or, if neither does, if the path isn't a conflict root. The claim is
// identified by a token, which is required and replaces that of a checkout that
// the submitting endpoint already held. It returns whether the submitting
// endpoint already held it. It owns and closes the connection.
//
// Each claim should use a new token, so that releases of earlier claims don't
// remove its checkout: a release can arrive after its submitter stopped
// waiting for it, such as on timeout, and the conflict was claimed again.
func ClaimCheckout(ctx context.Context, connection io.ReadWriteCloser, path, token string) (bool, error) {
	held, err := exchangeCheckout(ctx, connection, path, token)
	return held != "", err
}

// ReleaseCheckout removes the checkout of a conflict root held by the
// submitting endpoint over a release submission (see Dial), if it has the
// specified token (see ClaimCheckout), and returns the token of the checkout
// that the submitting endpoint held beforehand, or an empty string if it held
// none. A checkout held by the opposite endpoint is left as it is. Since claims
// require a token, a release without one only reports the token, so that a
// checkout whose token is unknown is released by a second release naming it.
// It owns and closes the connection.
func ReleaseCheckout(ctx context.Context, connection io.ReadWriteCloser, path, token string) (string, error) {
	return exchangeCheckout(ctx, connection, path, token)
}

// ReceiveCheckout reads the conflict root and token of a claim or release
// submission arriving at the daemon, whose header has been received. The
// connection is closed if the request isn't received within a timeout or
// before cancellation.
func ReceiveCheckout(ctx context.Context, connection io.ReadWriteCloser) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, submissionTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	var request checkoutRequest
	err := readMessage(connection, &request)
	if !stop() {
		return "", "", fmt.Errorf("checkout request not received: %w", ctx.Err())
	} else if err != nil {
		return "", "", fmt.Errorf("unable to receive checkout request: %w", err)
	} else if !validConflictRoot(request.Path) {
		return "", "", errors.New("invalid conflict root")
	}
	return request.Path, request.Token, nil
}

// RespondToCheckout reports the outcome of a claim or release submission,
// including the token of the checkout that the submitting endpoint held
// beforehand, if any, and closes the connection. It returns any error
// encountered sending the response.
func RespondToCheckout(connection io.ReadWriteCloser, held string, err error) error {
	defer connection.Close()
	if err != nil {
		return writeMessage(connection, &checkoutResponse{Error: err.Error()})
	}
	return writeMessage(connection, &checkoutResponse{Token: held})
}
