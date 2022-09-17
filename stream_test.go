package fasthttp

import (
	"bufio"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestNewStreamReader(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{})
	r := NewStreamReader(func(w *bufio.Writer) {
		fmt.Fprintf(w, "Hello, world\n")
		fmt.Fprintf(w, "Line #2\n")
		close(ch)
	})

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedData := "Hello, world\nLine #2\n"
	if string(data) != expectedData {
		t.Fatalf("unexpected data %q. Expecting %q", data, expectedData)
	}

	if err = r.Close(); err != nil {
		t.Fatalf("unexpected error")
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

func TestStreamReaderClose(t *testing.T) {
	t.Parallel()

	firstLine := "the first line must pass"
	ch := make(chan error, 1)
	r := NewStreamReader(func(w *bufio.Writer) {
		fmt.Fprintf(w, "%s", firstLine)
		if err := w.Flush(); err != nil {
			ch <- fmt.Errorf("unexpected error on first flush: %w", err)
			return
		}

		data := createFixedBody(4000)
		for i := 0; i < 100; i++ {
			w.Write(data) //nolint:errcheck
		}
		if err := w.Flush(); err == nil {
			ch <- fmt.Errorf("expecting error on the second flush")
		}
		ch <- nil
	})

	buf := make([]byte, len(firstLine))
	n, err := io.ReadFull(r, buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("unexpected number of bytes read: %d. Expecting %d", n, len(buf))
	}
	if string(buf) != firstLine {
		t.Fatalf("unexpected result: %q. Expecting %q", buf, firstLine)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("error returned from stream reader: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout when waiting for stream reader")
	}

	// read trailing data
	go func() {
		if _, err := io.ReadAll(r); err != nil {
			ch <- fmt.Errorf("unexpected error when reading trailing data: %w", err)
			return
		}
		ch <- nil
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("error returned when reading tail data: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout when reading tail data")
	}
}
