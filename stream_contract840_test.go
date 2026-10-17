package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"testing"
	"testing/synctest"
)

// The explicit API must not accept an entire 10MB write without a consumer.
// The historical NewStreamReader baseline has no byte-bound guarantee.
func TestStream840BackpressureBeforePayloadAcceptance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		input := bytes.Repeat([]byte("x"), 10_000_000)
		r := NewBoundedStreamReader(func(w *bufio.Writer) {
			n, err := w.Write(input)
			done <- result{n, err}
		})
		synctest.Wait()
		var got result
		premature := false
		select {
		case got = <-done:
			premature = true
			t.Error("10MB Write accepted without a consumer; byte backpressure is absent")
		default:
		}
		r.Close()
		if !premature {
			got = <-done
			if got.err == nil || got.n <= 0 || got.n > 160*1024 {
				t.Errorf("closed blocked Write=%d,%v; accepted prefix must fit the internal payload budget", got.n, got.err)
			}
		}
		clear(input) // Write has returned; the accepted prefix must be owned.
		tail, err := io.ReadAll(r)
		if err != nil || len(tail) != got.n || bytes.Count(tail, []byte("x")) != got.n {
			t.Fatalf("accepted tail after Close/input reuse=%d,%v; want %d x bytes", len(tail), err, got.n)
		}
		r.Close()
	})
}

// Small nonempty Flush already propagates in the baseline. Keep this green
// while adding bounded buffering; this is not an empty-Flush barrier contract.
func TestStream840NonemptyFlushReadyBeforeFutureData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready, finish := make(chan error, 1), make(chan struct{})
		r := NewBoundedStreamReader(func(w *bufio.Writer) {
			if _, err := w.WriteString("event"); err != nil {
				ready <- err
				return
			}
			ready <- w.Flush()
			<-finish
		})
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 5)
		if _, err := io.ReadFull(r, got); err != nil || string(got) != "event" {
			t.Fatal(string(got), err)
		}
		close(finish)
		if tail, err := io.ReadAll(r); err != nil || len(tail) != 0 {
			t.Fatal(len(tail), err)
		}
		r.Close()
	})
}
