package fasthttp

import (
	"errors"
	"testing"
	"time"
)

type mockBalancingClient struct {
	err     error
	pending int
}

func (m *mockBalancingClient) DoDeadline(_ *Request, _ *Response, _ time.Time) error {
	return m.err
}

func (m *mockBalancingClient) PendingRequests() int {
	return m.pending
}

// TestLBClientRemoveAllClients reproduces the deadlock reported in #2270:
// once every client is removed, cc.cs is empty and get() used to panic on
// cs[0] while holding (and never releasing) the RLock, deadlocking every
// subsequent AddClient/RemoveClients call.
func TestLBClientRemoveAllClients(t *testing.T) {
	t.Parallel()

	lbc := &LBClient{Clients: []BalancingClient{&mockBalancingClient{}}}

	var req Request
	var resp Response
	deadline := func() time.Time { return time.Now().Add(time.Second) }

	// Trigger the lazy init and confirm the configured client serves the request.
	if err := lbc.DoDeadline(&req, &resp, deadline()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Remove every client so cc.cs becomes empty.
	if n := lbc.RemoveClients(func(BalancingClient) bool { return true }); n != 0 {
		t.Fatalf("unexpected client count after removal: %d", n)
	}

	// get() must not panic and must release the RLock, so the request fails
	// cleanly with ErrNoAvailableClients instead.
	if err := lbc.DoDeadline(&req, &resp, deadline()); !errors.Is(err, ErrNoAvailableClients) {
		t.Fatalf("unexpected error: %v. Expecting %v", err, ErrNoAvailableClients)
	}

	// With the bug the leaked RLock makes AddClient block forever.
	done := make(chan struct{})
	go func() {
		lbc.AddClient(&mockBalancingClient{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AddClient deadlocked after all clients were removed")
	}

	// The freshly added client must be usable again.
	if err := lbc.DoDeadline(&req, &resp, deadline()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLBClientRemoveClientsCallbackPanic ensures a panic in the user-supplied
// RemoveClients callback does not leak the write lock and deadlock later calls.
func TestLBClientRemoveClientsCallbackPanic(t *testing.T) {
	t.Parallel()

	lbc := &LBClient{Clients: []BalancingClient{
		&mockBalancingClient{},
		&mockBalancingClient{},
		&mockBalancingClient{},
	}}

	// Trigger the lazy init so cc.cs is populated and the callback runs.
	var req Request
	var resp Response
	if err := lbc.DoDeadline(&req, &resp, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Panic part way through so the old code would have already niled an
	// earlier cc.cs slot, leaving a hole that crashes later get() calls.
	calls := 0
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic from RemoveClients callback")
			}
		}()
		lbc.RemoveClients(func(BalancingClient) bool {
			calls++
			if calls == 2 {
				panic("boom")
			}
			return false
		})
	}()

	// If the lock leaked, AddClient would block forever.
	done := make(chan struct{})
	go func() {
		lbc.AddClient(&mockBalancingClient{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AddClient deadlocked after RemoveClients callback panicked")
	}

	// cc.cs must not be left with nil holes: a request still has to work
	// instead of panicking on a nil client.
	if err := lbc.DoDeadline(&req, &resp, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("unexpected error after panicking RemoveClients: %v", err)
	}
}
