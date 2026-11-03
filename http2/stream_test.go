package http2

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestRequestBodyCompactsTinyChunksWithoutLosingData(t *testing.T) {
	body := newRequestBody(nil, 0)
	want := make([]byte, 64<<10)
	for i := range want {
		want[i] = byte(i)
		if err := body.writeOwned([]byte{want[i]}, nil); err != nil {
			t.Fatalf("writeOwned(%d) error: %v", i, err)
		}
	}
	body.complete()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("compacted body differs: got %d bytes, want %d", len(got), len(want))
	}
}

func TestRequestBodyDiscardWaitsForTrailerCommit(t *testing.T) {
	// Finalization discards the body before releasing the request the commit
	// writes into.
	body := newRequestBody(nil, 0)
	committing := make(chan struct{})
	finish := make(chan struct{})
	body.setEOFCommit(func() error {
		close(committing)
		<-finish
		return nil
	})
	body.complete()
	readErr := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 1))
		readErr <- err
	}()
	<-committing
	discarded := make(chan struct{})
	go func() {
		body.discardWithError(errStreamClosed)
		close(discarded)
	}()
	select {
	case <-discarded:
		t.Fatal("discardWithError returned during the trailer commit")
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	<-discarded
	if err := <-readErr; !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want EOF", err)
	}
}

func TestStreamConnExpiredWriteDeadlineDoesNotQueueData(t *testing.T) {
	conn := &streamConn{stream: &serverStream{}}
	conn.init(nil)
	if err := conn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline() error: %v", err)
	}
	if n, err := conn.Write([]byte("data")); n != 0 || !isTimeout(err) {
		t.Fatalf("Write() = %d, %v; want 0, timeout", n, err)
	}
}

func newTestReadState(body *requestBody) *streamConnState {
	state := &streamConnState{}
	state.init(nil)
	state.wakeRead = body.wake
	return state
}

func TestReadUnderDeadlineExpiredKeepsTheStream(t *testing.T) {
	body := newRequestBody(nil, 0)
	state := newTestReadState(body)
	_ = state.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	buf := make([]byte, 1)
	n, err := state.readUnderDeadline(body, buf)
	if n != 0 || !isTimeout(err) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("readUnderDeadline() = %d, %v; want 0 and a deadline error", n, err)
	}
	if err := body.writeOwned([]byte("x"), nil); err != nil {
		t.Fatalf("writeOwned() error: %v", err)
	}
	if _, err := state.readUnderDeadline(body, buf); !isTimeout(err) {
		t.Fatalf("readUnderDeadline() past the deadline = %v, want timeout", err)
	}
	_ = state.SetReadDeadline(time.Time{})
	if n, err := state.readUnderDeadline(body, buf); n != 1 || err != nil || buf[0] != 'x' {
		t.Fatalf("readUnderDeadline() after clearing the deadline = %d, %v, %q; want the buffered byte", n, err, buf[:n])
	}
}

// A deadline set while a read is already blocked must still end it.
func TestReadUnderDeadlineSetWhileBlocked(t *testing.T) {
	body := newRequestBody(nil, 0)
	state := newTestReadState(body)
	done := make(chan error, 1)
	go func() {
		_, err := state.readUnderDeadline(body, make([]byte, 1))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = state.SetReadDeadline(time.Now())
	select {
	case err := <-done:
		if !isTimeout(err) {
			t.Fatalf("readUnderDeadline() error = %v, want timeout", err)
		}
	case <-time.After(2 * time.Second):
		body.complete()
		t.Fatal("a deadline set during the read didn't end it")
	}
}

func TestReadUnderDeadlinePassedDeadline(t *testing.T) {
	body := newRequestBody(nil, 0)
	if err := body.writeOwned([]byte("x"), nil); err != nil {
		t.Fatalf("writeOwned() error: %v", err)
	}
	state := newTestReadState(body)
	_ = state.SetReadDeadline(time.Now().Add(-time.Second))
	if _, err := state.readUnderDeadline(body, make([]byte, 1)); !isTimeout(err) {
		t.Fatalf("readUnderDeadline() error = %v, want timeout", err)
	}
	body.mu.Lock()
	buffered := body.buffered
	body.mu.Unlock()
	if buffered != 1 {
		t.Fatal("a read past its deadline consumed data")
	}
}

// Data arriving as the deadline fires is either read or left for the next read,
// never lost.
func TestReadUnderDeadlineRace(t *testing.T) {
	for range 500 {
		body := newRequestBody(nil, 0)
		state := newTestReadState(body)
		deadline := time.Now().Add(200 * time.Microsecond)
		_ = state.SetReadDeadline(deadline)
		go func() {
			time.Sleep(time.Until(deadline))
			_ = body.writeOwned([]byte("x"), nil)
		}()
		buf := make([]byte, 1)
		n, err := state.readUnderDeadline(body, buf)
		if err != nil {
			if !isTimeout(err) {
				t.Fatalf("readUnderDeadline() error = %v, want timeout", err)
			}
			_ = state.SetReadDeadline(time.Time{})
			n, err = state.readUnderDeadline(body, buf)
		}
		if n != 1 || err != nil || buf[0] != 'x' {
			t.Fatalf("readUnderDeadline() = %d, %v; want the byte", n, err)
		}
	}
}
