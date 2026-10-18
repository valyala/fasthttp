package fasthttp

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// Use for waits outside synctest bubbles, notably real TCP and codec workers.
// Pure bubble-local channel/Cond deadlocks are diagnosed by synctest itself.
func streamAwait[T any](t testing.TB, done <-chan T, event string) T {
	t.Helper()
	select {
	case value := <-done:
		return value
	case <-time.After(testTimeout(15 * time.Second)):
		t.Fatalf("timeout waiting for %s", event)
		var zero T
		return zero
	}
}

func streamClose(t testing.TB, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Error(err)
	}
}
