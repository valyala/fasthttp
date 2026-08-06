package http2

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestReadWithStreamDeadlineExpired(t *testing.T) {
	var canceled atomic.Bool
	blocked := make(chan struct{})
	reader := readerFunc(func([]byte) (int, error) {
		<-blocked
		return 0, errors.New("stream canceled")
	})
	n, err := readWithStreamDeadline(reader, make([]byte, 1), time.Now().Add(10*time.Millisecond), func() {
		canceled.Store(true)
		close(blocked)
	})
	if n != 0 || !isTimeout(err) {
		t.Fatalf("readWithStreamDeadline() = %d, %v; want 0, timeout", n, err)
	}
	if !canceled.Load() {
		t.Fatal("expired deadline didn't cancel the stream")
	}
}

func TestReadWithStreamDeadlinePassedDeadline(t *testing.T) {
	var canceled, attempted atomic.Bool
	reader := readerFunc(func([]byte) (int, error) {
		attempted.Store(true)
		return 0, io.EOF
	})
	if _, err := readWithStreamDeadline(reader, make([]byte, 1), time.Now().Add(-time.Second), func() {
		canceled.Store(true)
	}); !isTimeout(err) {
		t.Fatalf("readWithStreamDeadline() error = %v, want timeout", err)
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
func TestReadWithStreamDeadlineRace(t *testing.T) {
	for range 500 {
		var canceled atomic.Bool
		deadline := time.Now().Add(200 * time.Microsecond)
		reader := readerFunc(func(p []byte) (int, error) {
			time.Sleep(time.Until(deadline))
			p[0] = 'x'
			return 1, nil
		})
		n, err := readWithStreamDeadline(reader, make([]byte, 1), deadline, func() {
			canceled.Store(true)
		})
		if err != nil {
			if !isTimeout(err) {
				t.Fatalf("readWithStreamDeadline() error = %v, want timeout", err)
			}
			continue
		}
		if n != 1 {
			t.Fatalf("readWithStreamDeadline() = %d, want 1", n)
		}
		time.Sleep(200 * time.Microsecond)
		if canceled.Load() {
			t.Fatal("successful read left the stream cancelled")
		}
	}
}
