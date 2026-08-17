package fasthttp

import (
	"testing"
	"time"
)

func TestInitTimerToleratesActiveTimer(t *testing.T) {
	t.Parallel()

	active := time.NewTimer(time.Hour)
	defer active.Stop()

	got := initTimer(active, 10*time.Millisecond)
	select {
	case <-got.C:
	case <-time.After(time.Second):
		t.Fatal("re-armed timer didn't fire")
	}
}
