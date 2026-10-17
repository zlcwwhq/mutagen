package synchronization

import (
	"context"
	"testing"
	"time"
)

func TestCommandRelayFlush(t *testing.T) {
	// Flushes complete once the synchronization loop responds.
	relay := &commandRelay{flushRequests: make(chan chan error, 1)}
	go func() { (<-relay.flushRequests) <- nil }()
	if err := relay.flush(context.Background()); err != nil {
		t.Fatal("flush failed:", err)
	}

	// Flushes fail once synchronization stops, rather than waiting for a
	// response that won't arrive, both after their request is queued and while
	// waiting to queue it (behind the request left by the first case).
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if err := relay.flush(ctx); err != errFlushTerminated {
		t.Fatal("queued flush not terminated:", err)
	} else if err := relay.flush(ctx); err != errFlushTerminated {
		t.Fatal("unqueued flush not terminated:", err)
	}
}
