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
	// Token identifies the claim that recorded the checkout (see
	// ClaimCheckout). Checkouts recorded by builds that didn't issue tokens
	// have none until they're claimed again.
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

// CheckoutStatus describes the outcome of a claim or release submission (see
// ClaimCheckout and ReleaseCheckout).
type CheckoutStatus struct {
	// Held indicates whether the submitting endpoint held a checkout of the
	// conflict root before the submission.
	Held bool `json:",omitempty"`
	// Token is the token of that checkout, which is empty if it was recorded
	// by a build that didn't issue tokens.
	Token string `json:",omitempty"`
	// Sequence is the sequence number of a claim that succeeded (see
	// ClaimCheckout), and zero otherwise.
	Sequence uint64 `json:",omitempty"`
}

// checkoutResponse reports the outcome of a claim or release submission. Hops
// that are unable to reach the daemon send a status instead, which decodes
// compatibly.
type checkoutResponse struct {
	// Error is the claim, release, or routing error, if any.
	Error string `json:",omitempty"`
	// ErrorKind classifies Error independently of its diagnostic text.
	ErrorKind ErrorKind `json:",omitempty"`
	// CheckoutStatus is the outcome, if there was no error.
	CheckoutStatus
}

// validConflictRoot determines whether a path can be a conflict root.
func validConflictRoot(path string) bool {
	return path == "" || (path != "." && utf8.ValidString(path) && strings.IndexByte(path, 0) < 0 && validRelativePath(path))
}

// exchangeCheckout sends the conflict root and token of a claim or release
// submission (see Dial) and returns its outcome. It owns and closes the
// connection.
func exchangeCheckout(ctx context.Context, connection io.ReadWriteCloser, path, token string) (*CheckoutStatus, error) {
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	// A hop that rejects the submission may close the connection before
	// reading the request, so write errors are ignored in favor of the status
	// read next.
	if err := writeMessage(connection, &checkoutRequest{Path: path, Token: token}); err != nil && ClassifyError(err) != ErrorKindConnection {
		return nil, err
	}
	var response checkoutResponse
	if err := readMessage(connection, &response); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ConnectionError(fmt.Errorf("unable to receive checkout status: %w", err))
	} else if response.Error != "" {
		return nil, receivedError(response.ErrorKind, errors.New(response.Error))
	}
	return &response.CheckoutStatus, nil
}

// ClaimCheckout records a checkout of a conflict root by the submitting
// endpoint over a claim submission (see Dial), failing if the opposite endpoint
// holds it or, if neither does, if the path isn't a conflict root. The claim is
// identified by a token, which is required. A checkout that the submitting
// endpoint already holds is left as it is, so the claim only succeeds if the
// endpoint holds no checkout of the conflict root, or holds one with the
// claim's token, or one that a build that didn't issue tokens recorded, which
// takes the claim's token. It owns and closes the connection.
//
// A claim that succeeds returns a sequence number, which is later than those of
// the earlier successful claims of the session's checkouts, through either
// endpoint, and orders the replacements that a resolution makes after claiming
// its checkout (see Replacement.Sequence). Otherwise, it returns the token of
// the checkout that the submitting endpoint holds.
//
// Each checkout should be claimed under a new token, so that claims and
// releases of earlier checkouts can't take over or remove it: they can arrive
// after their submitters stopped waiting for them, such as on timeout, and the
// conflict was claimed again. A checkout held under another token must thus be
// released (see ReleaseCheckout) before it's claimed again.
func ClaimCheckout(ctx context.Context, connection io.ReadWriteCloser, path, token string) (uint64, string, error) {
	status, err := exchangeCheckout(ctx, connection, path, token)
	if err != nil {
		return 0, "", err
	} else if status.Sequence != 0 {
		return status.Sequence, "", nil
	} else if !status.Held || status.Token == "" || status.Token == token {
		return 0, "", ConnectionError(errors.New("invalid checkout claim status"))
	}
	return 0, status.Token, nil
}

// ReleaseCheckout removes the checkout of a conflict root held by the
// submitting endpoint over a release submission (see Dial), if it has the
// specified token (see ClaimCheckout), and returns whether the submitting
// endpoint held a checkout beforehand and, if so, its token. A checkout held by
// the opposite endpoint is left as it is. Since claims require a token, a
// release without one only reports the token, so that a checkout whose token
// is unknown is released by a second release naming it, unless a build that
// didn't issue tokens recorded the checkout, in which case the release removes
// it. It owns and closes the connection.
func ReleaseCheckout(ctx context.Context, connection io.ReadWriteCloser, path, token string) (bool, string, error) {
	status, err := exchangeCheckout(ctx, connection, path, token)
	if err != nil {
		return false, "", err
	}
	return status.Held, status.Token, nil
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
		return "", "", ConnectionError(fmt.Errorf("unable to receive checkout request: %w", err))
	} else if !validConflictRoot(request.Path) {
		return "", "", errors.New("invalid conflict root")
	}
	return request.Path, request.Token, nil
}

// RespondToCheckout reports the outcome of a claim or release submission, which
// is described by status unless it failed, and closes the connection. It
// returns any error encountered sending the response.
func RespondToCheckout(connection io.ReadWriteCloser, status CheckoutStatus, err error) error {
	defer connection.Close()
	if err != nil {
		return writeMessage(connection, &checkoutResponse{Error: err.Error(), ErrorKind: ClassifyError(err)})
	}
	return writeMessage(connection, &checkoutResponse{CheckoutStatus: status})
}
