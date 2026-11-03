package ipc

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Microsoft/go-winio"
)

// TestPipeListenerCloseWithClients tests that closing a named pipe listener
// returns while clients connect and disconnect, which can fail the listener's
// pending connection with ERROR_NO_DATA as it's closed (see pipeListener).
func TestPipeListenerCloseWithClients(t *testing.T) {
	for i := 0; i < 50; i++ {
		// Create a listener that accepts and closes connections.
		name := fmt.Sprintf(`\\.\pipe\mutagen-ipc-test-%s`, uuid.NewString())
		listener, err := ListenPipe(name, &winio.PipeConfig{})
		if err != nil {
			t.Fatal("unable to create listener:", err)
		}
		accepting := make(chan struct{})
		go func() {
			defer close(accepting)
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				connection.Close()
			}
		}()

		// Start clients that connect and disconnect immediately.
		stop := make(chan struct{})
		var clients sync.WaitGroup
		for j := 0; j < 4; j++ {
			clients.Add(1)
			go func() {
				defer clients.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					if connection, err := winio.DialPipeContext(ctx, name); err == nil {
						connection.Close()
					}
					cancel()
				}
			}()
		}

		// Close the listener while the clients are connecting.
		time.Sleep(5 * time.Millisecond)
		closed := make(chan error, 1)
		go func() {
			closed <- listener.Close()
		}()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatal("listener closure didn't return")
		}
		select {
		case <-accepting:
		case <-time.After(10 * time.Second):
			t.Fatal("accepting didn't stop after listener closure")
		}
		close(stop)
		clients.Wait()
	}
}
