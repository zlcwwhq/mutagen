package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/multiplexing"
)

func TestErrorClassification(t *testing.T) {
	operation := errors.New("connection lost is just diagnostic text")
	connection := ConnectionError(errors.New("permission denied is just diagnostic text"))
	for _, test := range []struct {
		err  error
		kind ErrorKind
	}{
		{operation, ErrorKindOperation},
		{connection, ErrorKindConnection},
		{fmt.Errorf("context: %w", operation), ErrorKindOperation},
		{fmt.Errorf("context: %w", connection), ErrorKindConnection},
		{errors.Join(operation, connection), ErrorKindConnection},
		{errors.Join(connection, operation), ErrorKindConnection},
		{errors.Join(connection, context.DeadlineExceeded), ErrorKindTimeout},
		{errors.Join(context.DeadlineExceeded, connection), ErrorKindTimeout},
		{fmt.Errorf("internal dial timeout: %w", ConnectionError(context.DeadlineExceeded)), ErrorKindConnection},
		{fmt.Errorf("caller timeout: %w", context.DeadlineExceeded), ErrorKindTimeout},
	} {
		if actual := ClassifyError(test.err); actual != test.kind {
			t.Errorf("%v: classified as %s, expected %s", test.err, actual, test.kind)
		}
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", ConnectionError(operation)), operation) {
		t.Fatal("classification discarded the underlying cause")
	}
}

func TestProtocolErrorCategories(t *testing.T) {
	// All error-bearing responses preserve the same category across actual
	// protocol exchanges, including routing rejections of non-command modes.
	for _, cause := range []error{errors.New("operation rejected"), ConnectionError(errors.New("unreachable")), context.DeadlineExceeded} {
		for _, operation := range []string{"route", "flush", "conflicts", "checkout", "initialization"} {
			t.Run(string(ClassifyError(cause))+"/"+operation, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()
				var err error
				switch operation {
				case "route":
					go Reject(server, cause)
					_, err = connect(ctx, client)
				case "flush":
					go RespondToFlush(server, nil, cause)
					_, err = Flush(ctx, client)
				case "conflicts":
					go RespondWithConflicts(server, nil, cause)
					_, err = ListConflicts(ctx, client)
				case "checkout":
					go func() {
						ReceiveCheckout(ctx, server)
						RespondToCheckout(server, CheckoutStatus{}, cause)
					}()
					_, _, err = ClaimCheckout(ctx, client, "file", "token")
				case "initialization":
					go func() {
						var endpoint Endpoint
						readMessage(server, &endpoint)
						writeMessage(server, &initializeResponse{Error: cause.Error(), ErrorKind: ClassifyError(cause)})
					}()
					carrier := multiplexing.NewCarrierFromStream(client)
					err = initializeExecutor(logging.NewLogger(logging.LevelDisabled, io.Discard), carrier, &Endpoint{})
				}
				if err == nil || ClassifyError(err) != ClassifyError(cause) || !strings.Contains(err.Error(), cause.Error()) {
					t.Fatalf("category or diagnostic lost: %v (%s)", err, ClassifyError(err))
				}
			})
		}
	}

	// Result errors retain both the category and their specific operation
	// type, so hints and conflict retry logic continue to work.
	response := failedResult(ConnectionError(errors.New("directory missing")))
	response.DirectoryError, response.Directory = DirectoryErrorMissing, "/missing"
	var encoded bytes.Buffer
	if err := writeMessage(&encoded, &response); err != nil {
		t.Fatal(err)
	}
	var decoded result
	if err := readMessage(&encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	var directory *DirectoryError
	if err := decoded.executionError(); !errors.As(err, &directory) || directory.Path != "/missing" || ClassifyError(err) != ErrorKindConnection {
		t.Fatalf("directory error type or category lost: %v", err)
	}
	for _, stale := range []bool{false, true} {
		response := failedResult(errors.New("replacement rejected"))
		response.Stale, response.Superseded = stale, !stale
		expected := ErrSuperseded
		if stale {
			expected = ErrStale
		}
		if err := responseError(&response); !errors.Is(err, expected) || ClassifyError(err) != ErrorKindOperation {
			t.Fatalf("replacement error type or category lost: %v", err)
		}
	}
}

func TestMetadataInputAndTransportFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := helperRequest(t, "exit")
	request.Environment = append(request.Environment, "LARGE="+strings.Repeat("x", maximumMessageSize))
	if _, err := executeTest(t, ctx, request, nil, nil, nil); err == nil || ClassifyError(err) != ErrorKindOperation {
		t.Fatalf("oversized local request not classified as input failure: %v", err)
	}
	if err := writeMessage(failingIO{}, &Request{}); err == nil || ClassifyError(err) != ErrorKindConnection {
		t.Fatalf("metadata transport write not classified as connection failure: %v", err)
	}
	client, server := net.Pipe()
	go func() {
		// A complete frame with an invalid JSON body is a protocol failure.
		server.Write([]byte{0, 0, 0, 1, '!'})
		server.Close()
	}()
	if _, err := connect(ctx, client); err == nil || ClassifyError(err) != ErrorKindConnection {
		t.Fatalf("invalid metadata not classified as protocol failure: %v", err)
	}
}
