package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestForwardedDirectoryError preserves executor error details through a daemon
// boundary, so callers can still explain a wrongly expanded home directory.
func TestForwardedDirectoryError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, server := net.Pipe()
	go ServeForwarded(ctx, server, func(context.Context, *Request, io.Reader, io.Writer, io.Writer, func(*CommandEvent) error) (*CommandResult, error) {
		return &CommandResult{ExitCode: 126}, &DirectoryError{Kind: DirectoryErrorMissing, Path: "/missing/home", Message: "missing directory"}
	})
	code, err := ExecuteWithEvents(ctx, client, &Request{Arguments: []string{"unused"}}, strings.NewReader(""), io.Discard, io.Discard, nil)
	var directory *DirectoryError
	if code != 126 || !errors.As(err, &directory) || directory.Kind != DirectoryErrorMissing || directory.Path != "/missing/home" {
		t.Fatalf("directory error lost: %d %v", code, err)
	}
}
