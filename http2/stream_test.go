package http2

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestBodyCompactsTinyChunksWithoutLosingData(t *testing.T) {
	body := newRequestBody(nil)
	want := make([]byte, 64<<10)
	for i := range want {
		want[i] = byte(i)
		if err := body.writeOwned([]byte{want[i]}, nil); err != nil {
			t.Fatalf("writeOwned(%d) error: %v", i, err)
		}
	}
	body.closeWithError(nil)
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("compacted body differs: got %d bytes, want %d", len(got), len(want))
	}
}

func TestStreamConnExpiredWriteDeadlineDoesNotQueueData(t *testing.T) {
	conn := &streamConn{stream: &serverStream{}}
	if err := conn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline() error: %v", err)
	}
	if n, err := conn.Write([]byte("data")); n != 0 || !isTimeout(err) {
		t.Fatalf("Write() = %d, %v; want 0, timeout", n, err)
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestReadUnderDeadlineExpired(t *testing.T) {
	var canceled atomic.Bool
	blocked := make(chan struct{})
	reader := readerFunc(func([]byte) (int, error) {
		<-blocked
		return 0, errors.New("stream canceled")
	})
	state := streamConnState{cancel: func() {
		canceled.Store(true)
		close(blocked)
	}}
	_ = state.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	n, err := state.readUnderDeadline(reader, make([]byte, 1))
	if n != 0 || !isTimeout(err) {
		t.Fatalf("readUnderDeadline() = %d, %v; want 0, timeout", n, err)
	}
	if !canceled.Load() {
		t.Fatal("expired deadline didn't cancel the stream")
	}
}

// A deadline set while a read is already blocked must still end it.
func TestReadUnderDeadlineSetWhileBlocked(t *testing.T) {
	blocked := make(chan struct{})
	reader := readerFunc(func([]byte) (int, error) {
		<-blocked
		return 0, errors.New("stream canceled")
	})
	state := streamConnState{cancel: func() { close(blocked) }}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = state.SetReadDeadline(time.Now())
	}()
	done := make(chan error, 1)
	go func() {
		_, err := state.readUnderDeadline(reader, make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if !isTimeout(err) {
			t.Fatalf("readUnderDeadline() error = %v, want timeout", err)
		}
	case <-time.After(2 * time.Second):
		close(blocked)
		t.Fatal("a deadline set during the read didn't end it")
	}
}

func TestReadUnderDeadlinePassedDeadline(t *testing.T) {
	var canceled, attempted atomic.Bool
	reader := readerFunc(func([]byte) (int, error) {
		attempted.Store(true)
		return 0, io.EOF
	})
	state := streamConnState{cancel: func() { canceled.Store(true) }}
	_ = state.SetReadDeadline(time.Now().Add(-time.Second))
	if _, err := state.readUnderDeadline(reader, make([]byte, 1)); !isTimeout(err) {
		t.Fatalf("readUnderDeadline() error = %v, want timeout", err)
	}
	if attempted.Load() {
		t.Fatal("read ran after the deadline had passed")
	}
	if canceled.Load() {
		t.Fatal("a deadline that had already passed cancelled the stream")
	}
}

// A read that completes as its deadline fires must not report success on a
// stream the deadline callback then cancels.
func TestReadUnderDeadlineRace(t *testing.T) {
	for range 500 {
		var canceled atomic.Bool
		deadline := time.Now().Add(200 * time.Microsecond)
		reader := readerFunc(func(p []byte) (int, error) {
			time.Sleep(time.Until(deadline))
			p[0] = 'x'
			return 1, nil
		})
		state := streamConnState{cancel: func() { canceled.Store(true) }}
		_ = state.SetReadDeadline(deadline)
		n, err := state.readUnderDeadline(reader, make([]byte, 1))
		if err != nil {
			if !isTimeout(err) {
				t.Fatalf("readUnderDeadline() error = %v, want timeout", err)
			}
			continue
		}
		if n != 1 {
			t.Fatalf("readUnderDeadline() = %d, want 1", n)
		}
		time.Sleep(200 * time.Microsecond)
		if canceled.Load() {
			t.Fatal("successful read left the stream cancelled")
		}
	}
}
