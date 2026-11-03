package ipc

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// pipeCloseRetryInterval is the interval at which pipeListener.Close repeats
// its request to close the underlying listener.
const pipeCloseRetryInterval = 100 * time.Millisecond

// pipeListener is a named pipe listener that works around a race in the
// closure of go-winio's listeners: if a client connects and disconnects while
// a listener is closed, which fails the pending connection with ERROR_NO_DATA,
// then the listener consumes the close request and waits for another client
// instead of closing, and Close blocks forever. This is fixed by go-winio
// v0.6.3, which requires Go 1.26. Each call to go-winio's Close sends a close
// request of its own, and they all return once the listener closes.
type pipeListener struct {
	// Listener is the underlying named pipe listener.
	net.Listener
}

// Close implements net.Listener.Close. It repeats the close request until the
// underlying listener closes.
func (l *pipeListener) Close() error {
	closed := make(chan error, 1)
	go func() {
		closed <- l.Listener.Close()
	}()
	retry := time.NewTicker(pipeCloseRetryInterval)
	defer retry.Stop()
	for {
		select {
		case err := <-closed:
			return err
		case <-retry.C:
			go l.Listener.Close()
		}
	}
}

// ListenPipe creates a named pipe listener as winio.ListenPipe does, but whose
// Close can't block forever (see pipeListener). Named pipe listeners should be
// created with it rather than with winio.ListenPipe.
func ListenPipe(name string, configuration *winio.PipeConfig) (net.Listener, error) {
	listener, err := winio.ListenPipe(name, configuration)
	if err != nil {
		return nil, err
	}
	return &pipeListener{listener}, nil
}
