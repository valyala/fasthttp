package fasthttp

import (
	"testing"
	"time"
)

func TestInitTimerDoesNotPanicWhenResetReportsActive(t *testing.T) {
	t.Parallel()

	// An active timer makes Reset return true deterministically. The runtime
	// race can return the same value for a stopped timer, and initTimer must
	// ignore it.
	active := time.NewTimer(time.Hour)
	defer active.Stop()

	got := initTimer(active, 10*time.Millisecond)
	select {
	case <-got.C:
	case <-time.After(time.Second):
		t.Fatal("re-armed timer didn't fire")
	}
}
