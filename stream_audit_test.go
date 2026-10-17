package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// This comparison probe records framing/flush granularity without requiring
// either implementation to choose a particular batching policy.
func TestStreamAuditBurstFlushCount(t *testing.T) {
	producerDone := make(chan error, 1)
	r := NewBoundedStreamReader(func(w *bufio.Writer) {
		for range 4 {
			if _, err := w.Write(bytes.Repeat([]byte("a"), 128)); err != nil {
				producerDone <- err
				return
			}
			if err := w.Flush(); err != nil {
				producerDone <- err
				return
			}
		}
		producerDone <- nil
	})
	if err := <-producerDone; err != nil {
		t.Fatal(err)
	}
	var resp Response
	resp.SetBodyStream(r, -1)
	var dst streamAuditCountingWriter
	w := bufio.NewWriterSize(&dst, 64*1024)
	if err := resp.Write(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	wire := dst.String()
	_, body, found := strings.Cut(wire, "\r\n\r\n")
	if !found {
		t.Fatal("missing headers")
	}
	var chunks []int
	for {
		end := strings.Index(body, "\r\n")
		if end < 0 {
			t.Fatal("missing chunk size")
		}
		n, err := strconv.ParseInt(body[:end], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		body = body[end+2:]
		if n == 0 {
			break
		}
		chunks = append(chunks, int(n))
		if int(n)+2 > len(body) || body[n:n+2] != "\r\n" {
			t.Fatal("invalid chunk framing")
		}
		body = body[n+2:]
	}
	parsed, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wire)), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(parsed.Body)
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("a"), 512)) {
		t.Fatalf("body %d bytes, error %v", len(got), err)
	}
	if err := parsed.Body.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("queued input Flush calls=4; chunk sizes=%v; downstream Write calls=%d", chunks, dst.writes)
}

type streamAuditCountingWriter struct {
	bytes.Buffer

	writes int
}

func (w *streamAuditCountingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func TestStreamAuditBurstOrdinaryRead(t *testing.T) {
	producerDone := make(chan error, 1)
	r := NewBoundedStreamReader(func(w *bufio.Writer) {
		for range 4 {
			if _, err := w.Write(bytes.Repeat([]byte("a"), 128)); err != nil {
				producerDone <- err
				return
			}
			if err := w.Flush(); err != nil {
				producerDone <- err
				return
			}
		}
		producerDone <- nil
	})
	if err := <-producerDone; err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 4096)
	n, err := r.Read(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(append(first[:n], tail...), bytes.Repeat([]byte("a"), 512)) {
		t.Fatalf("first read = %d; tail = %d; error = %v", n, len(tail), err)
	}
	t.Logf("queued input Flush calls=4; first Read into 4096-byte buffer=%d; tail=%d", n, len(tail))
}

func TestStreamAuditAcceptedInputReuseCloseWhileDestinationBlocked(t *testing.T) {
	input := make([]byte, 100_000)
	for i := range input {
		input[i] = byte(i * 31)
	}
	want := bytes.Clone(input)
	producerDone := make(chan error, 1)
	producerFinish := make(chan struct{})
	r := NewBoundedStreamReader(func(w *bufio.Writer) {
		if n, err := w.Write(input); n != len(input) || err != nil {
			producerDone <- fmt.Errorf("write = %d, %v", n, err)
			return
		}
		producerDone <- w.Flush()
		<-producerFinish
	})
	if err := <-producerDone; err != nil {
		t.Fatal(err)
	}
	clear(input)
	w := streamAuditGateWriter{entered: make(chan struct{}), release: make(chan struct{})}
	type result struct {
		n   int64
		err error
	}
	consumerDone := make(chan result, 1)
	go func() {
		n, err := io.Copy(&w, r)
		consumerDone <- result{n, err}
	}()
	<-w.entered
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	close(producerFinish)
	close(w.release)
	got := <-consumerDone
	if got.n != int64(len(want)) || got.err != nil || !bytes.Equal(w.dst.Bytes(), want) {
		t.Fatalf("copy = %d, %v; output bytes = %d", got.n, got.err, w.dst.Len())
	}
}

type streamAuditGateWriter struct {
	dst     bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *streamAuditGateWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return w.dst.Write(p)
}

func TestStreamAuditWriterBoundariesAndResume(t *testing.T) {
	sentinel := errors.New("audit destination failed")
	for _, tc := range []struct {
		name   string
		failAt int
		n      int
		full   bool
		err    error
	}{
		{"zeroNil", 1, 0, false, nil},
		{"zeroError", 1, 0, false, sentinel},
		{"fullError", 1, 0, true, sentinel},
		{"multiBlockPartialError", 2, 17, false, sentinel},
		{"multiBlockFullError", 2, 0, true, sentinel},
	} {
		for _, resume := range []string{"Read", "WriteTo"} {
			t.Run(tc.name+"/"+resume, func(t *testing.T) {
				source := make([]byte, 100_000)
				for i := range source {
					source[i] = byte(i * 31)
				}
				r := streamAuditPreparedReader(t, source)
				defer r.Close()
				wt, ok := r.(io.WriterTo)
				if !ok {
					t.Skip("baseline reader has no WriterTo; candidate-specific ownership check")
				}
				w := streamAuditBoundaryWriter{failAt: tc.failAt, n: tc.n, full: tc.full, err: tc.err}
				n, err := wt.WriteTo(&w)
				wantErr := tc.err
				if wantErr == nil {
					wantErr = io.ErrShortWrite
				}
				if !errors.Is(err, wantErr) || n != int64(w.dst.Len()) || w.calls != tc.failAt {
					t.Fatalf("WriteTo = %d, %v; accepted = %d; calls = %d", n, err, w.dst.Len(), w.calls)
				}
				if !bytes.Equal(w.dst.Bytes(), source[:n]) {
					t.Fatal("accepted prefix differs from source")
				}
				var tail []byte
				if resume == "Read" {
					tail, err = io.ReadAll(r)
				} else {
					var dst bytes.Buffer
					var copied int64
					copied, err = wt.WriteTo(&dst)
					tail = dst.Bytes()
					if copied != int64(len(tail)) {
						t.Fatalf("resumed WriteTo count = %d, bytes = %d", copied, len(tail))
					}
				}
				if err != nil || !bytes.Equal(tail, source[n:]) {
					t.Fatalf("resumed %s: %d bytes, %v; want %d bytes", resume, len(tail), err, len(source)-int(n))
				}
			})
		}
	}
}

func streamAuditPreparedReader(t *testing.T, source []byte) io.ReadCloser {
	t.Helper()
	done := make(chan error, 1)
	r := NewBoundedStreamReader(func(w *bufio.Writer) {
		n, err := w.Write(source)
		if err == nil && n != len(source) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = w.Flush()
		}
		done <- err
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return r
}

type streamAuditBoundaryWriter struct {
	dst    bytes.Buffer
	failAt int
	n      int
	full   bool
	err    error
	calls  int
}

func (w *streamAuditBoundaryWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls != w.failAt {
		return w.dst.Write(p)
	}
	n := min(w.n, len(p))
	if w.full {
		n = len(p)
	}
	w.dst.Write(p[:n])
	return n, w.err
}

func TestStreamAuditEmptyReadCloseWakeup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finishProducer := make(chan struct{})
		r := NewBoundedStreamReader(func(w *bufio.Writer) { <-finishProducer })
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() {
			n, err := r.Read(make([]byte, 1))
			done <- result{n, err}
		}()
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("empty live reader did not block: %d, %v", got.n, got.err)
		default:
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.n != 0 || !errors.Is(got.err, io.EOF) {
			t.Fatalf("empty closed reader = %d, %v", got.n, got.err)
		}
		close(finishProducer)
	})
}

func TestStreamAuditReadyReadDoesNotWaitForFutureData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready, finish := make(chan error, 1), make(chan struct{})
		r := streamAuditGatedBurst(ready, finish)
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		first := make([]byte, 4096)
		n, err := r.Read(first)
		if n != 512 || err != nil || !bytes.Equal(first[:n], bytes.Repeat([]byte("v"), 512)) {
			t.Fatalf("ready Read = %d, %v", n, err)
		}
		close(finish)
		tail, err := io.ReadAll(r)
		if err != nil || len(tail) != 0 {
			t.Fatalf("tail = %d, %v", len(tail), err)
		}
		r.Close()
	})
}

func TestStreamAuditReadyHTTPBurstFlushBeforeFutureData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready, finish := make(chan error, 1), make(chan struct{})
		r := streamAuditGatedBurst(ready, finish)
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		var resp Response
		resp.Header.noDefaultDate = true
		resp.SetBodyStream(r, -1)
		writes := make(chan []byte, 16)
		w := bufio.NewWriterSize(streamAuditNotifyWriter{writes}, 64*1024)
		done := make(chan error, 1)
		go func() {
			err := resp.Write(w)
			if err == nil {
				err = w.Flush()
			}
			close(writes)
			done <- err
		}()
		first := <-writes
		// All four messages must reach the destination while the producer is
		// still waiting for permission to generate any future data or EOF.
		if bytes.Count(first, bytes.Repeat([]byte("v"), 128)) != 4 {
			t.Fatalf("first flush has fewer than four ready messages: %q", first)
		}
		close(finish)
		wire := bytes.Clone(first)
		for p := range writes {
			wire = append(wire, p...)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		parsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(wire)), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(parsed.Body)
		if err != nil || !bytes.Equal(body, bytes.Repeat([]byte("v"), 512)) {
			t.Fatalf("HTTP body = %d bytes, %v", len(body), err)
		}
		parsed.Body.Close()
	})
}

func streamAuditGatedBurst(ready chan<- error, finish <-chan struct{}) io.ReadCloser {
	return NewBoundedStreamReader(func(w *bufio.Writer) {
		for range 4 {
			if _, err := w.Write(bytes.Repeat([]byte("v"), 128)); err != nil {
				ready <- err
				return
			}
			if err := w.Flush(); err != nil {
				ready <- err
				return
			}
		}
		ready <- nil
		<-finish
	})
}

type streamAuditNotifyWriter struct{ writes chan<- []byte }

func (w streamAuditNotifyWriter) Write(p []byte) (int, error) {
	w.writes <- bytes.Clone(p)
	return len(p), nil
}
